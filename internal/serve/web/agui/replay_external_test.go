//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agui_test

import (
	"encoding/json"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/serve/web"
)

// folded is a stored conversation as the store hands one back: the records a run
// journals, folded the way a load folds them, so a spec replays a shape a run produced
// rather than one it wrote by hand.
func folded(prompt string, records ...runstate.Record) *runstate.RunState {
	GinkgoHelper()

	all := []runstate.Record{{Seq: 1, Protocol: runstate.MetaProtocol, Meta: &runstate.MetaRecord{
		Version: runstate.Version,
		RunID:   "w-abc",
		Prompt:  prompt,
	}}}

	for i, r := range records {
		r.Seq = uint64(i + 2)
		all = append(all, r)
	}

	rs, err := runstate.Fold(all)
	Expect(err).ToNot(HaveOccurred())

	return rs
}

// assistant is one assistant turn of the blocks it produced.
func assistant(blocks ...llm.ContentBlock) runstate.Record {
	return runstate.Record{Protocol: runstate.AssistantProtocol, Assistant: &runstate.AssistantRecord{
		Message: llm.Message{Role: llm.RoleAssistant, Content: blocks},
	}}
}

// typed is a turn the person typed after the conversation had started.
func typed(text string) runstate.Record {
	return runstate.Record{Protocol: runstate.UserProtocol, User: &runstate.UserRecord{
		Message: llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Text: &llm.TextBlock{Text: text}}}},
	}}
}

// answered is the result of one call.
func answered(id, output string, failed bool) runstate.Record {
	return runstate.Record{Protocol: runstate.ToolResultProtocol, ToolResult: &runstate.ToolResultRecord{
		ToolUseID: id,
		Result:    llm.ToolResultBlock{ToolUseID: id, Content: output, IsError: failed},
	}}
}

func prose(text string) llm.ContentBlock { return llm.ContentBlock{Text: &llm.TextBlock{Text: text}} }
func reason(text string) llm.ContentBlock {
	return llm.ContentBlock{Thinking: &llm.ThinkingBlock{Text: text}}
}

func calls(id, name, input string) llm.ContentBlock {
	return llm.ContentBlock{ToolUse: &llm.ToolUseBlock{ID: id, Name: name, Input: json.RawMessage(input)}}
}

// at is the nth second of a fixed minute, so a spec can pin the timestamps a replay
// writes.
func at(n int) time.Time {
	return time.Date(2026, 1, 1, 0, 0, n, 0, time.UTC)
}

// dated stamps a folded conversation the way a fold of a journal written since
// runstate.Record.Time does: one time per message a second apart, each tool result dated
// from the message carrying it, and the unfinished turn after the last message.
func dated(rs *runstate.RunState) *runstate.RunState {
	rs.Times = make([]time.Time, len(rs.Messages))
	rs.ResultTimes = map[string]time.Time{}

	for i, msg := range rs.Messages {
		rs.Times[i] = at(i + 1)

		for _, block := range msg.Content {
			if block.ToolResult == nil {
				continue
			}

			rs.ResultTimes[block.ToolResult.ToolUseID] = at(i + 1)
		}
	}

	if rs.Pending != nil {
		rs.Pending.Time = at(len(rs.Messages) + 1)
	}

	return rs
}

// replayed is a stored conversation read back through the open route's writer, and the
// body the page read.
func replayed(rs *runstate.RunState, ask ...web.Question) string {
	GinkgoHelper()

	o := open()

	o.writer.Open()
	o.writer.Replay(rs)

	for _, q := range ask {
		o.writer.Ask(q)
	}

	if len(ask) > 0 {
		o.writer.Close(suspended)
	} else {
		o.writer.Close(completed)
	}

	return o.body()
}

var _ = Describe("A replayed conversation", func() {
	// A snapshot is what AG-UI has for a conversation that is already over, so nothing
	// is re-narrated as the events a live turn produced.
	It("Should write the turns it holds as one messages snapshot", func() {
		Expect(replayed(folded("list the streams",
			assistant(prose("let me look"), calls("c1", "list", `{"limit":2}`)),
			answered("c1", "two streams", false),
			assistant(prose("there are two")),
			typed("and the consumers?"),
			assistant(prose("two of those too")),
		))).To(Equal(frames("w-abc", "open-w-abc",
			`{"type":"MESSAGES_SNAPSHOT","messages":[`+
				`{"id":"open-w-abc-1","role":"user","content":"list the streams"},`+
				`{"id":"open-w-abc-2","role":"assistant","content":"let me look","toolCalls":[{"id":"c1","type":"function","function":{"name":"list","arguments":"{\"limit\":2}"}}]},`+
				`{"id":"open-w-abc-3","role":"tool","content":"two streams","toolCallId":"c1"},`+
				`{"id":"open-w-abc-4","role":"assistant","content":"there are two"},`+
				`{"id":"open-w-abc-5","role":"user","content":"and the consumers?"},`+
				`{"id":"open-w-abc-6","role":"assistant","content":"two of those too"}`+
				`]}`,
			`{"type":"RUN_FINISHED","threadId":"w-abc","runId":"open-w-abc","result":{"reason":"completed"},"outcome":{"type":"success"}}`,
		)))
	})

	// A journal message is a list of blocks and an AG-UI message is one role's turn, so
	// a turn that reasoned before it wrote is two messages.
	It("Should write a turn's reasoning as the message before its prose", func() {
		Expect(replayed(folded("think first",
			assistant(reason("let me think"), prose("here it is")),
		))).To(ContainSubstring(
			`{"id":"open-w-abc-2","role":"reasoning","content":"let me think"},` +
				`{"id":"open-w-abc-3","role":"assistant","content":"here it is"}`,
		))
	})

	// error is the field that marks a failure and content is the field every client
	// renders, so what the tool said is in both.
	It("Should mark a call that failed", func() {
		Expect(replayed(folded("list the streams",
			assistant(calls("c1", "list", `{}`)),
			answered("c1", "the stream does not exist", true),
		))).To(ContainSubstring(
			`{"id":"open-w-abc-3","role":"tool","content":"the stream does not exist","toolCallId":"c1","error":"the stream does not exist"}`,
		))
	})

	// The pending turn is in the snapshot, so the call the interrupt names is a call
	// the client already holds when it reads the run-finished event.
	It("Should come back on the interrupt it was left on, holding the call it names", func() {
		body := replayed(
			folded("wipe it", assistant(calls("c1", "stream_rm", `{}`))),
			web.Question{Kind: web.KindApprove, ToolUseID: "c1", Command: "stream rm", Display: "stream rm", Tag: "ai:confirm"},
		)

		Expect(body).To(ContainSubstring(`{"id":"open-w-abc-2","role":"assistant","toolCalls":[{"id":"c1","type":"function","function":{"name":"stream_rm","arguments":"{}"}}]}`))
		Expect(body).To(ContainSubstring(`"outcome":{"type":"interrupt","interrupts":[{"id":"approve:c1","reason":"tool_call","message":"stream rm","toolCallId":"c1"`))
	})

	// A run journaled mid-batch left the calls after the one it answered without
	// results, and the client is shown what the journal holds.
	It("Should write a pending turn's answered calls and leave the rest unanswered", func() {
		body := replayed(folded("do both",
			assistant(calls("c1", "list", `{}`), calls("c2", "list", `{}`)),
			answered("c1", "two streams", false),
		))

		Expect(body).To(ContainSubstring(`"toolCalls":[{"id":"c1","type":"function","function":{"name":"list","arguments":"{}"}},{"id":"c2","type":"function","function":{"name":"list","arguments":"{}"}}]`))
		Expect(body).To(ContainSubstring(`{"id":"open-w-abc-3","role":"tool","content":"two streams","toolCallId":"c1"}`))
		Expect(body).ToNot(ContainSubstring(`"toolCallId":"c2"`))
	})

	// The snapshot stays one event, so a client renders a conversation that is already
	// over rather than a turn being narrated. The times follow it as custom events, in the
	// SDK's own time slot.
	Describe("the times the journal holds", func() {
		It("Should follow the snapshot with one timestamped event per dated message", func() {
			body := replayed(dated(folded("list the streams",
				assistant(prose("let me look"), calls("c1", "list", `{}`)),
				answered("c1", "two streams", false),
				assistant(prose("there are two")),
			)))

			Expect(body).To(ContainSubstring(`data: {"type":"MESSAGES_SNAPSHOT"`))
			Expect(body).To(ContainSubstring(`data: {"type":"CUSTOM","timestamp":1767225601000,"name":"fisk.message_time","value":"open-w-abc-1"}`))
			Expect(body).To(ContainSubstring(`data: {"type":"CUSTOM","timestamp":1767225602000,"name":"fisk.message_time","value":"open-w-abc-2"}`))
			Expect(body).To(ContainSubstring(`data: {"type":"CUSTOM","timestamp":1767225603000,"name":"fisk.message_time","value":"open-w-abc-3"}`))
			Expect(body).To(ContainSubstring(`data: {"type":"CUSTOM","timestamp":1767225604000,"name":"fisk.message_time","value":"open-w-abc-4"}`))
			accepted(body)
		})

		It("Should date the reasoning message apart from the prose of the same turn", func() {
			body := replayed(dated(folded("think first",
				assistant(reason("let me think"), prose("here it is")),
			)))

			Expect(body).To(ContainSubstring(`"name":"fisk.message_time","value":"open-w-abc-2"}`))
			Expect(body).To(ContainSubstring(`"name":"fisk.message_time","value":"open-w-abc-3"}`))
		})

		It("Should send no timestamped event for a conversation carrying no times", func() {
			body := replayed(folded("list the streams",
				assistant(prose("there are two")),
			))

			Expect(body).ToNot(ContainSubstring("fisk.message_time"))
		})
	})

	// A conversation that never ran writes the run frame and nothing between it.
	It("Should write an empty snapshot for a conversation holding no message", func() {
		Expect(replayed(&runstate.RunState{})).To(Equal(frames("w-abc", "open-w-abc",
			`{"type":"MESSAGES_SNAPSHOT","messages":null}`,
			`{"type":"RUN_FINISHED","threadId":"w-abc","runId":"open-w-abc","result":{"reason":"completed"},"outcome":{"type":"success"}}`,
		)))
	})
})
