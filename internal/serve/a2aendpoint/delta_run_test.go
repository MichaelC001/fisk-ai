//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2aendpoint

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/a2a"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/serve"
)

// deltaTurn is one model call the fake answers: the fragments it reports while the turn is
// written, and the assembled turn it returns.
type deltaTurn struct {
	deltas []llm.Delta
	resp   *llm.Response
}

// streamingScript is an llm.StreamingProvider answering a queue of turns, one per call.
//
// CallStream reports a turn's fragments on the goroutine that called it, in order and
// before it returns, and then returns the same *llm.Response Call returns for that turn,
// which is what llm.StreamingProvider requires of a backend.
//
// It lives here rather than in agenttest because every streaming fake so far wants a
// different shape: the two in the agent package pace their fragments and count which call
// path each run took, and this one scripts a turn at a time so a conversation of several
// calls can be driven. agenttest is a surface an embedder builds against, and no caller
// outside this file is asking for this one yet.
type streamingScript struct {
	mu       sync.Mutex
	turns    []deltaTurn
	idx      int
	called   int
	streamed int
}

func newStreamingScript(turns ...deltaTurn) *streamingScript {
	return &streamingScript{turns: turns}
}

func (p *streamingScript) Capabilities() llm.Caps { return llm.Caps{Provider: "anthropic"} }

// next takes the turn this call answers and records which path it came in on. A call past
// the end of the script returns an error, which the run reports as a failed turn.
func (p *streamingScript) next(streamed bool) (deltaTurn, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.idx >= len(p.turns) {
		return deltaTurn{}, fmt.Errorf("the script holds %d turns and this is call %d", len(p.turns), p.idx+1)
	}

	turn := p.turns[p.idx]
	p.idx++

	if streamed {
		p.streamed++
	} else {
		p.called++
	}

	return turn, nil
}

func (p *streamingScript) Call(context.Context, llm.Request) (*llm.Response, error) {
	turn, err := p.next(false)
	if err != nil {
		return nil, err
	}

	return turn.resp, nil
}

func (p *streamingScript) CallStream(_ context.Context, _ llm.Request, fn func(llm.Delta)) (*llm.Response, error) {
	turn, err := p.next(true)
	if err != nil {
		return nil, err
	}

	for _, d := range turn.deltas {
		fn(d)
	}

	return turn.resp, nil
}

// Counts is how many calls took the ordinary path and how many streamed.
func (p *streamingScript) Counts() (int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.called, p.streamed
}

// lossyTransport loses one message of a reply set between the worker and the caller.
//
// The message is published and numbered by the worker and never reaches the caller, so the
// sequence carries a hole and a2a.TaskStream.Gaps reports it. A sink told not to send the
// fragment would leave no hole, and the rule exists for the hole.
type lossyTransport struct {
	a2a.StreamingTransport

	drops a2a.BlockType
}

func (t *lossyTransport) Stream(ctx context.Context, agent string, op a2a.RouteHint, body []byte) (a2a.Reader, error) {
	reader, err := t.StreamingTransport.Stream(ctx, agent, op, body)
	if err != nil {
		return nil, err
	}

	return &lossyReader{Reader: reader, drops: t.drops}, nil
}

// lossyReader swallows the first message carrying a block of the kind it was built for.
type lossyReader struct {
	a2a.Reader

	drops   a2a.BlockType
	dropped bool
}

func (r *lossyReader) Next(ctx context.Context) ([]byte, error) {
	for {
		body, err := r.Reader.Next(ctx)
		if err != nil || r.dropped || !carriesBlock(body, r.drops) {
			return body, err
		}

		r.dropped = true
	}
}

// carriesBlock reports whether a message of a reply set is an event carrying a block of
// the given kind.
func carriesBlock(body []byte, kind a2a.BlockType) bool {
	msg, err := a2a.Decode(body)
	if err != nil {
		return false
	}

	event, ok := msg.(*a2a.Event)
	if !ok {
		return false
	}

	return event.Block.Type() == kind
}

// dropFirst builds the transport wrapper that loses the first message carrying a block of
// the given kind.
func dropFirst(kind a2a.BlockType) func(a2a.StreamingTransport) a2a.StreamingTransport {
	return func(t a2a.StreamingTransport) a2a.StreamingTransport {
		return &lossyTransport{StreamingTransport: t, drops: kind}
	}
}

// wireBlock is one block of a reply set with the model call it belongs to.
//
// The call is counted the way a receiver counts it: a call's whole blocks arrive before
// the status block that ends it, so every block up to the status block carrying iteration
// n belongs to call n.
type wireBlock struct {
	call  int
	block a2a.Block
}

// blockKey is one content block of one model call, which is what a fragment and the whole
// block that ends it share.
type blockKey struct {
	call  int
	index int
}

// runWorker starts a worker whose model is the given provider, hands a client and a
// bounded context to fn, and stops the worker before returning, so a spec can run two
// workers of one identity one after the other.
//
// wrap wraps the transport the client is built on, for a spec that loses a message on the
// way to the caller. Nil leaves the binding as it was built.
func runWorker(model llm.Provider, wrap func(a2a.StreamingTransport) a2a.StreamingTransport, fn func(context.Context, *a2a.Client)) {
	GinkgoHelper()

	app := agenttest.NewFakeApp(GinkgoTB(), streamsApp())

	cfg := parseConfig(fmt.Sprintf(`
identity: agent1
application_path: %s
system_prompt: answer about streams
nats_context: ctx
llm:
  model: claude-sonnet-4-6
expose:
  agent:
    a2a:
      prompts:
        workers: 1
`, app.Path))

	built, err := NewFromConfig(cfg, ConfigOptions{Conns: provider, Logger: quietLogger()})
	Expect(err).ToNot(HaveOccurred())

	srv, err := serve.New(serve.Options{
		Channels:   []serve.Channel{channelOf(built)},
		Config:     cfg,
		ConfigFile: "agent.yaml",
		StoreDir:   GinkgoT().TempDir(),
		Provider:   model,
		Logger:     quietLogger(),
	})
	Expect(err).ToNot(HaveOccurred())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	served := make(chan error, 1)
	go func() {
		defer GinkgoRecover()

		served <- srv.Serve(ctx)
	}()

	// A failed assertion panics out of fn, so the stop is deferred rather than written
	// after it. A worker left serving agent1 answers the next spec's request, which turns
	// one failure into a run of unrelated timeouts. It runs before the cancel above,
	// deferred later.
	defer func() {
		Expect(srv.Stop()).To(Succeed())
		Eventually(served, 10*time.Second).Should(Receive(Succeed()))
	}()

	transport, err := a2a.NewTransport("nats", a2a.TransportConfig{Resources: provider, Identity: "caller1", Timeout: 5 * time.Second})
	Expect(err).ToNot(HaveOccurred())
	DeferCleanup(transport.Close)

	streaming, ok := transport.(a2a.StreamingTransport)
	Expect(ok).To(BeTrue())

	if wrap != nil {
		streaming = wrap(streaming)
	}

	client, err := a2a.NewClient(streaming, "caller1")
	Expect(err).ToNot(HaveOccurred())

	fn(ctx, client)
}

// asks builds a prompt request, asking for the turn as it is written when deltas is set.
func asks(prompt string, deltas bool) *a2a.Request {
	req := a2a.NewRequest(prompt)
	if deltas {
		yes := true
		req.Deltas = &yes
	}

	return req
}

// readSet reads a reply set to its terminal message and returns the blocks it carried and
// the result it ended on.
func readSet(ctx context.Context, stream *a2a.TaskStream) ([]wireBlock, *a2a.Result) {
	GinkgoHelper()

	var blocks []wireBlock
	call := 1

	for {
		msg, err := stream.Next(ctx)
		Expect(err).ToNot(HaveOccurred())

		switch m := msg.(type) {
		case *a2a.Ack:
			Expect(m.Accepted).To(BeTrue())

		case *a2a.Event:
			blocks = append(blocks, wireBlock{call: call, block: m.Block})

			status, ok := m.Block.Content().(a2a.StatusBlock)
			if ok && status.Phase == "" {
				call = status.Iteration + 1
			}

		case *a2a.ErrorMessage:
			Fail(fmt.Sprintf("the run ended in error: %s (%s)", m.Err, m.Code))

		case *a2a.Result:
			return blocks, m
		}
	}
}

// contentsOf is the content of every block of a set, in arrival order, for comparing one
// run's blocks with another's.
func contentsOf(blocks []wireBlock) []a2a.BlockContent {
	out := make([]a2a.BlockContent, len(blocks))
	for i, b := range blocks {
		out[i] = b.block.Content()
	}

	return out
}

// positions is where a set carried the fragments of each content block and where it
// carried the whole block, so a spec can say which arrived first.
func positions(blocks []wireBlock) (map[blockKey][]int, map[blockKey]int) {
	fragments := map[blockKey][]int{}
	wholes := map[blockKey]int{}

	for at, b := range blocks {
		switch v := b.block.Content().(type) {
		case a2a.TextDeltaBlock:
			key := blockKey{call: v.Iteration, index: v.Index}
			fragments[key] = append(fragments[key], at)
		case a2a.ThinkingDeltaBlock:
			key := blockKey{call: v.Iteration, index: v.Index}
			fragments[key] = append(fragments[key], at)
		case a2a.TextBlock:
			wholes[blockKey{call: b.call, index: v.Index}] = at
		case a2a.ThinkingBlock:
			wholes[blockKey{call: b.call, index: v.Index}] = at
		}
	}

	return fragments, wholes
}

// fragmentText is the text of every fragment of one content block, joined in arrival
// order, and how many of them closed the block.
func fragmentText(blocks []wireBlock, key blockKey) (string, int) {
	var (
		text   strings.Builder
		finals int
	)

	for _, b := range blocks {
		switch v := b.block.Content().(type) {
		case a2a.TextDeltaBlock:
			if v.Iteration != key.call || v.Index != key.index {
				continue
			}
			text.WriteString(v.Text)
			if v.Final {
				finals++
			}
		case a2a.ThinkingDeltaBlock:
			if v.Iteration != key.call || v.Index != key.index {
				continue
			}
			text.WriteString(v.Text)
			if v.Final {
				finals++
			}
		}
	}

	return text.String(), finals
}

// wholeBlocks is the whole text and thinking blocks of a set, keyed by the call and index
// they were produced under.
func wholeBlocks(blocks []wireBlock) map[blockKey]a2a.BlockContent {
	out := map[blockKey]a2a.BlockContent{}

	for _, b := range blocks {
		switch v := b.block.Content().(type) {
		case a2a.TextBlock:
			out[blockKey{call: b.call, index: v.Index}] = v
		case a2a.ThinkingBlock:
			out[blockKey{call: b.call, index: v.Index}] = v
		}
	}

	return out
}

// assembleCalls drives llm.DeltaAssembler over a reply set the way a receiver rendering a
// live turn does: fragments as they arrive, whole blocks as they close, and a reset on the
// status block that ends a call. It returns what each call assembled to, keyed by call.
func assembleCalls(blocks []wireBlock) map[int][]llm.AssembledBlock {
	var assembler llm.DeltaAssembler

	out := map[int][]llm.AssembledBlock{}
	last := 1

	for _, b := range blocks {
		last = b.call

		switch v := b.block.Content().(type) {
		case a2a.TextDeltaBlock:
			assembler.AddDelta(llm.Delta{Kind: llm.DeltaText, Index: v.Index, Text: v.Text, Final: v.Final})

		case a2a.ThinkingDeltaBlock:
			assembler.AddDelta(llm.Delta{Kind: llm.DeltaThinking, Index: v.Index, Text: v.Text, Final: v.Final})

		case a2a.TextBlock:
			assembler.AddBlock(llm.WholeBlock{Kind: llm.DeltaText, Index: v.Index, Text: v.Text, Trimmed: v.Trimmed})

		case a2a.ThinkingBlock:
			assembler.AddBlock(llm.WholeBlock{Kind: llm.DeltaThinking, Index: v.Index, Text: v.Text, Trimmed: v.Trimmed})

		case a2a.StatusBlock:
			if v.Phase != "" {
				continue
			}

			out[b.call] = assembler.Blocks()
			assembler.Reset()
		}
	}

	held := assembler.Blocks()
	if len(held) > 0 {
		out[last] = held
	}

	return out
}

// The whole chain in one place: a backend that reports fragments, the runner's branch, the
// events half, the recorder that forwards it, the sink's coalescer, the wire, and a
// receiver reconciling what arrived. Every join in it is a place the opt-in can leak, and
// each of the layers is tested on its own elsewhere.
var _ = Describe("A run streamed to a caller", func() {
	// The conversation the specs drive: a call that reasons, says something and runs a
	// tool, then a call that answers.
	scripted := func() (deltaTurn, deltaTurn) {
		reasoning := deltaTurn{
			deltas: []llm.Delta{
				{Kind: llm.DeltaThinking, Index: 0, Text: "checking "},
				{Kind: llm.DeltaThinking, Index: 0, Text: "the streams"},
				{Kind: llm.DeltaThinking, Index: 0, Final: true},
				{Kind: llm.DeltaText, Index: 1, Text: "one "},
				{Kind: llm.DeltaText, Index: 1, Text: "moment", Final: true},
			},
			resp: &llm.Response{
				StopReason: llm.StopToolUse,
				Content: []llm.ContentBlock{
					{Thinking: &llm.ThinkingBlock{Text: "checking the streams", Signature: []byte("opaque")}},
					{Text: &llm.TextBlock{Text: "one moment"}},
					{ToolUse: &llm.ToolUseBlock{ID: "c1", Name: "streams", Input: json.RawMessage(`{}`)}},
				},
			},
		}

		answer := deltaTurn{
			deltas: []llm.Delta{
				{Kind: llm.DeltaText, Index: 0, Text: "there are "},
				{Kind: llm.DeltaText, Index: 0, Text: "three streams", Final: true},
			},
			resp: agenttest.TextResponse("there are three streams"),
		}

		return reasoning, answer
	}

	// prompt sends one prompt and reads the whole set it answers on.
	prompt := func(ctx context.Context, client *a2a.Client, deltas bool) ([]wireBlock, *a2a.Result, *a2a.TaskStream) {
		GinkgoHelper()

		stream, err := client.Task(ctx, "agent1", asks("how many streams are there", deltas))
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(stream.Close)

		blocks, res := readSet(ctx, stream)

		return blocks, res, stream
	}

	It("Should carry every fragment before the whole block it belongs to, under the index and call that produced it", func() {
		reasoning, answer := scripted()
		script := newStreamingScript(reasoning, answer)

		runWorker(script, nil, func(ctx context.Context, client *a2a.Client) {
			blocks, res, stream := prompt(ctx, client, true)
			Expect(res.Text).To(Equal("there are three streams"))
			Expect(stream.Gaps()).To(BeZero())

			called, streamed := script.Counts()
			Expect(streamed).To(Equal(2), "a caller that asked for fragments puts every call of the run on the streaming path")
			Expect(called).To(BeZero())

			reasoned := blockKey{call: 1, index: 0}
			narration := blockKey{call: 1, index: 1}
			answered := blockKey{call: 2, index: 0}

			text, finals := fragmentText(blocks, reasoned)
			Expect(text).To(Equal("checking the streams"))
			Expect(finals).To(Equal(1), "one fragment closes a block")

			text, finals = fragmentText(blocks, narration)
			Expect(text).To(Equal("one moment"))
			Expect(finals).To(Equal(1))

			text, finals = fragmentText(blocks, answered)
			Expect(text).To(Equal("there are three streams"))
			Expect(finals).To(Equal(1))

			// The whole blocks still say what they say to a caller that streams nothing:
			// the reasoning without its signature, the narration, and the answer marked as
			// the text the run ended on.
			wholes := wholeBlocks(blocks)
			Expect(wholes).To(HaveLen(3))
			Expect(wholes[reasoned]).To(Equal(a2a.ThinkingBlock{Text: "checking the streams"}))
			Expect(wholes[narration]).To(Equal(a2a.TextBlock{Text: "one moment", Index: 1}))
			Expect(wholes[answered]).To(Equal(a2a.TextBlock{Text: "there are three streams", Final: true}))

			fragments, at := positions(blocks)
			Expect(at).To(HaveLen(3))

			for key, whole := range at {
				Expect(fragments[key]).ToNot(BeEmpty(), "call %d block %d streamed no fragment", key.call, key.index)

				for _, pos := range fragments[key] {
					Expect(pos).To(BeNumerically("<", whole), "a fragment of call %d block %d arrived after the whole block", key.call, key.index)
				}
			}
		})
	})

	It("Should leave a receiver reconciling the set with the text the model wrote", func() {
		reasoning, answer := scripted()

		runWorker(newStreamingScript(reasoning, answer), nil, func(ctx context.Context, client *a2a.Client) {
			blocks, res, _ := prompt(ctx, client, true)
			Expect(res.Text).To(Equal("there are three streams"))

			calls := assembleCalls(blocks)
			Expect(calls).To(HaveLen(2), "a call's blocks are reset on the status block that ends it")

			Expect(calls[1]).To(Equal([]llm.AssembledBlock{
				{Kind: llm.DeltaThinking, Index: 0, Text: "checking the streams", Source: llm.SourceBlock},
				{Kind: llm.DeltaText, Index: 1, Text: "one moment", Source: llm.SourceBlock},
			}))

			Expect(calls[2]).To(Equal([]llm.AssembledBlock{
				{Kind: llm.DeltaText, Index: 0, Text: "there are three streams", Source: llm.SourceBlock},
			}))
		})
	})

	// A caller that asked for no fragments gets what it got before there were any. A change
	// that moved the recorder's answer, or made the sink send a fragment whatever the
	// request said, fails here.
	It("Should send no fragment to a caller that asked for none, and the blocks a run that cannot stream sends", func() {
		reasoning, answer := scripted()

		var streamedRun []a2a.BlockContent

		script := newStreamingScript(reasoning, answer)
		runWorker(script, nil, func(ctx context.Context, client *a2a.Client) {
			blocks, res, _ := prompt(ctx, client, false)
			Expect(res.Text).To(Equal("there are three streams"))
			Expect(wholeBlocks(blocks)).To(HaveLen(3), "the reasoning, the narration and the answer")

			streamedRun = contentsOf(blocks)
		})

		for _, content := range streamedRun {
			Expect(content).ToNot(BeAssignableToTypeOf(a2a.TextDeltaBlock{}))
			Expect(content).ToNot(BeAssignableToTypeOf(a2a.ThinkingDeltaBlock{}))
		}

		called, streamed := script.Counts()
		Expect(called).To(Equal(2), "a caller that asked for no fragments leaves the run on the ordinary call")
		Expect(streamed).To(BeZero())

		// The same script against a backend with no CallStream at all, which is the run
		// this one has to match block for block.
		var plainRun []a2a.BlockContent

		runWorker(agenttest.NewScriptedProvider(GinkgoTB(), reasoning.resp, answer.resp), nil, func(ctx context.Context, client *a2a.Client) {
			blocks, res, _ := prompt(ctx, client, false)
			Expect(res.Text).To(Equal("there are three streams"))

			plainRun = contentsOf(blocks)
		})

		Expect(streamedRun).To(Equal(plainRun))
	})

	// The fragments an index holds may have a hole in them that nothing downstream can
	// see, which is why the rule takes the whole block.
	It("Should assemble from the whole block when a fragment was lost on the way", func() {
		// One fragment over the flush cap, so the sink puts the block on the wire as more
		// than one message and losing one still leaves the caller part of the text.
		text := strings.Repeat("streams ", 2500)

		turn := deltaTurn{
			deltas: []llm.Delta{
				{Kind: llm.DeltaText, Index: 0, Text: text},
				{Kind: llm.DeltaText, Index: 0, Final: true},
			},
			resp: agenttest.TextResponse(text),
		}

		runWorker(newStreamingScript(turn), dropFirst(a2a.BlockTextDelta), func(ctx context.Context, client *a2a.Client) {
			blocks, res, stream := prompt(ctx, client, true)
			Expect(res.Text).To(Equal(text))
			Expect(stream.Gaps()).To(Equal(uint64(1)), "the caller sees the message it never received")

			held, _ := fragmentText(blocks, blockKey{call: 1, index: 0})
			Expect(held).ToNot(Equal(text), "the fragments that arrived are short of the answer")

			Expect(assembleCalls(blocks)[1]).To(Equal([]llm.AssembledBlock{
				{Kind: llm.DeltaText, Index: 0, Text: text, Source: llm.SourceBlock},
			}))
		})
	})

	// The whole block is cut to what one block carries. The fragments are capped one
	// message at a time and never in aggregate, so they carry all of it.
	It("Should keep the fragments when the whole block arrives trimmed", func() {
		text := strings.Repeat("a", a2a.MaxBlockText+6000)

		var deltas []llm.Delta
		for at := 0; at < len(text); at += 10000 {
			deltas = append(deltas, llm.Delta{Kind: llm.DeltaText, Index: 0, Text: text[at:min(at+10000, len(text))]})
		}
		deltas = append(deltas, llm.Delta{Kind: llm.DeltaText, Index: 0, Final: true})

		turn := deltaTurn{deltas: deltas, resp: agenttest.TextResponse(text)}

		runWorker(newStreamingScript(turn), nil, func(ctx context.Context, client *a2a.Client) {
			blocks, _, stream := prompt(ctx, client, true)
			Expect(stream.Gaps()).To(BeZero())

			whole, ok := wholeBlocks(blocks)[blockKey{call: 1, index: 0}].(a2a.TextBlock)
			Expect(ok).To(BeTrue())
			Expect(whole.Trimmed).To(BeTrue())
			Expect(whole.Text).ToNot(Equal(text))

			Expect(assembleCalls(blocks)[1]).To(Equal([]llm.AssembledBlock{
				{Kind: llm.DeltaText, Index: 0, Text: text, Source: llm.SourceKeptFragments},
			}))
		})
	})
})
