//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/toolkit"
	"github.com/choria-io/fisk-ai/internal/util"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// movingTool is a tool for the moving-set tests: it honors deferral (so a set that
// crosses the tool-search threshold actually defers), records that it ran, and can
// publish a new set from inside its own Execute, which is where a tool list arriving
// mid-turn lands.
type movingTool struct {
	name  string
	onRun func()

	// One set can be read by several runs at once, so a tool in it can be executing on
	// more than one goroutine.
	mu  sync.Mutex
	ran int
}

func (t *movingTool) Name() string                { return t.name }
func (t *movingTool) Description() string         { return t.name }
func (t *movingTool) ModelDescription() string    { return t.name }
func (t *movingTool) InputSchema() map[string]any { return map[string]any{"type": "object"} }
func (t *movingTool) MCPExposable() bool          { return false }
func (t *movingTool) A2AExposable() bool          { return false }

func (t *movingTool) Definition(deferLoading bool) llm.ToolDef {
	return llm.ToolDef{Name: t.name, DeferLoading: deferLoading}
}

func (t *movingTool) Execute(context.Context, json.RawMessage, toolkit.ExecDeps) (*toolkit.Outcome, error) {
	t.mu.Lock()
	t.ran++
	t.mu.Unlock()

	if t.onRun != nil {
		t.onRun()
	}

	return &toolkit.Outcome{Output: "ran " + t.name}, nil
}

// runs is how many times the tool executed.
func (t *movingTool) runs() int {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.ran
}

// noSearchProvider answers like providerFunc but reports a backend that cannot do
// tool search, which is what raises the degradation advisory.
type noSearchProvider func(context.Context, llm.Request) (*llm.Response, error)

func (f noSearchProvider) Call(ctx context.Context, req llm.Request) (*llm.Response, error) {
	return f(ctx, req)
}

func (noSearchProvider) Capabilities() llm.Caps {
	return llm.Caps{Provider: "anthropic"}
}

// toolSetCfg is the configuration a loop test needs: a model to name and a call
// timeout to bound the call that never leaves the process.
func toolSetCfg() *config.Config {
	cfg := &config.Config{}
	cfg.LLM.Model = "test-model"
	cfg.LLM.Budget.CallTimeoutParsed = time.Second

	return cfg
}

// defNames is the tool names a request carried, in the order it carried them.
func defNames(defs []llm.ToolDef) []string {
	out := make([]string, 0, len(defs))
	for _, d := range defs {
		out = append(out, d.Name)
	}

	return out
}

// movingTools builds n tools named t0..t(n-1), for the counts the tool-search
// threshold turns on.
func movingTools(n int) []toolkit.Tool {
	out := make([]toolkit.Tool, 0, n)
	for i := range n {
		out = append(out, &movingTool{name: fmt.Sprintf("t%d", i)})
	}

	return out
}

const toolSetFinalMsg = `{"id":"mf","type":"message","role":"assistant","model":"m","stop_reason":"end_turn","content":[{"type":"text","text":"done"}],"usage":{"input_tokens":1,"output_tokens":1}}`

// toolUseMsg is a non-terminal reply calling one tool, so the loop runs the batch and
// makes another model call.
func toolUseMsg(id, name string) string {
	return fmt.Sprintf(`{"id":"m1","type":"message","role":"assistant","model":"m","stop_reason":"tool_use","content":[{"type":"tool_use","id":%q,"name":%q,"input":{}}],"usage":{"input_tokens":1,"output_tokens":1}}`, id, name)
}

var _ = Describe("a tool set that moves during a run", func() {
	It("sends the set published between two model calls", func() {
		a := &movingTool{name: "a"}
		b := &movingTool{name: "b"}

		src := NewToolSource(NewToolSet([]toolkit.Tool{a}, nil, false))

		var sent [][]string
		calls := 0
		r := &runner{
			cfg: toolSetCfg(), stats: &util.RunStats{}, maxIter: 5, events: nopEvents{},
			messages: []llm.Message{userMsg("go")},
			toolSrc:  src,
			provider: providerFunc(func(_ context.Context, req llm.Request) (*llm.Response, error) {
				sent = append(sent, defNames(req.Tools))
				calls++
				if calls > 1 {
					return mustResponse(toolSetFinalMsg), nil
				}

				// What an MCP session's goroutine does when a server reports a longer
				// list: computed once, published once.
				src.Publish(NewToolSet([]toolkit.Tool{a, b}, nil, false))

				return mustResponse(toolUseMsg("t1", "a")), nil
			}),
		}

		reason, err := r.loop(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(reason).To(Equal(runstate.ReasonCompleted))

		Expect(sent).To(HaveLen(2))
		Expect(sent[0]).To(Equal([]string{"a"}))
		Expect(sent[1]).To(Equal([]string{"a", "b"}))
	})

	It("dispatches a whole tool batch against the set its own model call carried", func() {
		goes := &movingTool{name: "goes"}
		keep := &movingTool{name: "keep"}

		src := NewToolSource(NewToolSet([]toolkit.Tool{keep, goes}, nil, false))

		// The first tool of the batch removes the second, which the model has already
		// asked for in the same reply.
		keep.onRun = func() { src.Publish(NewToolSet([]toolkit.Tool{keep}, nil, false)) }

		batch := `{"id":"m1","type":"message","role":"assistant","model":"m","stop_reason":"tool_use","content":[` +
			`{"type":"tool_use","id":"t1","name":"keep","input":{}},` +
			`{"type":"tool_use","id":"t2","name":"goes","input":{}}` +
			`],"usage":{"input_tokens":1,"output_tokens":1}}`

		ev := &captureEvents{}
		var sent [][]string
		calls := 0
		r := &runner{
			cfg: toolSetCfg(), stats: &util.RunStats{}, maxIter: 5, events: ev,
			messages: []llm.Message{userMsg("go")},
			toolSrc:  src,
			provider: providerFunc(func(_ context.Context, req llm.Request) (*llm.Response, error) {
				sent = append(sent, defNames(req.Tools))
				calls++
				if calls > 1 {
					return mustResponse(toolSetFinalMsg), nil
				}

				return mustResponse(batch), nil
			}),
		}

		reason, err := r.loop(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(reason).To(Equal(runstate.ReasonCompleted))

		// The removed tool still ran and still answered its call, because the batch was
		// dispatched against the set its own model call carried.
		Expect(goes.runs()).To(Equal(1))
		res := findToolResult(r.messages, "t2")
		Expect(res).NotTo(BeNil())
		Expect(res.IsError).To(BeFalse())
		Expect(res.Content).To(Equal("ran goes"))
		Expect(ev.warns).To(BeEmpty())

		// The next call is where the removal applies.
		Expect(sent).To(HaveLen(2))
		Expect(sent[0]).To(Equal([]string{"keep", "goes"}))
		Expect(sent[1]).To(Equal([]string{"keep"}))
	})

	It("re-decides tool search as the set crosses the threshold in either direction", func() {
		small := movingTools(2)
		big := movingTools(util.ToolSearchThreshold)

		src := NewToolSource(NewToolSet(small, nil, true))

		var search []bool
		var deferred []bool
		calls := 0
		r := &runner{
			cfg: toolSetCfg(), stats: &util.RunStats{}, maxIter: 5, events: nopEvents{},
			messages: []llm.Message{userMsg("go")},
			toolSrc:  src,
			provider: providerFunc(func(_ context.Context, req llm.Request) (*llm.Response, error) {
				search = append(search, req.ToolSearch)
				deferred = append(deferred, req.Tools[0].DeferLoading)
				calls++

				switch calls {
				case 1:
					src.Publish(NewToolSet(big, nil, true))
				case 2:
					src.Publish(NewToolSet(small, nil, true))
				default:
					return mustResponse(toolSetFinalMsg), nil
				}

				return mustResponse(toolUseMsg(fmt.Sprintf("t%d", calls), "t0")), nil
			}),
		}

		reason, err := r.loop(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(reason).To(Equal(runstate.ReasonCompleted))

		Expect(search).To(Equal([]bool{false, true, false}))
		Expect(deferred).To(Equal([]bool{false, true, false}))
	})

	It("reports a set that crosses the threshold without tool search once for the run", func() {
		src := NewToolSource(NewToolSet(movingTools(2), nil, false))

		ev := &captureEvents{}
		calls := 0
		r := &runner{
			cfg: toolSetCfg(), stats: &util.RunStats{}, maxIter: 6, events: ev,
			messages: []llm.Message{userMsg("go")},
			toolSrc:  src,
			provider: noSearchProvider(func(context.Context, llm.Request) (*llm.Response, error) {
				calls++

				switch calls {
				case 1:
					// Over the threshold: the advisory is due at the next call.
					src.Publish(NewToolSet(movingTools(util.ToolSearchThreshold), nil, false))
				case 2:
					// Back under it, then over it again: the operator hears it once.
					src.Publish(NewToolSet(movingTools(2), nil, false))
				case 3:
					src.Publish(NewToolSet(movingTools(util.ToolSearchThreshold+2), nil, false))
				default:
					return mustResponse(toolSetFinalMsg), nil
				}

				return mustResponse(toolUseMsg(fmt.Sprintf("t%d", calls), "t0")), nil
			}),
		}

		reason, err := r.loop(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(reason).To(Equal(runstate.ReasonCompleted))

		Expect(calls).To(Equal(4))
		Expect(ev.warns).To(HaveLen(1))
		Expect(ev.warns[0].Kind).To(Equal(WarnToolSearchUnsupported))
		Expect(ev.warns[0].Count).To(Equal(util.ToolSearchThreshold))
	})

	It("stays quiet about a set that already crossed the threshold before the run started", func() {
		ev := &captureEvents{}
		r := &runner{
			cfg: toolSetCfg(), stats: &util.RunStats{}, maxIter: 2, events: ev,
			messages:         []llm.Message{userMsg("go")},
			toolSrc:          NewToolSource(NewToolSet(movingTools(util.ToolSearchThreshold), nil, false)),
			toolSearchWarned: true,
			provider: noSearchProvider(func(context.Context, llm.Request) (*llm.Response, error) {
				return mustResponse(toolSetFinalMsg), nil
			}),
		}

		_, err := r.loop(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(ev.warns).To(BeEmpty())
	})

	It("backs several runs at once while a session keeps publishing", func() {
		a := &movingTool{name: "a"}
		b := &movingTool{name: "b"}
		one := NewToolSet([]toolkit.Tool{a}, nil, false)
		two := NewToolSet([]toolkit.Tool{a, b}, nil, false)

		// One source, the shape fisk serve builds: the runs share a configuration and an
		// application, so a rebuild is computed once and published once.
		src := NewToolSource(one)

		done := make(chan struct{})
		var publisher sync.WaitGroup
		publisher.Add(1)
		go func() {
			defer publisher.Done()
			defer GinkgoRecover()

			for i := 0; ; i++ {
				select {
				case <-done:
					return
				default:
				}

				if i%2 == 0 {
					src.Publish(two)
					continue
				}
				src.Publish(one)
			}
		}()

		var runs sync.WaitGroup
		for range 3 {
			runs.Add(1)
			go func() {
				defer runs.Done()
				defer GinkgoRecover()

				calls := 0
				r := &runner{
					cfg: toolSetCfg(), stats: &util.RunStats{}, maxIter: 8, events: nopEvents{},
					messages: []llm.Message{userMsg("go")},
					toolSrc:  src,
					provider: providerFunc(func(_ context.Context, req llm.Request) (*llm.Response, error) {
						// Whichever set the call took, it took the whole of it.
						Expect(defNames(req.Tools)).To(Or(Equal([]string{"a"}), Equal([]string{"a", "b"})))
						calls++
						if calls > 3 {
							return mustResponse(toolSetFinalMsg), nil
						}

						return mustResponse(toolUseMsg(fmt.Sprintf("t%d", calls), "a")), nil
					}),
				}

				reason, err := r.loop(context.Background())
				Expect(err).NotTo(HaveOccurred())
				Expect(reason).To(Equal(runstate.ReasonCompleted))
			}()
		}

		runs.Wait()
		close(done)
		publisher.Wait()

		Expect(a.runs()).To(Equal(9))
	})

	It("sends the same tools on every call of a run nothing publishes to", func() {
		set := NewToolSet(movingTools(3), nil, true)
		src := NewToolSource(set)

		var sent [][]llm.ToolDef
		var search []bool
		calls := 0
		r := &runner{
			cfg: toolSetCfg(), stats: &util.RunStats{}, maxIter: 5, events: nopEvents{},
			messages: []llm.Message{userMsg("go")},
			toolSrc:  src,
			provider: providerFunc(func(_ context.Context, req llm.Request) (*llm.Response, error) {
				sent = append(sent, req.Tools)
				search = append(search, req.ToolSearch)
				calls++
				if calls > 2 {
					return mustResponse(toolSetFinalMsg), nil
				}

				return mustResponse(toolUseMsg(fmt.Sprintf("t%d", calls), "t0")), nil
			}),
		}

		reason, err := r.loop(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(reason).To(Equal(runstate.ReasonCompleted))

		Expect(sent).To(HaveLen(3))
		for _, defs := range sent {
			Expect(defs).To(Equal(set.defs))
		}
		Expect(search).To(Equal([]bool{false, false, false}))
	})
})

var _ = Describe("ToolSource", func() {
	It("hands out a set a later publish does not change", func() {
		a := &movingTool{name: "a"}
		b := &movingTool{name: "b"}

		src := NewToolSource(NewToolSet([]toolkit.Tool{a}, nil, false))
		held := src.Snapshot()

		src.Publish(NewToolSet([]toolkit.Tool{a, b}, nil, false))

		Expect(defNames(held.defs)).To(Equal([]string{"a"}))
		_, ok := held.tool("b")
		Expect(ok).To(BeFalse())

		Expect(defNames(src.Snapshot().defs)).To(Equal([]string{"a", "b"}))
	})

	It("refuses a nil set rather than leaving every run with no tools", func() {
		set := NewToolSet([]toolkit.Tool{&movingTool{name: "a"}}, nil, false)
		src := NewToolSource(set)

		src.Publish(nil)

		Expect(src.Snapshot()).To(BeIdenticalTo(set))
	})
})

var _ = Describe("NewToolSet", func() {
	It("sends the built-in tools after the deferrable ones and never defers them", func() {
		set := NewToolSet(movingTools(util.ToolSearchThreshold), []toolkit.Tool{&movingTool{name: "hitl"}}, true)

		names := defNames(set.defs)
		Expect(names).To(HaveLen(util.ToolSearchThreshold + 1))
		Expect(names[len(names)-1]).To(Equal("hitl"))
		Expect(set.defs[0].DeferLoading).To(BeTrue())
		Expect(set.defs[len(set.defs)-1].DeferLoading).To(BeFalse())
		Expect(set.search).To(BeTrue())

		// The registry holds both kinds, so the model is offered nothing the runner
		// cannot dispatch.
		_, ok := set.tool("hitl")
		Expect(ok).To(BeTrue())
		_, ok = set.tool("t0")
		Expect(ok).To(BeTrue())
	})

	It("counts the built-ins toward the threshold", func() {
		set := NewToolSet(movingTools(util.ToolSearchThreshold-1), []toolkit.Tool{&movingTool{name: "hitl"}}, true)
		Expect(set.search).To(BeTrue())

		set = NewToolSet(movingTools(util.ToolSearchThreshold-1), nil, true)
		Expect(set.search).To(BeFalse())
	})
})
