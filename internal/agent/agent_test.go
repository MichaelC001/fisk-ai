//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/choria-io/fisk"
	"github.com/segmentio/ksuid"

	wire "github.com/choria-io/fisk-ai/internal/a2a/wire/v1"
	"github.com/choria-io/fisk-ai/internal/toolkit"
	"github.com/choria-io/fisk-ai/internal/toolkit/fisktool"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/a2a"
	"github.com/choria-io/fisk-ai/internal/llm"
	llmanthropic "github.com/choria-io/fisk-ai/internal/llm/anthropic"
	"github.com/choria-io/fisk-ai/internal/remotetools"
	"github.com/choria-io/fisk-ai/internal/runstate"
	runstatefile "github.com/choria-io/fisk-ai/internal/runstate/file"
	"github.com/choria-io/fisk-ai/internal/telemetry"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestAgent(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Agent")
}

// toolSetOf builds a runner's tool set from the name-keyed dispatch map these tests
// write, in name order so the definitions it sends are stable. Each key must be the
// tool's own Name, which is the name the set registers it under.
func toolSetOf(tools map[string]toolkit.Tool) *ToolSet {
	GinkgoHelper()

	out := make([]toolkit.Tool, 0, len(tools))
	for _, name := range slices.Sorted(maps.Keys(tools)) {
		Expect(tools[name].Name()).To(Equal(name))
		out = append(out, tools[name])
	}

	return NewToolSet(out, nil, false)
}

// toolSrcOf is toolSetOf published to a source, for a test that drives the loop
// rather than a single tool call.
func toolSrcOf(tools map[string]toolkit.Tool) *ToolSource {
	GinkgoHelper()

	return NewToolSource(toolSetOf(tools))
}

// nopEvents discards every event, for tests that exercise the loop rather than
// its rendering.
type nopEvents struct{}

func (nopEvents) Warn(Warning)                                                          {}
func (nopEvents) Starting(RunInfo)                                                      {}
func (nopEvents) RemoteHostNotes([]remotetools.HostImport)                              {}
func (nopEvents) ResumeTranscript(*runstate.RunState, map[string]*fisktool.CommandTool) {}
func (nopEvents) LLMRequest(string)                                                     {}
func (nopEvents) ToolCall(ToolTrace)                                                    {}
func (nopEvents) ToolResult(ToolResultTrace)                                            {}
func (nopEvents) Message(llm.Response, bool)                                            {}
func (nopEvents) SessionRotated(string)                                                 {}
func (nopEvents) Panicked(any, []byte)                                                  {}

// captureEvents records the tool traces so a test can assert what was emitted; it
// inherits the no-op behavior for every other event.
type captureEvents struct {
	nopEvents
	calls   []ToolTrace
	results []ToolResultTrace
	warns   []Warning
}

func (c *captureEvents) ToolCall(t ToolTrace)         { c.calls = append(c.calls, t) }
func (c *captureEvents) ToolResult(t ToolResultTrace) { c.results = append(c.results, t) }
func (c *captureEvents) Warn(w Warning)               { c.warns = append(c.warns, w) }

// warnRecorder records the warnings a run emits, inheriting the no-op behavior for
// every other event.
type warnRecorder struct {
	nopEvents
	warns []Warning
}

func (w *warnRecorder) Warn(x Warning) { w.warns = append(w.warns, x) }

func (w *warnRecorder) has(kind WarningKind) bool {
	for _, x := range w.warns {
		if x.Kind == kind {
			return true
		}
	}
	return false
}

// rotateRecorder records the previous session ids reported when a context reset rotates
// to a fresh session, inheriting the no-op behavior for every other event.
type rotateRecorder struct {
	nopEvents
	prevIDs []string
}

func (r *rotateRecorder) SessionRotated(prevID string) { r.prevIDs = append(r.prevIDs, prevID) }

// stubInvoker is a canned a2a.RemoteInvoker for driving a RemoteTool through the
// runner without a transport: every call returns the same reply.
type stubInvoker struct {
	reply *wire.ToolReply
}

func (s stubInvoker) InvokeTool(context.Context, string, string, json.RawMessage) (*wire.ToolReply, error) {
	return s.reply, nil
}

// recordingTool is a controllable in-process tool for the hook tests: it records the
// arguments it was executed with (so a test can prove which tool ran after a rewrite and
// with what input) and returns a canned result. It implements only toolkit.Tool, so it is
// not Confirmable and describes to toolkit.KindUnknown, which is all the hook tests need.
type recordingTool struct {
	name      string
	output    string
	isError   bool
	ranInputs []string
}

func (t *recordingTool) Name() string                { return t.name }
func (t *recordingTool) Description() string         { return t.name }
func (t *recordingTool) InputSchema() map[string]any { return map[string]any{"type": "object"} }
func (t *recordingTool) Definition(bool) llm.ToolDef { return llm.ToolDef{Name: t.name} }

func (t *recordingTool) ModelDescription() string { return t.name }
func (t *recordingTool) MCPExposable() bool       { return false }
func (t *recordingTool) A2AExposable() bool       { return false }

func (t *recordingTool) Execute(_ context.Context, input json.RawMessage, _ toolkit.ExecDeps) (*toolkit.Outcome, error) {
	t.ranInputs = append(t.ranInputs, string(input))
	if t.isError {
		return nil, errors.New(t.output)
	}

	return &toolkit.Outcome{Output: t.output}, nil
}

// findToolResult returns the tool_result block answering id in a reconstructed
// conversation, or nil when none does.
func findToolResult(msgs []llm.Message, id string) *llm.ToolResultBlock {
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.ToolResult != nil && b.ToolResult.ToolUseID == id {
				return b.ToolResult
			}
		}
	}
	return nil
}

// userTexts collects the text of every user-role message in a reconstructed
// conversation, so a test can assert which prompts were (and were not) journaled.
func userTexts(msgs []llm.Message) []string {
	var out []string
	for _, m := range msgs {
		if m.Role != llm.RoleUser {
			continue
		}
		for _, b := range m.Content {
			if b.Text != nil {
				out = append(out, b.Text.Text)
			}
		}
	}
	return out
}

func mustMessage(j string) *anthropic.Message {
	GinkgoHelper()
	var m anthropic.Message
	Expect(json.Unmarshal([]byte(j), &m)).To(Succeed())
	return &m
}

// mustResponse builds a neutral llm.Response from an Anthropic message JSON, so the
// test data stays the wire form the model returns while the loop consumes neutral.
func mustResponse(j string) *llm.Response {
	GinkgoHelper()
	resp, err := llmanthropic.ResponseToNeutral(mustMessage(j))
	Expect(err).NotTo(HaveOccurred())
	return &resp
}

// providerFunc adapts a plain function to the llm.Provider interface so a test can
// drive the loop with scripted responses in place of a live API call.
type providerFunc func(context.Context, llm.Request) (*llm.Response, error)

func (f providerFunc) Call(ctx context.Context, req llm.Request) (*llm.Response, error) {
	return f(ctx, req)
}

func (providerFunc) Capabilities() llm.Caps {
	return llm.Caps{Provider: "anthropic", SupportsToolSearch: true}
}

// userMsg and assistantTextMsg build the neutral turns a test conversation seeds.
func userMsg(text string) llm.Message {
	return llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Text: &llm.TextBlock{Text: text}}}}
}

func assistantTextMsg(text string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Text: &llm.TextBlock{Text: text}}}}
}

// failOnUserJournal wraps a real journal but rejects the interactive user record, so a
// test can exercise the "journaling a follow-up failed" path without corrupting a real
// store. Every other record is appended normally.
type failOnUserJournal struct {
	runstate.Journal
}

func (j failOnUserJournal) Append(ctx context.Context, seq uint64, rec runstate.Record) error {
	if rec.Protocol == runstate.UserProtocol {
		return errors.New("disk full")
	}
	return j.Journal.Append(ctx, seq, rec)
}

// testCtx is what the specs below hand the session store. None of them tests
// cancellation, which the store's own package covers.
var testCtx = context.Background()

var _ = Describe("runner", func() {
	Describe("resumeHazards", func() {
		It("warns when resuming at a paused-turn boundary", func() {
			rs := &runstate.RunState{LastStopReason: "pause_turn"}
			ws := resumeHazards(rs)
			Expect(ws).To(HaveLen(1))
			Expect(ws[0].Kind).To(Equal(WarnResumePausedTurn))
		})

		It("stays quiet for an ordinary resume", func() {
			rs := &runstate.RunState{LastStopReason: "tool_use"}
			Expect(resumeHazards(rs)).To(BeEmpty())
		})
	})

	Describe("suspend then resume across runners", func() {
		It("journals a run, suspends at a boundary, and resumes in a fresh runner to completion", func() {
			store, err := runstatefile.NewFileStore(GinkgoT().TempDir())
			Expect(err).NotTo(HaveOccurred())
			id := ksuid.New().String()

			cfg := &config.Config{}
			cfg.LLM.Model = "test-model"
			cfg.LLM.Budget.CallTimeoutParsed = time.Second

			toolMsg := `{"id":"m1","type":"message","role":"assistant","model":"m","stop_reason":"tool_use","content":[{"type":"tool_use","id":"toolu_1","name":"missing","input":{}}],"usage":{"input_tokens":10,"output_tokens":5}}`
			finalMsg := `{"id":"m2","type":"message","role":"assistant","model":"m","stop_reason":"end_turn","content":[{"type":"text","text":"all done"}],"usage":{"input_tokens":3,"output_tokens":2}}`

			// Both fields: the loop takes its own snapshot per model call, while a
			// restored in-flight batch runs against the set the runner was built with.
			emptyTools := func(r *runner) {
				r.set = toolSetOf(nil)
				r.toolSrc = NewToolSource(r.set)
			}

			// Runner A: one tool-using turn, then a suspend request lands, so the
			// loop stops at the next boundary before calling the LLM again.
			jA, err := store.Create(testCtx, id, runstate.MetaRecord{Version: runstate.Version, RunID: id, Prompt: "go"})
			Expect(err).NotTo(HaveOccurred())

			var suspendNow atomic.Bool
			rA := &runner{
				cfg: cfg, stats: &RunStats{}, maxIter: 10, events: nopEvents{},
				messages:         []llm.Message{userMsg("go")},
				journal:          jA,
				seq:              1,
				suspendRequested: suspendNow.Load,
				provider: providerFunc(func(context.Context, llm.Request) (*llm.Response, error) {
					suspendNow.Store(true)
					return mustResponse(toolMsg), nil
				}),
			}
			emptyTools(rA)

			reason, err := rA.run(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(reason).To(Equal(runstate.ReasonSuspended))
			Expect(jA.Close()).To(Succeed())

			// The suspended session is resumable, its one tool turn recorded.
			mid, err := store.Load(testCtx, id)
			Expect(err).NotTo(HaveOccurred())
			Expect(mid.Completed()).To(BeFalse())
			Expect(mid.NextIteration).To(Equal(int64(1)))
			Expect(mid.Counters.LlmCalls).To(Equal(int64(1)))

			// Runner B: a fresh runner seeded from the store finishes the run.
			jB, err := store.Open(testCtx, id)
			Expect(err).NotTo(HaveOccurred())

			rB := &runner{
				cfg: cfg, stats: &RunStats{}, maxIter: 10, events: nopEvents{},
				messages:  mid.Messages,
				journal:   jB,
				seq:       jB.LastSeq(),
				startIter: mid.NextIteration,
				pending:   mid.Pending,
				provider: providerFunc(func(context.Context, llm.Request) (*llm.Response, error) {
					return mustResponse(finalMsg), nil
				}),
			}
			emptyTools(rB)

			reason, err = rB.run(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(reason).To(Equal(runstate.ReasonCompleted))
			Expect(jB.Close()).To(Succeed())

			done, err := store.Load(testCtx, id)
			Expect(err).NotTo(HaveOccurred())
			Expect(done.Completed()).To(BeTrue())
			Expect(done.Counters.LlmCalls).To(Equal(int64(2)))
		})
	})

	Describe("executeTool", func() {
		It("emits neither a call nor a result for a tool that never ran", func() {
			ev := &captureEvents{}
			r := &runner{
				stats:  &RunStats{},
				events: ev,
				set:    toolSetOf(nil),
			}

			block, dispatched, _, err := r.executeTool(context.Background(), llm.ToolUseBlock{ID: "t1", Name: "nope"})
			Expect(err).NotTo(HaveOccurred())
			Expect(dispatched).To(BeFalse())
			Expect(block.ToolUseID).To(Equal("t1"))
			Expect(block.IsError).To(BeTrue())

			// An unknown tool is reported as a warning only; it never ran, so it leaves
			// no call or result line in the transcript.
			Expect(ev.calls).To(BeEmpty())
			Expect(ev.results).To(BeEmpty())
		})

		It("rejects a local tool call missing a required parameter without running it", func() {
			ev := &captureEvents{}
			// The tool carries no application path: the call is rejected before it
			// would run, and a tool with no path cannot run at all.
			tool := &fisktool.CommandTool{
				Path: []string{"do"},
				Model: &fisk.CmdModel{RestrictedSchema: map[string]any{
					"type":     "object",
					"required": []string{"subject"},
					"properties": map[string]any{
						"subject": map[string]any{"type": "string"},
						"level":   map[string]any{"type": "string"},
					},
				}},
			}
			r := &runner{
				stats:  &RunStats{},
				events: ev,
				set:    toolSetOf(map[string]toolkit.Tool{"do": tool}),
			}

			block, dispatched, _, err := r.executeTool(context.Background(), llm.ToolUseBlock{ID: "t1", Name: "do", Input: json.RawMessage(`{"level":"info"}`)})
			Expect(err).NotTo(HaveOccurred())
			Expect(dispatched).To(BeFalse())
			Expect(block.ToolUseID).To(Equal("t1"))
			Expect(block.IsError).To(BeTrue())

			// Like an unknown tool, a rejected call never ran, so it emits only a
			// warning naming the missing parameters and no call or result line.
			Expect(ev.calls).To(BeEmpty())
			Expect(ev.results).To(BeEmpty())
			Expect(ev.warns).To(HaveLen(1))
			Expect(ev.warns[0].Kind).To(Equal(WarnMissingRequired))
			Expect(ev.warns[0].Name).To(Equal("do"))
			Expect(ev.warns[0].Params).To(Equal([]string{"subject"}))
		})

		It("dispatches a local command tool: traces the full call line and runs it", func() {
			ev := &captureEvents{}
			tool := runnableCommandTool()
			r := &runner{stats: &RunStats{}, events: ev, set: toolSetOf(map[string]toolkit.Tool{"do": tool})}

			block, dispatched, _, err := r.executeTool(context.Background(), llm.ToolUseBlock{ID: "t1", Name: "do", Input: json.RawMessage(`{}`)})
			Expect(err).NotTo(HaveOccurred())
			Expect(dispatched).To(BeTrue())
			Expect(block.ToolUseID).To(Equal("t1"))
			Expect(block.IsError).To(BeFalse())

			Expect(ev.calls).To(HaveLen(1))
			Expect(ev.calls[0].ProviderKind).To(Equal(toolkit.KindApplication))
			Expect(ev.calls[0].Display).NotTo(BeEmpty())
			Expect(ev.results).To(HaveLen(1))
			Expect(ev.results[0].ProviderKind).To(Equal(toolkit.KindApplication))
		})

		It("dispatches a remote tool: reports the dispatch, counts it, and traces the agent", func() {
			ev := &captureEvents{}
			desc := wire.ToolDescriptor{Name: "info", Description: "reports info", InputSchema: json.RawMessage(`{"type":"object"}`)}
			rt, err := a2a.NewRemoteTool("nats_info", "nats", desc, stubInvoker{reply: wire.NewToolReply("ok", false)})
			Expect(err).NotTo(HaveOccurred())

			r := &runner{stats: &RunStats{}, events: ev, set: toolSetOf(map[string]toolkit.Tool{"nats_info": rt})}

			block, dispatched, _, err := r.executeTool(context.Background(), llm.ToolUseBlock{ID: "t1", Name: "nats_info"})
			Expect(err).NotTo(HaveOccurred())
			Expect(dispatched).To(BeTrue())
			Expect(r.stats.RemoteToolCalls).To(Equal(int64(1)))
			Expect(block.ToolUseID).To(Equal("t1"))
			Expect(block.IsError).To(BeFalse())

			Expect(ev.calls).To(HaveLen(1))
			Expect(ev.calls[0].ProviderKind).To(Equal(toolkit.KindRemote))
			Expect(ev.calls[0].Agent).To(Equal("nats"))
			Expect(ev.results).To(HaveLen(1))
			Expect(ev.results[0].ProviderKind).To(Equal(toolkit.KindRemote))
		})

		It("gates a confirm-tagged local tool and denies it without running when no operator can approve", func() {
			ev := &captureEvents{}
			// The tool carries no application path: the gate denies before it would
			// run, and a tool with no path cannot run at all.
			tool := &fisktool.CommandTool{
				Path:  []string{"stream", "rm"},
				Model: &fisk.CmdModel{Tags: []string{"ai:confirm"}},
			}
			r := &runner{
				stats:  &RunStats{},
				events: ev,
				set:    toolSetOf(map[string]toolkit.Tool{"stream_rm": tool}),
				gate:   NewConfirmGate(toolkit.DefaultDenyPrompter(), nil),
			}

			// With no operator reachable (the deny prompter reports it cannot prompt)
			// there is no one to approve, so the gate denies. The gated tool is never run:
			// no call or result line is emitted, and the denial is an authoritative
			// non-error result to the model.
			block, dispatched, _, err := r.executeTool(context.Background(), llm.ToolUseBlock{ID: "t1", Name: "stream_rm"})
			Expect(err).NotTo(HaveOccurred())
			Expect(dispatched).To(BeFalse())
			Expect(block.ToolUseID).To(Equal("t1"))
			Expect(block.IsError).To(BeFalse())
			Expect(ev.calls).To(BeEmpty())
			Expect(ev.results).To(BeEmpty())
		})
	})

	Describe("tool hooks (PreToolUse/PostToolUse)", func() {
		It("denies a tool call: answers the exact id with an error result, does not run it, and journals it", func() {
			store, err := runstatefile.NewFileStore(GinkgoT().TempDir())
			Expect(err).NotTo(HaveOccurred())
			id := ksuid.New().String()

			cfg := &config.Config{}
			cfg.LLM.Model = "test-model"
			cfg.LLM.Budget.CallTimeoutParsed = time.Second

			toolMsg := `{"id":"m1","type":"message","role":"assistant","model":"m","stop_reason":"tool_use","content":[{"type":"tool_use","id":"toolu_1","name":"danger","input":{}}],"usage":{"input_tokens":10,"output_tokens":5}}`
			finalMsg := `{"id":"m2","type":"message","role":"assistant","model":"m","stop_reason":"end_turn","content":[{"type":"text","text":"done"}],"usage":{"input_tokens":3,"output_tokens":2}}`

			j, err := store.Create(testCtx, id, runstate.MetaRecord{Version: runstate.Version, RunID: id, Prompt: "go"})
			Expect(err).NotTo(HaveOccurred())

			tool := &recordingTool{name: "danger", output: "should never run"}
			var calls int
			r := &runner{
				cfg: cfg, stats: &RunStats{}, maxIter: 10, events: nopEvents{},
				messages: []llm.Message{userMsg("go")},
				journal:  j,
				seq:      1,
				toolSrc:  toolSrcOf(map[string]toolkit.Tool{"danger": tool}),
				hooks: Hooks{
					PreToolUse: func(_ context.Context, in PreToolUseInfo) (PreToolUseResult, error) {
						Expect(in.ToolName).To(Equal("danger"))
						Expect(in.ToolUseID).To(Equal("toolu_1"))
						return PreToolUseResult{Deny: true, DenyReason: "blocked by policy"}, nil
					},
				},
				provider: providerFunc(func(context.Context, llm.Request) (*llm.Response, error) {
					calls++
					if calls == 1 {
						return mustResponse(toolMsg), nil
					}
					return mustResponse(finalMsg), nil
				}),
			}

			reason, err := r.run(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(reason).To(Equal(runstate.ReasonCompleted))
			Expect(j.Close()).To(Succeed())

			// The denied call never ran, yet it is still one counted tool call.
			Expect(tool.ranInputs).To(BeEmpty())
			Expect(r.stats.ToolCalls).To(Equal(int64(1)))

			// The deny is journaled as an error result answering the exact id, so the
			// batch stays well-formed and a resume is consistent.
			rs, err := store.Load(testCtx, id)
			Expect(err).NotTo(HaveOccurred())
			tr := findToolResult(rs.Messages, "toolu_1")
			Expect(tr).NotTo(BeNil())
			Expect(tr.IsError).To(BeTrue())
			Expect(tr.Content).To(Equal("blocked by policy"))
		})

		It("rewrites a call: runs the effective tool with the new arguments, traces it, counts it once", func() {
			orig := &recordingTool{name: "orig", output: "orig-out"}
			safe := &recordingTool{name: "safe", output: "safe-out"}
			ev := &captureEvents{}
			r := &runner{
				stats: &RunStats{}, events: ev,
				set: toolSetOf(map[string]toolkit.Tool{"orig": orig, "safe": safe}),
				hooks: Hooks{
					PreToolUse: func(_ context.Context, in PreToolUseInfo) (PreToolUseResult, error) {
						Expect(in.ToolName).To(Equal("orig"))
						return PreToolUseResult{RewriteTool: "safe", RewriteInput: json.RawMessage(`{"sandbox":true}`)}, nil
					},
				},
			}

			block, dispatched, _, err := r.executeTool(context.Background(), llm.ToolUseBlock{ID: "t1", Name: "orig", Input: json.RawMessage(`{"x":1}`)})
			Expect(err).NotTo(HaveOccurred())
			Expect(dispatched).To(BeTrue())
			Expect(block.ToolUseID).To(Equal("t1"))
			Expect(block.Content).To(Equal("safe-out"))

			// The effective tool ran with the rewritten arguments; the original never ran.
			Expect(safe.ranInputs).To(ConsistOf(`{"sandbox":true}`))
			Expect(orig.ranInputs).To(BeEmpty())

			// Counted exactly once, and the rewrite flowed through the trace under the
			// effective tool's name.
			Expect(r.stats.ToolCalls).To(Equal(int64(1)))
			Expect(ev.calls).To(HaveLen(1))
			Expect(ev.calls[0].Name).To(Equal("safe"))
			Expect(ev.results).To(HaveLen(1))
		})

		It("gates a rewrite on the union: a redirect from a gated tool to an ungated one is still gated", func() {
			// The tool carries no application path: the gate denies before it would
			// run, and a tool with no path cannot run at all.
			orig := &fisktool.CommandTool{
				Path:  []string{"stream", "rm"},
				Model: &fisk.CmdModel{Tags: []string{"ai:confirm"}},
			}
			safe := &recordingTool{name: "safe", output: "safe-out"}
			ev := &captureEvents{}
			r := &runner{
				stats: &RunStats{}, events: ev,
				set:  toolSetOf(map[string]toolkit.Tool{"stream_rm": orig, "safe": safe}),
				gate: NewConfirmGate(toolkit.DefaultDenyPrompter(), nil),
				hooks: Hooks{
					PreToolUse: func(_ context.Context, in PreToolUseInfo) (PreToolUseResult, error) {
						// The original tool is confirm-gated, which the hook observes.
						Expect(in.ConfirmGated).To(BeTrue())
						return PreToolUseResult{RewriteTool: "safe"}, nil
					},
				},
			}

			// With no operator reachable the union gate denies, and the ungated effective
			// tool never runs: a hook cannot strip a gate by redirecting.
			block, dispatched, _, err := r.executeTool(context.Background(), llm.ToolUseBlock{ID: "t1", Name: "stream_rm"})
			Expect(err).NotTo(HaveOccurred())
			Expect(dispatched).To(BeFalse())
			Expect(block.ToolUseID).To(Equal("t1"))
			Expect(block.IsError).To(BeFalse()) // an authoritative confirm denial, not an error
			Expect(safe.ranInputs).To(BeEmpty())
			Expect(ev.calls).To(BeEmpty())
			Expect(ev.results).To(BeEmpty())
		})

		It("replaces the output the model sees when PostToolUse asks to", func() {
			tool := &recordingTool{name: "leaky", output: "BEGIN PRIVATE KEY abc END PRIVATE KEY"}
			ev := &captureEvents{}
			r := &runner{
				stats: &RunStats{}, events: ev,
				set: toolSetOf(map[string]toolkit.Tool{"leaky": tool}),
				hooks: Hooks{
					PostToolUse: func(_ context.Context, in PostToolUseInfo) (PostToolUseResult, error) {
						Expect(in.Output).To(ContainSubstring("PRIVATE KEY"))
						return PostToolUseResult{Replace: true, Output: "[redacted]"}, nil
					},
				},
			}

			block, _, _, err := r.executeTool(context.Background(), llm.ToolUseBlock{ID: "t1", Name: "leaky", Input: json.RawMessage(`{}`)})
			Expect(err).NotTo(HaveOccurred())
			Expect(block.ToolUseID).To(Equal("t1"))
			Expect(block.Content).To(Equal("[redacted]"))

			// The replacement is applied before the result trace, so a renderer never sees
			// the secret either.
			Expect(ev.results).To(HaveLen(1))
			Expect(ev.results[0].Output).To(Equal("[redacted]"))
		})

		It("isolates the run from a hook that mutates the Info snapshot", func() {
			tool := &recordingTool{name: "do", output: "ok"}
			r := &runner{
				stats: &RunStats{}, events: &captureEvents{},
				set: toolSetOf(map[string]toolkit.Tool{"do": tool}),
				hooks: Hooks{
					PreToolUse: func(_ context.Context, in PreToolUseInfo) (PreToolUseResult, error) {
						// Scribbling over the snapshot's buffer must not reach the tool.
						for i := range in.Input {
							in.Input[i] = 'x'
						}
						return PreToolUseResult{}, nil
					},
				},
			}

			block, _, _, err := r.executeTool(context.Background(), llm.ToolUseBlock{ID: "t1", Name: "do", Input: json.RawMessage(`{"a":1}`)})
			Expect(err).NotTo(HaveOccurred())
			Expect(block.IsError).To(BeFalse())
			Expect(tool.ranInputs).To(ConsistOf(`{"a":1}`))
		})

		It("aborts the run when a tool hook returns an error", func() {
			tool := &recordingTool{name: "do", output: "ok"}
			r := &runner{
				stats: &RunStats{}, events: &captureEvents{},
				set: toolSetOf(map[string]toolkit.Tool{"do": tool}),
				hooks: Hooks{
					PreToolUse: func(context.Context, PreToolUseInfo) (PreToolUseResult, error) {
						return PreToolUseResult{}, errors.New("boom")
					},
				},
			}

			_, _, _, err := r.executeTool(context.Background(), llm.ToolUseBlock{ID: "t1", Name: "do", Input: json.RawMessage(`{}`)})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("PreToolUse hook"))
			Expect(tool.ranInputs).To(BeEmpty())
		})

		It("aborts when a rewrite targets an unregistered tool", func() {
			tool := &recordingTool{name: "do", output: "ok"}
			r := &runner{
				stats: &RunStats{}, events: &captureEvents{},
				set: toolSetOf(map[string]toolkit.Tool{"do": tool}),
				hooks: Hooks{
					PreToolUse: func(context.Context, PreToolUseInfo) (PreToolUseResult, error) {
						return PreToolUseResult{RewriteTool: "ghost"}, nil
					},
				},
			}

			_, _, _, err := r.executeTool(context.Background(), llm.ToolUseBlock{ID: "t1", Name: "do", Input: json.RawMessage(`{}`)})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("unregistered"))
			Expect(tool.ranInputs).To(BeEmpty())
		})

		It("aborts when a rewrite produces invalid JSON arguments", func() {
			tool := &recordingTool{name: "do", output: "ok"}
			r := &runner{
				stats: &RunStats{}, events: &captureEvents{},
				set: toolSetOf(map[string]toolkit.Tool{"do": tool}),
				hooks: Hooks{
					PreToolUse: func(context.Context, PreToolUseInfo) (PreToolUseResult, error) {
						return PreToolUseResult{RewriteInput: json.RawMessage(`{not json`)}, nil
					},
				},
			}

			_, _, _, err := r.executeTool(context.Background(), llm.ToolUseBlock{ID: "t1", Name: "do", Input: json.RawMessage(`{}`)})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("invalid JSON"))
			Expect(tool.ranInputs).To(BeEmpty())
		})

		It("re-fires the tool hooks only for the unanswered tools of a restored batch", func() {
			store, err := runstatefile.NewFileStore(GinkgoT().TempDir())
			Expect(err).NotTo(HaveOccurred())
			runID := ksuid.New().String()

			// A partial batch: two calls to "echo", only the first answered before the crash.
			assistant := llm.Message{
				Role: llm.RoleAssistant,
				Content: []llm.ContentBlock{
					{ToolUse: &llm.ToolUseBlock{ID: "toolu_a", Name: "echo", Input: json.RawMessage(`{"n":1}`)}},
					{ToolUse: &llm.ToolUseBlock{ID: "toolu_b", Name: "echo", Input: json.RawMessage(`{"n":2}`)}},
				},
			}

			j, err := store.Create(testCtx, runID, runstate.MetaRecord{Version: runstate.Version, RunID: runID, Prompt: "go"})
			Expect(err).NotTo(HaveOccurred())
			Expect(j.Append(testCtx, 2, runstate.Record{Protocol: runstate.AssistantProtocol, Assistant: &runstate.AssistantRecord{Iteration: 0, Message: assistant}})).To(Succeed())
			Expect(j.Append(testCtx, 3, runstate.Record{Protocol: runstate.ToolResultProtocol, ToolResult: &runstate.ToolResultRecord{ToolUseID: "toolu_a", Result: llm.ToolResultBlock{ToolUseID: "toolu_a", Content: "already done"}}})).To(Succeed())
			Expect(j.Close()).To(Succeed())

			rs, err := store.Load(testCtx, runID)
			Expect(err).NotTo(HaveOccurred())
			Expect(rs.Pending).NotTo(BeNil())

			resumeJ, err := store.Open(testCtx, runID)
			Expect(err).NotTo(HaveOccurred())

			tool := &recordingTool{name: "echo", output: "ran"}
			var preIDs, postIDs []string
			r := &runner{
				stats:    &RunStats{},
				events:   nopEvents{},
				set:      toolSetOf(map[string]toolkit.Tool{"echo": tool}),
				messages: rs.Messages,
				journal:  resumeJ,
				seq:      resumeJ.LastSeq(),
				pending:  rs.Pending,
				hooks: Hooks{
					PreToolUse: func(_ context.Context, in PreToolUseInfo) (PreToolUseResult, error) {
						preIDs = append(preIDs, in.ToolUseID)
						return PreToolUseResult{}, nil
					},
					PostToolUse: func(_ context.Context, in PostToolUseInfo) (PostToolUseResult, error) {
						postIDs = append(postIDs, in.ToolUseID)
						return PostToolUseResult{}, nil
					},
				},
			}

			deferred, err := r.completePending(context.Background())
			Expect(err).ToNot(HaveOccurred())
			Expect(deferred).To(BeFalse())
			Expect(resumeJ.Close()).To(Succeed())

			// The hooks re-fire only for the unanswered tool; the answered one reuses its
			// journaled result and neither re-runs nor re-fires.
			Expect(preIDs).To(ConsistOf("toolu_b"))
			Expect(postIDs).To(ConsistOf("toolu_b"))
			Expect(tool.ranInputs).To(ConsistOf(`{"n":2}`))
		})
	})

	Describe("model-call hooks (PreModelCall/PostModelCall)", func() {
		newCfg := func() *config.Config {
			cfg := &config.Config{}
			cfg.LLM.Model = "test-model"
			cfg.LLM.Budget.CallTimeoutParsed = time.Second
			return cfg
		}

		finalMsg := `{"id":"m2","type":"message","role":"assistant","model":"m","stop_reason":"end_turn","content":[{"type":"text","text":"done"}],"usage":{"input_tokens":3,"output_tokens":2}}`

		It("fires PreModelCall before the call with counts and PostModelCall after with Terminal set on a final reply", func() {
			var pre []PreModelCallInfo
			var post []PostModelCallInfo
			r := &runner{
				cfg: newCfg(), stats: &RunStats{}, maxIter: 5, events: nopEvents{},
				messages: []llm.Message{userMsg("go")},
				toolSrc: toolSrcOf(map[string]toolkit.Tool{
					"a": &recordingTool{name: "a"},
					"b": &recordingTool{name: "b"},
				}),
				hooks: Hooks{
					PreModelCall: func(_ context.Context, in PreModelCallInfo) error {
						pre = append(pre, in)
						return nil
					},
					PostModelCall: func(_ context.Context, in PostModelCallInfo) error {
						post = append(post, in)
						return nil
					},
				},
				provider: providerFunc(func(context.Context, llm.Request) (*llm.Response, error) {
					return mustResponse(finalMsg), nil
				}),
			}

			reason, err := r.loop(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(reason).To(Equal(runstate.ReasonCompleted))

			Expect(pre).To(HaveLen(1))
			Expect(pre[0].Iteration).To(Equal(0))
			Expect(pre[0].Model).To(Equal("test-model"))
			Expect(pre[0].MessageCount).To(Equal(1)) // just the seed user prompt
			Expect(pre[0].ToolCount).To(Equal(2))

			Expect(post).To(HaveLen(1))
			Expect(post[0].Iteration).To(Equal(0))
			Expect(post[0].Terminal).To(BeTrue())
			Expect(post[0].ToolCalls).To(BeEmpty())
		})

		It("fires the model-call hooks once per iteration across a tool turn then a final turn", func() {
			toolMsg := `{"id":"m1","type":"message","role":"assistant","model":"m","stop_reason":"tool_use","content":[{"type":"tool_use","id":"toolu_1","name":"echo","input":{"n":1}}],"usage":{"input_tokens":10,"output_tokens":5}}`

			var pre []PreModelCallInfo
			var post []PostModelCallInfo
			var calls int
			r := &runner{
				cfg: newCfg(), stats: &RunStats{}, maxIter: 5, events: nopEvents{},
				messages: []llm.Message{userMsg("go")},
				toolSrc:  toolSrcOf(map[string]toolkit.Tool{"echo": &recordingTool{name: "echo", output: "ran"}}),
				hooks: Hooks{
					PreModelCall: func(_ context.Context, in PreModelCallInfo) error {
						pre = append(pre, in)
						return nil
					},
					PostModelCall: func(_ context.Context, in PostModelCallInfo) error {
						post = append(post, in)
						return nil
					},
				},
				provider: providerFunc(func(context.Context, llm.Request) (*llm.Response, error) {
					calls++
					if calls == 1 {
						return mustResponse(toolMsg), nil
					}
					return mustResponse(finalMsg), nil
				}),
			}

			reason, err := r.loop(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(reason).To(Equal(runstate.ReasonCompleted))

			Expect(pre).To(HaveLen(2))
			Expect(pre[0].MessageCount).To(Equal(1)) // seed prompt only
			Expect(pre[1].MessageCount).To(Equal(3)) // + assistant tool turn + tool result

			Expect(post).To(HaveLen(2))
			Expect(post[0].Terminal).To(BeFalse())
			Expect(post[0].ToolCalls).To(HaveLen(1))
			Expect(post[0].ToolCalls[0].Name).To(Equal("echo"))
			Expect(post[1].Terminal).To(BeTrue())
			Expect(post[1].ToolCalls).To(BeEmpty())
		})

		It("fires PostModelCall for a reply truncated at the output cap, with Terminal false", func() {
			truncMsg := `{"id":"m1","type":"message","role":"assistant","model":"m","stop_reason":"max_tokens","content":[{"type":"text","text":"partial"}],"usage":{"input_tokens":10,"output_tokens":5}}`

			var post []PostModelCallInfo
			r := &runner{
				cfg: newCfg(), stats: &RunStats{}, maxIter: 5, events: nopEvents{},
				messages: []llm.Message{userMsg("go")},
				toolSrc:  toolSrcOf(nil),
				hooks: Hooks{
					PostModelCall: func(_ context.Context, in PostModelCallInfo) error {
						post = append(post, in)
						return nil
					},
				},
				provider: providerFunc(func(context.Context, llm.Request) (*llm.Response, error) {
					return mustResponse(truncMsg), nil
				}),
			}

			reason, err := r.loop(context.Background())
			Expect(err).To(HaveOccurred())
			Expect(reason).To(Equal(runstate.ReasonError))

			// The hook still observed the reply, before the truncation branch ended the run.
			Expect(post).To(HaveLen(1))
			Expect(post[0].Terminal).To(BeFalse())
		})

		It("isolates the live conversation from a hook that mutates the reply copy", func() {
			toolMsg := `{"id":"m1","type":"message","role":"assistant","model":"m","stop_reason":"tool_use","content":[{"type":"text","text":"hi"},{"type":"tool_use","id":"toolu_1","name":"echo","input":{"n":1}}],"usage":{"input_tokens":10,"output_tokens":5}}`

			tool := &recordingTool{name: "echo", output: "ran"}
			var calls int
			r := &runner{
				cfg: newCfg(), stats: &RunStats{}, maxIter: 5, events: nopEvents{},
				messages: []llm.Message{userMsg("go")},
				toolSrc:  toolSrcOf(map[string]toolkit.Tool{"echo": tool}),
				hooks: Hooks{
					PostModelCall: func(_ context.Context, in PostModelCallInfo) error {
						// Scribble over every mutable surface of the snapshot; none of it
						// may reach the live conversation or the tool about to run.
						for i := range in.ToolCalls {
							for j := range in.ToolCalls[i].Input {
								in.ToolCalls[i].Input[j] = 'x'
							}
						}
						for _, b := range in.Response.Content {
							if b.Text != nil {
								b.Text.Text = "MUTATED"
							}
							if b.ToolUse != nil {
								for j := range b.ToolUse.Input {
									b.ToolUse.Input[j] = 'y'
								}
							}
						}
						return nil
					},
				},
				provider: providerFunc(func(context.Context, llm.Request) (*llm.Response, error) {
					calls++
					if calls == 1 {
						return mustResponse(toolMsg), nil
					}
					return mustResponse(finalMsg), nil
				}),
			}

			reason, err := r.loop(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(reason).To(Equal(runstate.ReasonCompleted))

			// The tool ran with the original arguments, not the scribbled copy.
			Expect(tool.ranInputs).To(ConsistOf(`{"n":1}`))

			// The live assistant turn is untouched.
			asst := r.messages[1]
			Expect(asst.Role).To(Equal(llm.RoleAssistant))
			var sawText, sawToolUse bool
			for _, b := range asst.Content {
				if b.Text != nil {
					sawText = true
					Expect(b.Text.Text).To(Equal("hi"))
				}
				if b.ToolUse != nil {
					sawToolUse = true
					Expect(string(b.ToolUse.Input)).To(Equal(`{"n":1}`))
				}
			}
			Expect(sawText).To(BeTrue())
			Expect(sawToolUse).To(BeTrue())
		})

		It("aborts before the call when PreModelCall returns an error, counting no LLM call", func() {
			var calls int
			r := &runner{
				cfg: newCfg(), stats: &RunStats{}, maxIter: 5, events: nopEvents{},
				messages: []llm.Message{userMsg("go")},
				toolSrc:  toolSrcOf(nil),
				hooks: Hooks{
					PreModelCall: func(context.Context, PreModelCallInfo) error {
						return errors.New("boom")
					},
				},
				provider: providerFunc(func(context.Context, llm.Request) (*llm.Response, error) {
					calls++
					return mustResponse(finalMsg), nil
				}),
			}

			reason, err := r.loop(context.Background())
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("PreModelCall hook"))
			Expect(reason).To(Equal(runstate.ReasonError))
			Expect(calls).To(Equal(0))
			Expect(r.stats.LlmCalls).To(Equal(int64(0)))
		})

		It("aborts the run when PostModelCall returns an error", func() {
			r := &runner{
				cfg: newCfg(), stats: &RunStats{}, maxIter: 5, events: nopEvents{},
				messages: []llm.Message{userMsg("go")},
				toolSrc:  toolSrcOf(nil),
				hooks: Hooks{
					PostModelCall: func(context.Context, PostModelCallInfo) error {
						return errors.New("boom")
					},
				},
				provider: providerFunc(func(context.Context, llm.Request) (*llm.Response, error) {
					return mustResponse(finalMsg), nil
				}),
			}

			reason, err := r.loop(context.Background())
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("PostModelCall hook"))
			Expect(reason).To(Equal(runstate.ReasonError))
		})
	})

	Describe("lifecycle hooks (TurnEnd/follow-up UserPromptSubmit)", func() {
		newCfg := func() *config.Config {
			cfg := &config.Config{}
			cfg.LLM.Model = "test-model"
			cfg.LLM.Budget.MaxIterations = 10
			cfg.LLM.Budget.CallTimeoutParsed = time.Second
			return cfg
		}

		echoToolMsg := `{"id":"m1","type":"message","role":"assistant","model":"m","stop_reason":"tool_use","content":[{"type":"tool_use","id":"toolu_1","name":"echo","input":{}}],"usage":{"input_tokens":1,"output_tokens":1}}`
		finalMsg := `{"id":"m2","type":"message","role":"assistant","model":"m","stop_reason":"end_turn","content":[{"type":"text","text":"done"}],"usage":{"input_tokens":1,"output_tokens":1}}`

		scriptPrompts := func(conts ...Continuation) func(context.Context) Continuation {
			i := 0
			return func(context.Context) Continuation {
				c := conts[i]
				i++
				return c
			}
		}

		It("fires TurnEnd once per continuation boundary, not per model call", func() {
			var turnEnds []TurnEndInfo
			var submits []UserPromptSubmitInfo
			var calls int
			r := &runner{
				cfg: newCfg(), stats: &RunStats{}, maxIter: 10, events: nopEvents{},
				messages:   []llm.Message{userMsg("go")},
				toolSrc:    toolSrcOf(map[string]toolkit.Tool{"echo": &recordingTool{name: "echo", output: "ran"}}),
				nextPrompt: scriptPrompts(Continuation{Continue: true, Text: "again"}, Continuation{Continue: false}),
				hooks: Hooks{
					TurnEnd: func(_ context.Context, in TurnEndInfo) error {
						turnEnds = append(turnEnds, in)
						return nil
					},
					UserPromptSubmit: func(_ context.Context, in UserPromptSubmitInfo) (UserPromptSubmitResult, error) {
						submits = append(submits, in)
						return UserPromptSubmitResult{}, nil
					},
				},
				provider: providerFunc(func(context.Context, llm.Request) (*llm.Response, error) {
					calls++
					// Turn 1 is a tool turn (two model calls); turn 2 is a single final reply.
					if calls == 1 {
						return mustResponse(echoToolMsg), nil
					}
					return mustResponse(finalMsg), nil
				}),
			}

			reason, err := r.run(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(reason).To(Equal(runstate.ReasonCompleted))

			// Three model calls across two turns, but only two continuation boundaries.
			Expect(calls).To(Equal(3))
			Expect(turnEnds).To(HaveLen(2))
			Expect(turnEnds[0].Reason).To(Equal(runstate.ReasonCompleted))

			// The single follow-up prompt is submitted as a non-initial prompt.
			Expect(submits).To(HaveLen(1))
			Expect(submits[0].Initial).To(BeFalse())
			Expect(submits[0].Text).To(Equal("again"))
		})

		It("denies a follow-up prompt: reopens the input, journals no record for it, and surfaces the reason", func() {
			store, err := runstatefile.NewFileStore(GinkgoT().TempDir())
			Expect(err).NotTo(HaveOccurred())
			id := ksuid.New().String()
			j, err := store.Create(testCtx, id, runstate.MetaRecord{Version: runstate.Version, RunID: id, Prompt: "go", Interactive: true})
			Expect(err).NotTo(HaveOccurred())

			ev := &captureEvents{}
			var calls int
			r := &runner{
				cfg: newCfg(), stats: &RunStats{}, maxIter: 10, events: ev,
				messages:   []llm.Message{userMsg("go")},
				journal:    j,
				seq:        1,
				toolSrc:    toolSrcOf(nil),
				nextPrompt: scriptPrompts(Continuation{Continue: true, Text: "blocked"}, Continuation{Continue: true, Text: "allowed"}, Continuation{Continue: false}),
				hooks: Hooks{
					UserPromptSubmit: func(_ context.Context, in UserPromptSubmitInfo) (UserPromptSubmitResult, error) {
						if in.Text == "blocked" {
							return UserPromptSubmitResult{Deny: true, DenyReason: "nope"}, nil
						}
						return UserPromptSubmitResult{}, nil
					},
				},
				provider: providerFunc(func(context.Context, llm.Request) (*llm.Response, error) {
					calls++
					return mustResponse(finalMsg), nil
				}),
			}

			reason, err := r.run(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(reason).To(Equal(runstate.ReasonSuspended))
			Expect(j.Close()).To(Succeed())

			// Only the initial turn and the allowed follow-up ran; the denied prompt did not.
			Expect(calls).To(Equal(2))

			// The deny reason surfaced to the operator.
			denied := false
			for _, w := range ev.warns {
				if w.Kind == WarnPromptDenied {
					denied = true
					Expect(w.Name).To(Equal("nope"))
				}
			}
			Expect(denied).To(BeTrue())

			// The journal holds the allowed prompt but no dangling record for the denied one.
			rs, err := store.Load(testCtx, id)
			Expect(err).NotTo(HaveOccurred())
			texts := userTexts(rs.Messages)
			Expect(texts).To(ContainElement("allowed"))
			Expect(texts).NotTo(ContainElement("blocked"))
		})

		It("aborts the run when TurnEnd returns an error", func() {
			r := &runner{
				cfg: newCfg(), stats: &RunStats{}, maxIter: 10, events: nopEvents{},
				messages:   []llm.Message{userMsg("go")},
				nextPrompt: scriptPrompts(Continuation{Continue: true, Text: "x"}),
				toolSrc:    toolSrcOf(nil),
				hooks: Hooks{
					TurnEnd: func(context.Context, TurnEndInfo) error {
						return errors.New("boom")
					},
				},
				provider: providerFunc(func(context.Context, llm.Request) (*llm.Response, error) {
					return mustResponse(finalMsg), nil
				}),
			}

			reason, err := r.run(context.Background())
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("TurnEnd hook"))
			Expect(reason).To(Equal(runstate.ReasonError))
		})

		It("aborts the run when a follow-up UserPromptSubmit returns an error", func() {
			r := &runner{
				cfg: newCfg(), stats: &RunStats{}, maxIter: 10, events: nopEvents{},
				messages:   []llm.Message{userMsg("go")},
				nextPrompt: scriptPrompts(Continuation{Continue: true, Text: "x"}),
				toolSrc:    toolSrcOf(nil),
				hooks: Hooks{
					UserPromptSubmit: func(context.Context, UserPromptSubmitInfo) (UserPromptSubmitResult, error) {
						return UserPromptSubmitResult{}, errors.New("boom")
					},
				},
				provider: providerFunc(func(context.Context, llm.Request) (*llm.Response, error) {
					return mustResponse(finalMsg), nil
				}),
			}

			reason, err := r.run(context.Background())
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("UserPromptSubmit hook"))
			Expect(reason).To(Equal(runstate.ReasonError))
		})
	})

	Describe("completePending (partial-turn resume)", func() {
		It("runs only the unanswered tools, reuses the rest, and commits the turn", func() {
			store, err := runstatefile.NewFileStore(GinkgoT().TempDir())
			Expect(err).NotTo(HaveOccurred())
			runID := ksuid.New().String()

			// Journal a run whose assistant turn called two (unknown) tools but
			// only the first was answered before the "crash": a partial batch.
			assistant := llm.Message{
				Role: llm.RoleAssistant,
				Content: []llm.ContentBlock{
					{ToolUse: &llm.ToolUseBlock{ID: "toolu_a", Name: "missing_a", Input: json.RawMessage(`{}`)}},
					{ToolUse: &llm.ToolUseBlock{ID: "toolu_b", Name: "missing_b", Input: json.RawMessage(`{}`)}},
				},
			}

			j, err := store.Create(testCtx, runID, runstate.MetaRecord{Version: runstate.Version, RunID: runID, Prompt: "go"})
			Expect(err).NotTo(HaveOccurred())
			Expect(j.Append(testCtx, 2, runstate.Record{Protocol: runstate.AssistantProtocol, Assistant: &runstate.AssistantRecord{Iteration: 0, Message: assistant}})).To(Succeed())
			Expect(j.Append(testCtx, 3, runstate.Record{Protocol: runstate.ToolResultProtocol, ToolResult: &runstate.ToolResultRecord{ToolUseID: "toolu_a", Result: llm.ToolResultBlock{ToolUseID: "toolu_a", Content: "already done"}}})).To(Succeed())
			Expect(j.Close()).To(Succeed())

			rs, err := store.Load(testCtx, runID)
			Expect(err).NotTo(HaveOccurred())
			Expect(rs.Pending).NotTo(BeNil())

			resumeJ, err := store.Open(testCtx, runID)
			Expect(err).NotTo(HaveOccurred())

			r := &runner{
				stats:    &RunStats{},
				events:   nopEvents{},
				set:      toolSetOf(nil),
				messages: rs.Messages,
				journal:  resumeJ,
				seq:      resumeJ.LastSeq(),
				pending:  rs.Pending,
			}

			before := len(r.messages)
			deferred, err := r.completePending(context.Background())
			Expect(err).ToNot(HaveOccurred())
			Expect(deferred).To(BeFalse())

			// The assistant turn plus a single user results message are committed.
			Expect(r.messages).To(HaveLen(before + 2))
			// Only the unanswered tool executed.
			Expect(r.stats.ToolCalls).To(Equal(int64(1)))
			Expect(resumeJ.Close()).To(Succeed())

			// Re-folding shows the turn fully answered: no pending remains, and
			// both tool results are recorded.
			done, err := store.Load(testCtx, runID)
			Expect(err).NotTo(HaveOccurred())
			Expect(done.Pending).To(BeNil())
			Expect(done.Counters.ToolCalls).To(Equal(int64(2)))
		})
	})

	Describe("interactive continuation", func() {
		newCfg := func() *config.Config {
			cfg := &config.Config{}
			cfg.LLM.Model = "test-model"
			cfg.LLM.Budget.CallTimeoutParsed = time.Second
			cfg.LLM.Budget.MaxIterations = 10
			return cfg
		}
		emptyTools := func(r *runner) {
			r.set = toolSetOf(nil)
			r.toolSrc = NewToolSource(r.set)
		}
		finalMsg := func(text string) string {
			return `{"id":"x","type":"message","role":"assistant","model":"m","stop_reason":"end_turn","content":[{"type":"text","text":"` + text + `"}],"usage":{"input_tokens":1,"output_tokens":1}}`
		}
		toolMsg := `{"id":"m1","type":"message","role":"assistant","model":"m","stop_reason":"tool_use","content":[{"type":"tool_use","id":"toolu_1","name":"missing","input":{}}],"usage":{"input_tokens":10,"output_tokens":5}}`

		It("re-enters the loop with a follow-up and ends on a false continuation", func() {
			var calls, prompts int
			answers := []string{finalMsg("first"), finalMsg("second")}

			r := &runner{
				cfg: newCfg(), stats: &RunStats{}, maxIter: 10, events: nopEvents{},
				messages: []llm.Message{userMsg("go")},
				provider: providerFunc(func(context.Context, llm.Request) (*llm.Response, error) {
					m := answers[calls]
					calls++
					return mustResponse(m), nil
				}),
				nextPrompt: func(context.Context) Continuation {
					prompts++
					if prompts == 1 {
						return Continuation{Text: "tell me more", Continue: true}
					}
					return Continuation{}
				},
			}
			emptyTools(r)

			reason, err := r.run(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(reason).To(Equal(runstate.ReasonCompleted))
			Expect(calls).To(Equal(2))
			Expect(prompts).To(Equal(2))
			Expect(r.stats.LlmCalls).To(Equal(int64(2)))
			// The follow-up became a user turn between the two assistant answers.
			Expect(r.messages).To(HaveLen(4))
			Expect(r.messages[2].Role).To(Equal(llm.RoleUser))
			// The iteration index stayed monotonic across the two turns.
			Expect(r.iter).To(Equal(int64(2)))
		})

		It("clears the conversation on a reset and runs the fresh prompt against the empty context", func() {
			var calls, prompts int
			var seenLens []int
			answers := []string{finalMsg("first"), finalMsg("second")}

			r := &runner{
				cfg: newCfg(), stats: &RunStats{}, maxIter: 10, events: nopEvents{},
				messages: []llm.Message{userMsg("go")},
				provider: providerFunc(func(_ context.Context, p llm.Request) (*llm.Response, error) {
					seenLens = append(seenLens, len(p.Messages))
					m := answers[calls]
					calls++
					return mustResponse(m), nil
				}),
				nextPrompt: func(context.Context) Continuation {
					prompts++
					if prompts == 1 {
						return Continuation{Text: "fresh", Reset: true, Continue: true}
					}
					return Continuation{}
				},
			}
			emptyTools(r)

			reason, err := r.run(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(reason).To(Equal(runstate.ReasonCompleted))
			Expect(calls).To(Equal(2))
			// The first turn saw the original single-message conversation; the reset
			// dropped it, so the second turn ran against an empty context (one user
			// message) rather than the three it would have accumulated without the clear.
			Expect(seenLens).To(Equal([]int{1, 1}))
			Expect(r.messages).To(HaveLen(2))
			Expect(r.messages[0].Role).To(Equal(llm.RoleUser))
		})

		It("reopens the input on a bare reset without running an extra turn", func() {
			var calls, prompts int
			answers := []string{finalMsg("first"), finalMsg("second")}

			r := &runner{
				cfg: newCfg(), stats: &RunStats{}, maxIter: 10, events: nopEvents{},
				messages: []llm.Message{userMsg("go")},
				provider: providerFunc(func(context.Context, llm.Request) (*llm.Response, error) {
					m := answers[calls]
					calls++
					return mustResponse(m), nil
				}),
				nextPrompt: func(context.Context) Continuation {
					prompts++
					switch prompts {
					case 1:
						return Continuation{Reset: true, Continue: true}
					case 2:
						return Continuation{Text: "now", Continue: true}
					default:
						return Continuation{}
					}
				},
			}
			emptyTools(r)

			reason, err := r.run(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(reason).To(Equal(runstate.ReasonCompleted))
			// The bare reset ran no turn: only the initial answer and the post-reset
			// "now" prompt reached the model.
			Expect(calls).To(Equal(2))
			Expect(prompts).To(Equal(3))
			// The cleared context leaves only the post-reset turn behind.
			Expect(r.messages).To(HaveLen(2))
		})

		It("rotates to a fresh checkpoint session on a reset, keeping the previous one resumable", func() {
			store, err := runstatefile.NewFileStore(GinkgoT().TempDir())
			Expect(err).NotTo(HaveOccurred())

			oldID := ksuid.New().String()
			jA, err := store.Create(testCtx, oldID, runstate.MetaRecord{Version: runstate.Version, RunID: oldID, Prompt: "go", Interactive: true})
			Expect(err).NotTo(HaveOccurred())

			var newID string
			newSession := func(ctx context.Context, prompt string) (runstate.Journal, string, error) {
				newID = ksuid.New().String()
				meta := runstate.MetaRecord{Version: runstate.Version, RunID: newID, Prompt: prompt, Interactive: true}
				j, e := store.Create(ctx, newID, meta)
				if e != nil {
					return nil, "", e
				}
				return j, newID, nil
			}

			var calls, prompts int
			rec := &rotateRecorder{}
			r := &runner{
				cfg: newCfg(), stats: &RunStats{}, maxIter: 10, events: rec,
				messages:   []llm.Message{userMsg("go")},
				journal:    jA,
				seq:        1,
				sessionID:  oldID,
				newSession: newSession,
				provider: providerFunc(func(context.Context, llm.Request) (*llm.Response, error) {
					calls++
					return mustResponse(finalMsg("answer")), nil
				}),
				nextPrompt: func(context.Context) Continuation {
					prompts++
					if prompts == 1 {
						return Continuation{Text: "fresh", Reset: true, Continue: true}
					}
					return Continuation{}
				},
			}
			emptyTools(r)

			reason, err := r.run(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(reason).To(Equal(runstate.ReasonSuspended))
			Expect(calls).To(Equal(2))

			// The reset rotated onto a brand-new session and reported the previous one.
			Expect(newID).NotTo(BeEmpty())
			Expect(r.sessionID).To(Equal(newID))
			Expect(rec.prevIDs).To(Equal([]string{oldID}))

			// The previous session is finalized as suspended (never completed), so it stays
			// resumable, with just its single pre-reset turn recorded.
			old, err := store.Load(testCtx, oldID)
			Expect(err).NotTo(HaveOccurred())
			Expect(old.Completed()).To(BeFalse())
			Expect(old.Counters.LlmCalls).To(Equal(int64(1)))

			// The fresh session holds only the post-reset conversation: its "fresh" prompt (the
			// new Meta.Prompt) and the one answer, not the pre-reset "go" turn.
			fresh, err := store.Load(testCtx, newID)
			Expect(err).NotTo(HaveOccurred())
			Expect(fresh.Counters.LlmCalls).To(Equal(int64(1)))
			Expect(fresh.Messages).To(HaveLen(2))
			Expect(fresh.Messages[0].Role).To(Equal(llm.RoleUser))
		})

		It("defers a bare reset until the next prompt before rotating (checkpointed)", func() {
			store, err := runstatefile.NewFileStore(GinkgoT().TempDir())
			Expect(err).NotTo(HaveOccurred())

			oldID := ksuid.New().String()
			jA, err := store.Create(testCtx, oldID, runstate.MetaRecord{Version: runstate.Version, RunID: oldID, Prompt: "go", Interactive: true})
			Expect(err).NotTo(HaveOccurred())

			var newID string
			var rotateCalls int
			newSession := func(ctx context.Context, prompt string) (runstate.Journal, string, error) {
				rotateCalls++
				newID = ksuid.New().String()
				j, e := store.Create(ctx, newID, runstate.MetaRecord{Version: runstate.Version, RunID: newID, Prompt: prompt, Interactive: true})
				if e != nil {
					return nil, "", e
				}
				return j, newID, nil
			}

			var calls, prompts int
			r := &runner{
				cfg: newCfg(), stats: &RunStats{}, maxIter: 10, events: nopEvents{},
				messages:   []llm.Message{userMsg("go")},
				journal:    jA,
				seq:        1,
				sessionID:  oldID,
				newSession: newSession,
				provider: providerFunc(func(context.Context, llm.Request) (*llm.Response, error) {
					calls++
					return mustResponse(finalMsg("answer")), nil
				}),
				nextPrompt: func(context.Context) Continuation {
					prompts++
					switch prompts {
					case 1:
						return Continuation{Reset: true, Continue: true}
					case 2:
						return Continuation{Text: "typed", Continue: true}
					default:
						return Continuation{}
					}
				},
			}
			emptyTools(r)

			reason, err := r.run(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(reason).To(Equal(runstate.ReasonSuspended))

			// The bare reset ran no turn and did not rotate; rotation happened once, on the next
			// prompt, so only the "go" and "typed" turns reached the model.
			Expect(calls).To(Equal(2))
			Expect(rotateCalls).To(Equal(1))
			Expect(r.sessionID).To(Equal(newID))

			// The previous session kept just its pre-reset turn and stays resumable.
			old, err := store.Load(testCtx, oldID)
			Expect(err).NotTo(HaveOccurred())
			Expect(old.Completed()).To(BeFalse())
			Expect(old.Counters.LlmCalls).To(Equal(int64(1)))
		})

		It("runs on in the current session and warns when rotation fails", func() {
			store, err := runstatefile.NewFileStore(GinkgoT().TempDir())
			Expect(err).NotTo(HaveOccurred())

			oldID := ksuid.New().String()
			jA, err := store.Create(testCtx, oldID, runstate.MetaRecord{Version: runstate.Version, RunID: oldID, Prompt: "go", Interactive: true})
			Expect(err).NotTo(HaveOccurred())

			newSession := func(context.Context, string) (runstate.Journal, string, error) {
				return nil, "", errors.New("store unavailable")
			}

			var calls, prompts int
			we := &warnRecorder{}
			r := &runner{
				cfg: newCfg(), stats: &RunStats{}, maxIter: 10, events: we,
				messages:   []llm.Message{userMsg("go")},
				journal:    jA,
				seq:        1,
				sessionID:  oldID,
				newSession: newSession,
				provider: providerFunc(func(context.Context, llm.Request) (*llm.Response, error) {
					calls++
					return mustResponse(finalMsg("answer")), nil
				}),
				nextPrompt: func(context.Context) Continuation {
					prompts++
					if prompts == 1 {
						return Continuation{Text: "fresh", Reset: true, Continue: true}
					}
					return Continuation{}
				},
			}
			emptyTools(r)

			reason, err := r.run(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(reason).To(Equal(runstate.ReasonSuspended))
			Expect(calls).To(Equal(2))

			// Rotation failed, so the reset was abandoned: the session id is unchanged and a
			// warning was raised.
			Expect(r.sessionID).To(Equal(oldID))
			Expect(we.has(WarnSessionRotate)).To(BeTrue())

			// The turn ran on in the original session, which keeps both turns and stays consistent.
			old, err := store.Load(testCtx, oldID)
			Expect(err).NotTo(HaveOccurred())
			Expect(old.Completed()).To(BeFalse())
			Expect(old.Counters.LlmCalls).To(Equal(int64(2)))
		})

		It("recovers from a max-iterations turn, warning and re-prompting", func() {
			we := &warnRecorder{}
			var prompts int

			r := &runner{
				cfg: newCfg(), stats: &RunStats{}, maxIter: 1, events: we,
				messages: []llm.Message{userMsg("go")},
				provider: providerFunc(func(context.Context, llm.Request) (*llm.Response, error) {
					return mustResponse(toolMsg), nil
				}),
				nextPrompt: func(context.Context) Continuation {
					prompts++
					return Continuation{}
				},
			}
			emptyTools(r)

			reason, err := r.run(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(reason).To(Equal(runstate.ReasonCompleted))
			Expect(prompts).To(Equal(1))
			Expect(we.has(WarnMaxIterInteractive)).To(BeTrue())
		})

		It("returns to the input bar after a turn error, warns, and folds the retry into the dangling turn", func() {
			we := &warnRecorder{}
			var calls, prompts int

			r := &runner{
				cfg: newCfg(), stats: &RunStats{}, maxIter: 10, events: we,
				messages: []llm.Message{userMsg("go")},
				provider: providerFunc(func(context.Context, llm.Request) (*llm.Response, error) {
					calls++
					if calls == 1 {
						return nil, errors.New("llm call: context deadline exceeded")
					}
					return mustResponse(finalMsg("recovered")), nil
				}),
				nextPrompt: func(context.Context) Continuation {
					prompts++
					if prompts == 1 {
						return Continuation{Text: "try again", Continue: true}
					}
					return Continuation{}
				},
			}
			emptyTools(r)

			reason, err := r.run(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(reason).To(Equal(runstate.ReasonCompleted))
			Expect(calls).To(Equal(2))
			Expect(prompts).To(Equal(2))
			Expect(we.has(WarnTurnErrorInteractive)).To(BeTrue())
			// The failed turn left "go" dangling with no assistant reply, so the retry
			// folded into it rather than adding a second user message in a row: one user
			// turn carrying both texts, then the recovered assistant answer.
			Expect(r.messages).To(HaveLen(2))
			Expect(r.messages[0].Role).To(Equal(llm.RoleUser))
			Expect(r.messages[0].Content).To(HaveLen(2))
			Expect(r.messages[1].Role).To(Equal(llm.RoleAssistant))
		})

		It("journals interactive follow-ups and suspends a checkpointed chat on a clean end", func() {
			store, err := runstatefile.NewFileStore(GinkgoT().TempDir())
			Expect(err).NotTo(HaveOccurred())
			id := ksuid.New().String()
			j, err := store.Create(testCtx, id, runstate.MetaRecord{Version: runstate.Version, RunID: id, Prompt: "go", Interactive: true})
			Expect(err).NotTo(HaveOccurred())

			var calls, prompts int
			answers := []string{finalMsg("first"), finalMsg("second")}
			r := &runner{
				cfg: newCfg(), stats: &RunStats{}, maxIter: 10, events: nopEvents{},
				messages: []llm.Message{userMsg("go")},
				journal:  j, seq: 1,
				provider: providerFunc(func(context.Context, llm.Request) (*llm.Response, error) {
					m := answers[calls]
					calls++
					return mustResponse(m), nil
				}),
				nextPrompt: func(context.Context) Continuation {
					prompts++
					if prompts == 1 {
						return Continuation{Text: "more", Continue: true}
					}
					return Continuation{}
				},
			}
			emptyTools(r)

			reason, err := r.run(context.Background())
			Expect(err).NotTo(HaveOccurred())
			// A clean end on a checkpointed chat suspends (resumable), never completes.
			Expect(reason).To(Equal(runstate.ReasonSuspended))
			Expect(j.Close()).To(Succeed())

			rs, err := store.Load(testCtx, id)
			Expect(err).NotTo(HaveOccurred())
			Expect(rs.Completed()).To(BeFalse())
			Expect(rs.Interactive).To(BeTrue())
			// prompt, assistant(first), user(follow-up), assistant(second)
			Expect(rs.Messages).To(HaveLen(4))
			Expect(rs.Messages[2].Role).To(Equal(llm.RoleUser))
			Expect(rs.NextIteration).To(Equal(int64(2)))
		})

		It("resumes a chat at the input boundary without a spurious LLM call", func() {
			var calls, prompts int
			r := &runner{
				cfg: newCfg(), stats: &RunStats{}, maxIter: 12, events: nopEvents{},
				startIter:             2,
				resumeAtInputBoundary: true,
				// The restored conversation rests on an assistant turn awaiting a follow-up.
				messages: []llm.Message{
					userMsg("go"),
					assistantTextMsg("answer"),
				},
				provider: providerFunc(func(context.Context, llm.Request) (*llm.Response, error) {
					calls++
					return mustResponse(finalMsg("follow-up answer")), nil
				}),
				nextPrompt: func(context.Context) Continuation {
					prompts++
					if prompts == 1 {
						return Continuation{Text: "next", Continue: true}
					}
					return Continuation{}
				},
			}
			emptyTools(r)

			reason, err := r.run(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(reason).To(Equal(runstate.ReasonCompleted))
			// Exactly one LLM call, the follow-up turn: the initial loop was skipped so no
			// spurious call was made against the already-complete conversation.
			Expect(calls).To(Equal(1))
			Expect(prompts).To(Equal(2))
			// Iteration numbering continued from the resumed position, not restarted.
			Expect(r.iter).To(Equal(int64(3)))
		})

		It("ends the session, warns, and does not loop when journaling a follow-up fails", func() {
			store, err := runstatefile.NewFileStore(GinkgoT().TempDir())
			Expect(err).NotTo(HaveOccurred())
			id := ksuid.New().String()
			j, err := store.Create(testCtx, id, runstate.MetaRecord{Version: runstate.Version, RunID: id, Prompt: "go", Interactive: true})
			Expect(err).NotTo(HaveOccurred())

			we := &warnRecorder{}
			var prompts int
			r := &runner{
				cfg: newCfg(), stats: &RunStats{}, maxIter: 10, events: we,
				messages: []llm.Message{userMsg("go")},
				journal:  failOnUserJournal{Journal: j}, seq: 1,
				provider: providerFunc(func(context.Context, llm.Request) (*llm.Response, error) {
					return mustResponse(finalMsg("answer")), nil
				}),
				nextPrompt: func(context.Context) Continuation {
					prompts++
					return Continuation{Text: "a follow-up", Continue: true}
				},
			}
			emptyTools(r)

			reason, err := r.run(context.Background())
			Expect(reason).To(Equal(runstate.ReasonError))
			Expect(err).To(MatchError(ContainSubstring("disk full")))
			Expect(we.has(WarnJournalUser)).To(BeTrue())
			// The failed emit must break the continuation loop, not re-offer the bar (which
			// would fail again forever); the operator was prompted exactly once.
			Expect(prompts).To(Equal(1))
			Expect(j.Close()).To(Succeed())
		})

		It("surfaces an abort at the input boundary as an error, not a clean end", func() {
			ctx, cancel := context.WithCancel(context.Background())

			r := &runner{
				cfg: newCfg(), stats: &RunStats{}, maxIter: 10, events: nopEvents{},
				messages: []llm.Message{userMsg("go")},
				provider: providerFunc(func(context.Context, llm.Request) (*llm.Response, error) {
					return mustResponse(finalMsg("done")), nil
				}),
				nextPrompt: func(context.Context) Continuation {
					// The operator aborted (Ctrl-C) while the field was up: ctx is
					// canceled and the prompt reports no continuation.
					cancel()
					return Continuation{}
				},
			}
			emptyTools(r)

			reason, err := r.run(ctx)
			Expect(reason).To(Equal(runstate.ReasonError))
			Expect(err).To(MatchError(context.Canceled))
		})
	})

	Describe("cache accounting", func() {
		emptyTools := func(r *runner) {
			r.set = toolSetOf(nil)
			r.toolSrc = NewToolSource(r.set)
		}

		It("flows the cache split into stats, the journal, folded counters and the budget", func() {
			store, err := runstatefile.NewFileStore(GinkgoT().TempDir())
			Expect(err).NotTo(HaveOccurred())
			id := ksuid.New().String()

			cfg := &config.Config{}
			cfg.LLM.Model = "test-model"
			cfg.LLM.Budget.CallTimeoutParsed = time.Second
			// A budget just above the first turn's uncached input+output but below the full
			// throughput (which the cache tiers push over) proves the check counts all four.
			cfg.LLM.Budget.MaxTokens = 50

			// A tool turn whose usage carries a cache read and a cache write, so the split is
			// non-zero on a turn that continues (the budget check runs before the next turn).
			cachedTurn := `{"id":"m1","type":"message","role":"assistant","model":"m","stop_reason":"tool_use","content":[{"type":"tool_use","id":"toolu_1","name":"missing","input":{}}],"usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":100,"cache_creation_input_tokens":40}}`

			j, err := store.Create(testCtx, id, runstate.MetaRecord{Version: runstate.Version, RunID: id, Prompt: "go"})
			Expect(err).NotTo(HaveOccurred())

			r := &runner{
				cfg: cfg, stats: &RunStats{}, maxIter: 10, maxTokens: cfg.LLM.Budget.MaxTokens, events: nopEvents{},
				messages: []llm.Message{userMsg("go")},
				journal:  j,
				seq:      1,
				provider: providerFunc(func(context.Context, llm.Request) (*llm.Response, error) {
					return mustResponse(cachedTurn), nil
				}),
			}
			emptyTools(r)

			reason, err := r.run(context.Background())
			Expect(err).To(HaveOccurred())
			// input(10)+output(5)+read(100)+write(40) = 155 >= 50, so the budget stops it
			// before a second turn; uncached-only (15) would not have.
			Expect(reason).To(Equal(runstate.ReasonBudget))
			Expect(j.Close()).To(Succeed())

			// RunStats carries the split as first-class fields.
			Expect(r.stats.InTokens).To(Equal(int64(10)))
			Expect(r.stats.CacheReadTokens).To(Equal(int64(100)))
			Expect(r.stats.CacheCreateTokens).To(Equal(int64(40)))

			// The journaled assistant record carries it, and Fold sums it into the counters
			// that seed a resumed run, so all four stay consistent across a suspend.
			rs, err := store.Load(testCtx, id)
			Expect(err).NotTo(HaveOccurred())
			Expect(rs.Counters.InTokens).To(Equal(int64(10)))
			Expect(rs.Counters.OutTokens).To(Equal(int64(5)))
			Expect(rs.Counters.CacheReadTokens).To(Equal(int64(100)))
			Expect(rs.Counters.CacheCreateTokens).To(Equal(int64(40)))
		})
	})
})

var _ = Describe("Run tool availability guard", func() {
	// The guard must count every tool source the model can address (application,
	// built-in and remote), not just the filtered application tools; otherwise a run
	// whose only tools are native (for example knowledge_search) is wrongly aborted.

	// emptyAppCfg points at a fake fisk application that introspects to zero
	// commands, so LoadTools succeeds with an empty tool set and the run reaches the
	// availability guard rather than failing earlier in introspection.
	emptyAppCfg := func() *config.Config {
		dir := GinkgoT().TempDir()
		app := filepath.Join(dir, "fakeapp")
		Expect(os.WriteFile(app, []byte("#!/bin/sh\necho '{}'\n"), 0o755)).To(Succeed())

		cfg := &config.Config{ApplicationPath: app}
		cfg.LLM.Model = "test-model"
		cfg.LLM.Budget.MaxIterations = 1
		return cfg
	}

	It("aborts when no application, built-in, or remote tool is available", func() {
		cfg := emptyAppCfg()

		_, err := Run(context.Background(), Options{Config: cfg, ConfigFile: "agent.yaml"}, nopEvents{}, nil)
		Expect(err).To(MatchError(ContainSubstring("no tools available after filtering")))
	})

	It("aborts with an application-less message when no application_path and no tools are set", func() {
		cfg := &config.Config{}
		cfg.LLM.Model = "test-model"
		cfg.LLM.Budget.MaxIterations = 1

		_, err := Run(context.Background(), Options{Config: cfg, ConfigFile: "agent.yaml"}, nopEvents{}, nil)
		Expect(err).To(MatchError(ContainSubstring("this agent wraps no application")))
		Expect(err).To(MatchError(ContainSubstring(`in "agent.yaml"`)))
	})

	// An embedder that built its configuration in Go read no file, so the settings to
	// change are named without a file to change them in.
	It("names no file when the caller read none", func() {
		cfg := &config.Config{}
		cfg.LLM.Model = "test-model"
		cfg.LLM.Budget.MaxIterations = 1

		_, err := Run(context.Background(), Options{Config: cfg}, nopEvents{}, nil)
		Expect(err).To(MatchError(ContainSubstring("this agent wraps no application")))
		Expect(err.Error()).To(HaveSuffix("mcp_clients"))
	})

	It("proceeds past the guard when only a native tool (knowledge_search) is enabled", func() {
		cfg := emptyAppCfg()
		cfg.Harness.RAG = &config.RAGConfig{Enabled: true, Directory: GinkgoT().TempDir()}

		// The guard now passes on the knowledge_search built-in, so the run continues
		// to the model call, which fails fast against an unreachable local endpoint.
		// The point is only that it is not the "no tools" abort.
		opts := Options{
			Config:     cfg,
			ConfigFile: "agent.yaml",
			APIKey:     "test",
			BaseURL:    "http://127.0.0.1:1",
		}
		_, err := Run(context.Background(), opts, nopEvents{}, nil)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).ToNot(ContainSubstring("no tools available after filtering"))
	})
})

var _ = Describe("Run with an injected Options.Provider", func() {
	// ragAppCfg points at a fake fisk application that introspects to zero commands
	// and enables knowledge, so the run gets past the tool-availability guard on the
	// knowledge_search built-in and reaches the model call with the injected provider.
	ragAppCfg := func() *config.Config {
		dir := GinkgoT().TempDir()
		app := filepath.Join(dir, "fakeapp")
		Expect(os.WriteFile(app, []byte("#!/bin/sh\necho '{}'\n"), 0o755)).To(Succeed())

		cfg := &config.Config{ApplicationPath: app}
		cfg.LLM.Model = "test-model"
		cfg.LLM.Budget.MaxIterations = 1
		cfg.Harness.RAG = &config.RAGConfig{Enabled: true, Directory: GinkgoT().TempDir()}
		return cfg
	}

	It("uses the injected provider and never consults the registry", func() {
		cfg := ragAppCfg()
		// An unregistered provider name proves the registry is bypassed: the nil-provider
		// path would fail NewProvider with "unknown llm provider" before any model call.
		cfg.LLM.Provider = "definitely-not-a-registered-backend"

		var called atomic.Bool
		provider := providerFunc(func(context.Context, llm.Request) (*llm.Response, error) {
			called.Store(true)
			return mustResponse(`{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"done"}]}`), nil
		})

		opts := Options{Config: cfg, ConfigFile: "agent.yaml", Provider: provider}
		res, err := Run(context.Background(), opts, nopEvents{}, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(called.Load()).To(BeTrue())
		Expect(res.Reason).To(Equal(runstate.ReasonCompleted))
	})
})

// failCloseJournal is a runstate.Journal whose Close fails, to exercise the
// journal-close warning routing. Only Close is called by closeJournal.
type failCloseJournal struct {
	runstate.Journal
}

func (failCloseJournal) Close() error { return errors.New("disk gone") }

var _ = Describe("closeJournal", func() {
	It("routes a journal close failure through events.Warn rather than raw stderr", func() {
		ev := &warnRecorder{}
		closeJournal(failCloseJournal{}, ev)
		Expect(ev.has(WarnJournalClose)).To(BeTrue())
	})
})

var _ = Describe("cloneResponse", func() {
	// The clone every PostModelCall hook receives is a JSON round-trip over a struct
	// carrying no tags, so it adapts to a new field for free. That is only true while
	// the fields stay exported and untagged: a json:"-" added later would drop them from
	// the copy while the live conversation kept them, so a hook would see a reply that
	// silently differs from the one the run is using.
	It("carries the response id and model through the copy", func() {
		dup, err := cloneResponse(llm.Response{
			ID:         "msg_01ABC",
			Model:      "claude-sonnet-5-20260101",
			StopReason: llm.StopEndTurn,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(dup.ID).To(Equal("msg_01ABC"))
		Expect(dup.Model).To(Equal("claude-sonnet-5-20260101"))
	})
})

var _ = Describe("validateCallerDir", func() {
	It("accepts an empty value as inherit-today's-behavior", func() {
		Expect(validateCallerDir("tool_work_dir", "")).To(Succeed())
	})

	It("rejects a relative path, naming the option", func() {
		err := validateCallerDir("tool_work_dir", "rel/dir")
		Expect(err).To(MatchError(ContainSubstring("tool_work_dir must be an absolute path")))
	})

	It("rejects a missing directory", func() {
		err := validateCallerDir("tool_work_dir", filepath.Join(GinkgoT().TempDir(), "absent"))
		Expect(err).To(MatchError(ContainSubstring("does not exist")))
	})

	It("rejects a path that is not a directory", func() {
		f := filepath.Join(GinkgoT().TempDir(), "afile")
		Expect(os.WriteFile(f, []byte("x"), 0o600)).To(Succeed())
		err := validateCallerDir("tool_work_dir", f)
		Expect(err).To(MatchError(ContainSubstring("is not a directory")))
	})

	It("accepts an existing absolute directory", func() {
		Expect(validateCallerDir("tool_work_dir", GinkgoT().TempDir())).To(Succeed())
	})
})

var _ = Describe("resolveMaxOutputTokens", func() {
	It("Should use the default when max_output_tokens is unset and thinking is off", func() {
		cfg := &config.Config{}
		Expect(resolveMaxOutputTokens(cfg, false)).To(Equal(int64(defaultMaxOutputTokens)))
	})

	It("Should raise the default for thinking when max_output_tokens is unset", func() {
		cfg := &config.Config{}
		Expect(resolveMaxOutputTokens(cfg, true)).To(Equal(int64(thinkingMaxOutputTokens)))
	})

	It("Should let an explicit max_output_tokens win over the default", func() {
		cfg := &config.Config{}
		cfg.LLM.Budget.MaxOutputTokens = 4096
		Expect(resolveMaxOutputTokens(cfg, false)).To(Equal(int64(4096)))
	})

	It("Should let an explicit max_output_tokens win over the thinking increase", func() {
		cfg := &config.Config{}
		cfg.LLM.Budget.MaxOutputTokens = 4096
		Expect(resolveMaxOutputTokens(cfg, true)).To(Equal(int64(4096)))
	})
})

var _ = Describe("toolSearchDegradation", func() {
	supports := llm.Caps{SupportsToolSearch: true}
	noSupport := llm.Caps{SupportsToolSearch: false}

	It("Should not warn when the provider supports tool search", func() {
		Expect(toolSearchDegradation(ToolSearchThreshold, supports, true)).To(BeNil())
	})

	It("Should not warn when the set is below the threshold", func() {
		Expect(toolSearchDegradation(ToolSearchThreshold-1, noSupport, true)).To(BeNil())
	})

	It("Should not warn when the operator disabled tool search", func() {
		Expect(toolSearchDegradation(ToolSearchThreshold, noSupport, false)).To(BeNil())
	})

	It("Should report the provider-unsupported cause when the provider cannot do tool search", func() {
		w := toolSearchDegradation(ToolSearchThreshold, noSupport, true)
		Expect(w).NotTo(BeNil())
		Expect(w.Kind).To(Equal(WarnToolSearchUnsupported))
		Expect(w.Count).To(Equal(ToolSearchThreshold))
	})
})

var _ = Describe("the conversation token budget", func() {
	// Thinking is documented as a subset of the output rather than an addition to it, so
	// a cap that added it would stop a reasoning model at roughly half its allowance.
	It("Should count the four throughput fields and not thinking", func() {
		r := &runner{maxTokens: 100, stats: &RunStats{
			InTokens: 10, OutTokens: 5, CacheReadTokens: 20, CacheCreateTokens: 4, ThinkingTokens: 1000,
		}}

		Expect(r.tokensSpent()).To(Equal(int64(39)))
		Expect(r.overBudget()).To(BeFalse())
	})

	It("Should treat a cap of zero or less as no bound", func() {
		r := &runner{maxTokens: 0, stats: &RunStats{InTokens: 1e9}}
		Expect(r.overBudget()).To(BeFalse())

		r.maxTokens = -1
		Expect(r.overBudget()).To(BeFalse())
	})

	It("Should be over its budget at the cap rather than past it", func() {
		r := &runner{maxTokens: 40, stats: &RunStats{
			InTokens: 10, OutTokens: 5, CacheReadTokens: 20, CacheCreateTokens: 4,
		}}

		Expect(r.overBudget()).To(BeFalse())

		r.stats.OutTokens = 6
		Expect(r.overBudget()).To(BeTrue())
	})

	// A rotation opens a new journal, which is a new conversation with an allowance of
	// its own. The stats keep climbing to report the whole sitting, so the base is what
	// separates the two.
	It("Should measure the current conversation rather than the sitting", func() {
		r := &runner{maxTokens: 100, budgetBase: 500, stats: &RunStats{
			InTokens: 500, OutTokens: 20,
		}}

		Expect(r.tokensSpent()).To(Equal(int64(20)))
		Expect(r.overBudget()).To(BeFalse())
	})

	It("Should name both numbers and the key that raises it", func() {
		r := &runner{maxTokens: 100, stats: &RunStats{InTokens: 150}}

		Expect(r.budgetError()).To(MatchError(ContainSubstring("processed 150 of its 100 token budget")))
		Expect(r.budgetError()).To(MatchError(ContainSubstring("llm.budget.max_tokens")))
	})

	// The refusal has to happen above the append and the journal write, or the
	// conversation keeps a user message the model never saw and the next turn merges
	// with a prompt nobody answered.
	It("Should refuse a follow-up turn without journaling its prompt", func() {
		store, err := runstatefile.NewFileStore(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())
		id := ksuid.New().String()

		j, err := store.Create(testCtx, id, runstate.MetaRecord{Version: runstate.Version, RunID: id, Prompt: "go"})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { Expect(j.Close()).To(Succeed()) })

		cfg := &config.Config{}
		cfg.LLM.Budget.MaxIterations = 10

		r := &runner{
			cfg: cfg, maxTokens: 100, events: nopEvents{}, journal: j, seq: 1,
			stats:    &RunStats{InTokens: 200},
			followUp: "and what about the second one",
			messages: []llm.Message{userMsg("go")},
			toolSrc:  toolSrcOf(nil),
			provider: providerFunc(func(context.Context, llm.Request) (*llm.Response, error) {
				Fail("the model must not be called for a turn that is refused")

				return nil, nil
			}),
		}

		reason, err := r.followUpTurn(context.Background())
		Expect(reason).To(Equal(runstate.ReasonBudget))
		Expect(err).To(MatchError(ContainSubstring("token budget")))

		// The caller is told the prompt did not land, and the journal agrees.
		Expect(r.followUpTaken).To(BeFalse())
		Expect(r.messages).To(HaveLen(1), "the prompt was not appended to the conversation")

		recs, err := j.Records(testCtx)
		Expect(err).NotTo(HaveOccurred())
		for _, rec := range recs {
			Expect(rec.Protocol).ToNot(Equal(runstate.UserProtocol), "no user record was written")
		}
	})
})

var _ = Describe("HumanPaced", func() {
	// A conversation whose next turn is a person's to send re-sends its whole history at
	// their pace, so the provider needs to know that before it chooses how long to keep
	// the cache. Under model B each turn is a run of its own with no continuation loop,
	// so NextPrompt being nil says nothing about whether another turn is coming, and
	// keying the decision on it alone silently picked the short lifetime for every
	// terminal conversation.
	It("Should tell the provider a conversation is paced by somebody", func() {
		r := &runner{interactive: false, humanPaced: true}
		Expect(r.interactive || r.humanPaced).To(BeTrue())

		r = &runner{interactive: true, humanPaced: false}
		Expect(r.interactive || r.humanPaced).To(BeTrue(), "a run holding its own loop still counts")

		r = &runner{interactive: false, humanPaced: false}
		Expect(r.interactive || r.humanPaced).To(BeFalse(), "a one-shot job re-sends nothing")
	})

	It("Should reach the request the provider is given", func() {
		cfg := &config.Config{}
		cfg.LLM.Model = "test-model"
		cfg.LLM.Budget.CallTimeoutParsed = time.Second

		var seen []bool
		r := &runner{
			cfg: cfg, stats: &RunStats{}, maxIter: 2, events: nopEvents{},
			humanPaced: true,
			messages:   []llm.Message{userMsg("go")},
			toolSrc:    toolSrcOf(nil),
			provider: providerFunc(func(_ context.Context, req llm.Request) (*llm.Response, error) {
				seen = append(seen, req.Interactive)

				return mustResponse(`{"id":"m1","type":"message","role":"assistant","model":"m","stop_reason":"end_turn","content":[{"type":"text","text":"done"}],"usage":{"input_tokens":1,"output_tokens":1}}`), nil
			}),
		}

		_, err := r.run(context.Background())
		Expect(err).ToNot(HaveOccurred())
		Expect(seen).To(HaveExactElements(true))
	})
})

// telemetry declares its own terminal reasons and tool kinds because it imports nothing
// from the rest of this tree, so these two functions are the single place the sets meet.
// A value neither of them recognizes reaches a metric label, where one distinct string
// is one time series for the life of the process, so both fall back to a fixed member
// rather than passing the text through.
var _ = Describe("the telemetry vocabulary mapping", func() {
	DescribeTable("should map a run path terminal reason onto telemetry's own",
		func(reason runstate.TerminalReason, want telemetry.TerminalReason) {
			Expect(telemetryReason(reason)).To(Equal(want))
		},
		Entry("completed", runstate.ReasonCompleted, telemetry.TerminalCompleted),
		Entry("suspended", runstate.ReasonSuspended, telemetry.TerminalSuspended),
		Entry("error", runstate.ReasonError, telemetry.TerminalError),
		Entry("budget", runstate.ReasonBudget, telemetry.TerminalBudget),
		Entry("max iterations", runstate.ReasonMaxIterations, telemetry.TerminalMaxIterations),
		Entry("a reason this build does not know", runstate.TerminalReason("preempted"), telemetry.TerminalOther),
		Entry("no reason at all", runstate.TerminalReason(""), telemetry.TerminalOther),
	)

	DescribeTable("should map a tool provider kind onto telemetry's own",
		func(kind toolkit.Kind, want telemetry.ToolKind) {
			Expect(telemetryToolKind(kind)).To(Equal(want))
		},
		Entry("application", toolkit.KindApplication, telemetry.ToolKindApplication),
		Entry("builtin", toolkit.KindBuiltin, telemetry.ToolKindBuiltin),
		Entry("remote", toolkit.KindRemote, telemetry.ToolKindRemote),
		Entry("custom", toolkit.KindCustom, telemetry.ToolKindCustom),
		Entry("mcp", toolkit.KindMCP, telemetry.ToolKindMCP),
		Entry("unknown", toolkit.KindUnknown, telemetry.ToolKindUnknown),
		Entry("a kind this build does not know", toolkit.Kind(99), telemetry.ToolKindUnknown),
	)

	// Every mapped member renders what toolkit.Kind.String already wrote, so a build that
	// starts mapping does not move the kind token operators group by.
	DescribeTable("should render the token toolkit already wrote for a kind",
		func(kind toolkit.Kind) {
			Expect(telemetryToolKind(kind).String()).To(Equal(kind.String()))
		},
		Entry("application", toolkit.KindApplication),
		Entry("builtin", toolkit.KindBuiltin),
		Entry("remote", toolkit.KindRemote),
		Entry("custom", toolkit.KindCustom),
		Entry("mcp", toolkit.KindMCP),
		Entry("unknown", toolkit.KindUnknown),
	)

	// A crash leaves the reason unset on purpose, so runOutcome names the half of the run
	// it stopped in rather than exporting an empty label.
	DescribeTable("should name the half of the run a crash stopped in",
		func(reachedRunner bool, want telemetry.TerminalReason) {
			out := runOutcome(&Result{}, errors.New("boom"), reachedRunner, nil, 0, 0, 0)
			Expect(out.TerminalReason).To(Equal(want))
		},
		Entry("before the loop", false, telemetry.TerminalSetupFailed),
		Entry("inside the loop", true, telemetry.TerminalError),
	)

	It("should carry a run's own reason through", func() {
		out := runOutcome(&Result{Reason: runstate.ReasonMaxIterations}, nil, true, nil, 0, 0, 0)
		Expect(out.TerminalReason).To(Equal(telemetry.TerminalMaxIterations))
	})
})
