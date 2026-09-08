//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package web

import (
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/toolkit"
)

// gatedTool is a confirm-gated command as a rebuilt question sees one: the line the
// operator approves for these arguments, and the gating tag chosen out of the operator's
// own confirm tags the way every tool kind chooses it.
type gatedTool struct {
	*cardTool
	path string
	tags []string
}

func (t *gatedTool) Command() string                  { return t.path }
func (t *gatedTool) NeedsConfirm(extra []string) bool { return toolkit.NeedsConfirm(t.tags, extra) }
func (t *gatedTool) ConfirmTrigger(extra []string) string {
	return toolkit.ConfirmTrigger(t.tags, extra)
}
func (t *gatedTool) TraceLine(input json.RawMessage) string { return t.path + " " + string(input) }

// unlinedTool is confirm-gated and renders no command line of its own, which is what
// leaves the runner falling back on the call's describe line and then on its name.
type unlinedTool struct {
	*cardTool
	tags []string
}

func (t *unlinedTool) Command() string                  { return t.name }
func (t *unlinedTool) NeedsConfirm(extra []string) bool { return toolkit.NeedsConfirm(t.tags, extra) }
func (t *unlinedTool) ConfirmTrigger(extra []string) string {
	return toolkit.ConfirmTrigger(t.tags, extra)
}
func (t *unlinedTool) TraceLine(json.RawMessage) string { return "" }

// describingTool describes its call, which is the line the gate presents for a tool that
// renders no command line.
type describingTool struct {
	*unlinedTool
	display string
}

func (t *describingTool) Describe(json.RawMessage) toolkit.CallInfo {
	return toolkit.CallInfo{Display: t.display}
}

// confirmed is a tool set holding one confirm-gated command under name, running path.
func confirmed(name, path string, tags ...string) map[string]toolkit.Tool {
	if len(tags) == 0 {
		tags = []string{toolkit.ConfirmTag}
	}

	return map[string]toolkit.Tool{
		name: &gatedTool{cardTool: &cardTool{name: name}, path: path, tags: tags},
	}
}

// pendingCall is one call of a turn a run left unfinished.
func pendingCall(id, name, input string) llm.ContentBlock {
	return llm.ContentBlock{ToolUse: &llm.ToolUseBlock{ID: id, Name: name, Input: json.RawMessage(input)}}
}

// stopped is a conversation whose last turn left calls unanswered, as a fold produces
// one.
func stopped(calls ...llm.ContentBlock) *runstate.RunState {
	return &runstate.RunState{
		Pending: &runstate.PendingTurn{
			Assistant: llm.Message{Role: llm.RoleAssistant, Content: calls},
			Answered:  map[string]bool{},
			Deferred:  map[string]runstate.DeferredRecord{},
		},
	}
}

var _ = Describe("The question a stored conversation stopped on", func() {
	It("Should report none for a conversation whose last turn finished", func() {
		for _, state := range []*runstate.RunState{nil, {}, {Pending: &runstate.PendingTurn{}}} {
			_, waiting := PendingQuestion(state, nil, nil)
			Expect(waiting).To(BeFalse())
		}
	})

	// The line the person approves is produced at dispatch and nothing journals it, so it
	// is rendered again here from the tool that would run the call.
	It("Should render a gated command as the line it runs and the tag that gated it", func() {
		tools := map[string]toolkit.Tool{
			"wipe": &gatedTool{cardTool: &cardTool{name: "wipe"}, path: "stream rm", tags: []string{toolkit.ConfirmTag}},
		}

		q, waiting := PendingQuestion(stopped(pendingCall("c1", "wipe", `{"stream":"ORDERS"}`)), tools, nil)

		Expect(waiting).To(BeTrue())
		Expect(q).To(Equal(Question{
			Kind:      KindApprove,
			ToolUseID: "c1",
			Command:   "stream rm",
			Display:   `stream rm {"stream":"ORDERS"}`,
			Tag:       toolkit.ConfirmTag,
		}))
	})

	It("Should name the operator's own confirm tag where that is what gated the command", func() {
		tools := confirmed("wipe", "stream rm", "impact:rw")

		q, waiting := PendingQuestion(stopped(pendingCall("c1", "wipe", `{}`)), tools, []string{"impact:rw"})
		Expect(waiting).To(BeTrue())
		Expect(q.Tag).To(Equal("impact:rw"))

		_, waiting = PendingQuestion(stopped(pendingCall("c1", "wipe", `{}`)), tools, nil)
		Expect(waiting).To(BeFalse(), "no tag of this agent's gates the call, so the resume runs it")
	})

	It("Should take the described call for a tool that renders no command line", func() {
		tools := map[string]toolkit.Tool{
			"deploy": &describingTool{
				unlinedTool: &unlinedTool{cardTool: &cardTool{name: "deploy"}, tags: []string{toolkit.ConfirmTag}},
				display:     "deploy --env=prod",
			},
		}

		q, _ := PendingQuestion(stopped(pendingCall("c1", "deploy", `{"env":"prod"}`)), tools, nil)

		Expect(q.Kind).To(Equal(KindApprove))
		Expect(q.Command).To(Equal("deploy"))
		Expect(q.Display).To(Equal("deploy --env=prod"))
		Expect(q.Tag).To(Equal(toolkit.ConfirmTag))
	})

	It("Should fall back to the call's name for a tool that describes nothing", func() {
		tools := map[string]toolkit.Tool{
			"plain": &unlinedTool{cardTool: &cardTool{name: "plain"}, tags: []string{toolkit.ConfirmTag}},
		}

		q, _ := PendingQuestion(stopped(pendingCall("c1", "plain", `{}`)), tools, nil)
		Expect(q.Display).To(Equal("plain"))
	})

	// A pending turn is one journaled mid-batch, which a crash reaches as readily as a
	// question does, so a call the resume runs without asking draws no card and the page
	// gets the conversation with nothing to answer.
	It("Should report none for an unanswered call the gate would run without asking", func() {
		tools := map[string]toolkit.Tool{"list": &cardTool{name: "list"}}

		state := stopped(pendingCall("c1", "list", `{}`), pendingCall("c2", "list", `{}`))
		state.Pending.Answered["c1"] = true

		_, waiting := PendingQuestion(state, tools, nil)
		Expect(waiting).To(BeFalse())
	})

	// The redispatch answers the model that the tool is unknown, and nobody is asked
	// anything on the way.
	It("Should report none for a call whose tool the agent no longer holds", func() {
		_, waiting := PendingQuestion(stopped(pendingCall("c1", "gone", `{"stream":"ORDERS"}`)), map[string]toolkit.Tool{}, nil)
		Expect(waiting).To(BeFalse())
	})

	// The gate spends an approval the operator has already given rather than asking
	// again, so the conversation is waiting on nobody.
	It("Should report none for a call the operator has already approved", func() {
		tools := confirmed("wipe", "stream rm")

		standing := stopped(pendingCall("c1", "wipe", `{}`))
		standing.Approvals = []string{"wipe"}

		_, waiting := PendingQuestion(standing, tools, nil)
		Expect(waiting).To(BeFalse(), "the tool carries a standing approval")

		once := stopped(pendingCall("c1", "wipe", `{}`))
		once.CallApprovals = []runstate.CallApprovalRecord{{ToolUseID: "c1", ToolName: "wipe"}}

		_, waiting = PendingQuestion(once, tools, nil)
		Expect(waiting).To(BeFalse(), "the call carries a one-shot approval")
	})

	// A one-shot approval is spent on the call it names, so the next call of the same
	// tool is asked about again.
	It("Should ask about a later call of a tool whose one-shot approval names another", func() {
		state := stopped(pendingCall("c1", "wipe", `{}`), pendingCall("c2", "wipe", `{}`))
		state.CallApprovals = []runstate.CallApprovalRecord{{ToolUseID: "c1", ToolName: "wipe"}}

		q, waiting := PendingQuestion(state, confirmed("wipe", "stream rm"), nil)

		Expect(waiting).To(BeTrue())
		Expect(q.ToolUseID).To(Equal("c2"))
	})

	DescribeTable("A human-in-the-loop call",
		func(tool string, input string, want Question) {
			q, waiting := PendingQuestion(stopped(pendingCall("c1", tool, input)), nil, nil)

			Expect(waiting).To(BeTrue())
			Expect(q).To(Equal(want))
		},
		Entry("asking for a yes or no",
			config.AskHumanConfirmToolName, `{"question":"shall I?"}`,
			Question{Kind: KindConfirm, ToolUseID: "c1", Text: "shall I?"}),
		Entry("asking for one of a list",
			config.AskHumanSelectToolName, `{"question":"which one?","options":["ORDERS","EVENTS"]}`,
			Question{Kind: KindSelect, ToolUseID: "c1", Text: "which one?", Options: []string{"ORDERS", "EVENTS"}}),
		Entry("asking for a value",
			config.AskHumanInputToolName, `{"question":"which subject?","default":"orders"}`,
			Question{Kind: KindInput, ToolUseID: "c1", Text: "which subject?", Default: "orders"}),
		Entry("written with arguments the tool would have refused",
			config.AskHumanConfirmToolName, `{"question":`,
			Question{Kind: KindConfirm, ToolUseID: "c1"}),
	)

	// The runner dispatches in order and stops at the first call it has to ask about, so
	// a call already answered, a call whose answer arrives later and a call it runs on its
	// own are all passed over.
	It("Should name the first call a resume asks about", func() {
		tools := confirmed("wipe", "stream rm")
		tools["list"] = &cardTool{name: "list"}

		state := stopped(
			pendingCall("c1", "list", `{}`),
			pendingCall("c2", "defer", `{}`),
			pendingCall("c3", "list", `{}`),
			pendingCall("c4", "wipe", `{}`),
		)
		state.Pending.Answered["c1"] = true
		state.Pending.Deferred["c2"] = runstate.DeferredRecord{ToolUseID: "c2"}

		q, waiting := PendingQuestion(state, tools, nil)

		Expect(waiting).To(BeTrue())
		Expect(q.ToolUseID).To(Equal("c4"))
	})

	It("Should report none for a turn whose calls are all answered or deferred", func() {
		state := stopped(pendingCall("c1", "list", `{}`))
		state.Pending.Deferred["c1"] = runstate.DeferredRecord{ToolUseID: "c1"}

		_, waiting := PendingQuestion(state, nil, nil)
		Expect(waiting).To(BeFalse())
	})
})
