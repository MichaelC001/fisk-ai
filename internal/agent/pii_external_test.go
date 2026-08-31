//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// These specs exercise harness.pii through the exported agent.Run API: what the model
// receives, what a caller's own hook sees, and what the operator is told. The scanner
// itself is covered in internal/pii; what is proved here is the wiring, the ordering
// against a caller's hooks, and the two modes.
package agent_test

import (
	"context"
	"encoding/json"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/toolkit"
	"github.com/choria-io/fisk-ai/internal/toolkit/functool"
)

// piiAddress is the value the specs expect to be found. An address is what the detector
// is most dependable on; the assertions check the value is gone rather than matching the
// placeholder, which is the detector's wording and not ours to pin.
const piiAddress = "alice.smith@example.com"

// promptText carries the address into a run through the prompt.
const promptText = "mail the report to " + piiAddress + " and tell me when it is sent"

// requestText is every text block of the messages a provider was sent, joined, which is
// what the model actually saw.
func requestText(reqs []llm.Request) string {
	var sb strings.Builder
	for _, req := range reqs {
		for _, msg := range req.Messages {
			for _, block := range msg.Content {
				if block.Text != nil {
					sb.WriteString(block.Text.Text)
					sb.WriteString("\n")
				}
				if block.ToolResult != nil {
					sb.WriteString(block.ToolResult.Content)
					sb.WriteString("\n")
				}
			}
		}
	}

	return sb.String()
}

// leakyTool returns a custom tool whose output carries the address, standing in for the
// grep over a mailbox or the log file that is where personal data actually arrives.
func leakyTool() *functool.Tool {
	GinkgoHelper()

	tool, err := functool.New(functool.Spec{
		Name:        "read_record",
		Description: "read a customer record",
		Schema:      map[string]any{"type": "object"},
		Handler: func(context.Context, json.RawMessage, *functool.CallContext) (string, error) {
			return "customer: acme ltd, contact " + piiAddress + ", plan: enterprise", nil
		},
	})
	Expect(err).NotTo(HaveOccurred())

	return tool
}

var _ = Describe("harness.pii", func() {
	// The initial prompt is scanned, the redaction reaches the conversation the provider
	// is sent (not only the prompt slice the journal records), and the operator is told.
	It("Should redact the initial prompt before the model and the journal", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		provider := agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("sent"))
		events := agenttest.NewRecordingEvents()
		store := agenttest.NewFakeSessionStore(GinkgoTB())

		res, err := agent.Run(context.Background(), agent.Options{
			Config:       agenttest.Config(GinkgoTB(), app, agenttest.WithPII(config.PIIModeRedact)),
			ConfigFile:   "agent.yaml",
			Prompt:       []string{promptText},
			Provider:     provider,
			Checkpoint:   agent.Checkpoint{Enabled: true},
			SessionStore: store,
		}, events, agenttest.NewScriptedPrompter(GinkgoTB()))

		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonCompleted))

		// The model never saw the address, and did see the rest of the prompt.
		sent := requestText(provider.Requests())
		Expect(sent).NotTo(ContainSubstring(piiAddress))
		Expect(sent).To(ContainSubstring("mail the report to"))

		// Neither did the journal, which is also what a resume would replay.
		rs, lerr := store.Load(context.Background(), res.SessionID)
		Expect(lerr).NotTo(HaveOccurred())
		Expect(rs.Prompt).NotTo(ContainSubstring(piiAddress))
		Expect(rs.Prompt).To(ContainSubstring("mail the report to"))

		Expect(events.HasWarning(agent.WarnPIIRedacted)).To(BeTrue())
		for _, w := range events.Warnings() {
			Expect(w.Name).NotTo(ContainSubstring(piiAddress))
			Expect(strings.Join(w.Params, " ")).NotTo(ContainSubstring(piiAddress))
		}
	})

	// Reject mode stops the run before a model call and says why without repeating the
	// value.
	It("Should reject the initial prompt", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		// No scripted responses: a model call would error, proving none is made.
		provider := agenttest.NewScriptedProvider(GinkgoTB())
		events := agenttest.NewRecordingEvents()

		_, err := agent.Run(context.Background(), agent.Options{
			Config:     agenttest.Config(GinkgoTB(), app, agenttest.WithPII(config.PIIModeReject)),
			ConfigFile: "agent.yaml",
			Prompt:     []string{promptText},
			Provider:   provider,
		}, events, agenttest.NewScriptedPrompter(GinkgoTB()))

		Expect(err).To(MatchError(ContainSubstring("initial prompt was rejected")))
		Expect(err).To(MatchError(ContainSubstring("harness.pii.mode is reject")))
		Expect(err.Error()).NotTo(ContainSubstring(piiAddress))
		Expect(provider.Requests()).To(BeEmpty())
		Expect(events.HasWarning(agent.WarnPIIWithheld)).To(BeTrue())
	})

	// A run whose text has nothing personal in it is untouched and says nothing, so the
	// feature is invisible until it acts.
	It("Should leave clean text alone", func() {
		const clean = "restart the payments service and summarize the logs"

		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		provider := agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done"))
		events := agenttest.NewRecordingEvents()

		res, err := agent.Run(context.Background(), agent.Options{
			Config:     agenttest.Config(GinkgoTB(), app, agenttest.WithPII(config.PIIModeRedact)),
			ConfigFile: "agent.yaml",
			Prompt:     []string{clean},
			Provider:   provider,
		}, events, agenttest.NewScriptedPrompter(GinkgoTB()))

		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonCompleted))
		Expect(requestText(provider.Requests())).To(ContainSubstring(clean))
		Expect(events.HasWarning(agent.WarnPIIRedacted)).To(BeFalse())
		Expect(events.HasWarning(agent.WarnPIIWithheld)).To(BeFalse())
	})

	// A tool result is scanned before the model, the journal or the trace sees it.
	It("Should redact tool output", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), emptyFiskApp())
		provider := agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.ToolUseResponse("call-1", "read_record", json.RawMessage(`{}`)),
			agenttest.TextResponse("read it"),
		)
		events := agenttest.NewRecordingEvents()

		res, err := agent.Run(context.Background(), agent.Options{
			Config:      agenttest.Config(GinkgoTB(), app, agenttest.WithPII(config.PIIModeRedact)),
			ConfigFile:  "agent.yaml",
			Prompt:      []string{"read the record"},
			Provider:    provider,
			CustomTools: []toolkit.Tool{leakyTool()},
		}, events, agenttest.NewScriptedPrompter(GinkgoTB()))

		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonCompleted))

		sent := requestText(provider.Requests())
		Expect(sent).NotTo(ContainSubstring(piiAddress))
		Expect(sent).To(ContainSubstring("customer: acme ltd"))

		// The operator's own screen shows what the model was given, not the raw output: the
		// same trace crosses a2a to a caller who is not on this machine.
		for _, tr := range events.ToolResults() {
			Expect(tr.Output).NotTo(ContainSubstring(piiAddress))
		}

		Expect(events.HasWarning(agent.WarnPIIRedacted)).To(BeTrue())
	})

	// Reject mode replaces a tool result with an error the model can act on rather than
	// leaving a silent hole, and repeats no value in it.
	It("Should withhold tool output", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), emptyFiskApp())
		provider := agenttest.NewScriptedProvider(GinkgoTB(),
			agenttest.ToolUseResponse("call-1", "read_record", json.RawMessage(`{}`)),
			agenttest.TextResponse("could not read it"),
		)
		events := agenttest.NewRecordingEvents()

		_, err := agent.Run(context.Background(), agent.Options{
			Config:      agenttest.Config(GinkgoTB(), app, agenttest.WithPII(config.PIIModeReject)),
			ConfigFile:  "agent.yaml",
			Prompt:      []string{"read the record"},
			Provider:    provider,
			CustomTools: []toolkit.Tool{leakyTool()},
		}, events, agenttest.NewScriptedPrompter(GinkgoTB()))

		Expect(err).NotTo(HaveOccurred())

		sent := requestText(provider.Requests())
		Expect(sent).NotTo(ContainSubstring(piiAddress))
		Expect(sent).NotTo(ContainSubstring("acme ltd"))
		Expect(sent).To(ContainSubstring("withheld"))
		Expect(sent).To(ContainSubstring("harness.pii.mode is reject"))

		Expect(events.HasWarning(agent.WarnPIIWithheld)).To(BeTrue())
	})

	// This is the ordering the feature exists for: a caller's own UserPromptSubmit runs
	// first, and whatever it produced is scanned. A hook that introduces personal data
	// does not thereby get it past the scan.
	It("Should scan what a caller's hook left behind", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		provider := agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("done"))
		events := agenttest.NewRecordingEvents()

		var saw agent.UserPromptSubmitInfo

		res, err := agent.Run(context.Background(), agent.Options{
			Config:     agenttest.Config(GinkgoTB(), app, agenttest.WithPII(config.PIIModeRedact)),
			ConfigFile: "agent.yaml",
			Prompt:     []string{"look up the contact"},
			Provider:   provider,
			Hooks: agent.Hooks{
				UserPromptSubmit: func(_ context.Context, in agent.UserPromptSubmitInfo) (agent.UserPromptSubmitResult, error) {
					saw = in

					return agent.UserPromptSubmitResult{Rewrite: "mail it to " + piiAddress}, nil
				},
			},
		}, events, agenttest.NewScriptedPrompter(GinkgoTB()))

		Expect(err).NotTo(HaveOccurred())
		Expect(res.Reason).To(Equal(runstate.ReasonCompleted))

		// The caller's hook saw the operator's text, unscanned, since nothing has acted yet.
		Expect(saw.Text).To(Equal("look up the contact"))
		Expect(saw.Initial).To(BeTrue())

		// The address the hook introduced did not reach the model.
		sent := requestText(provider.Requests())
		Expect(sent).NotTo(ContainSubstring(piiAddress))
		Expect(sent).To(ContainSubstring("mail it to"))
	})

	// A caller's deny is answered as-is: nothing is entering, so nothing is scanned and
	// the caller's own reason is what the run reports.
	It("Should keep a caller's deny", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		provider := agenttest.NewScriptedProvider(GinkgoTB())
		events := agenttest.NewRecordingEvents()

		_, err := agent.Run(context.Background(), agent.Options{
			Config:     agenttest.Config(GinkgoTB(), app, agenttest.WithPII(config.PIIModeRedact)),
			ConfigFile: "agent.yaml",
			Prompt:     []string{promptText},
			Provider:   provider,
			Hooks: agent.Hooks{
				UserPromptSubmit: func(context.Context, agent.UserPromptSubmitInfo) (agent.UserPromptSubmitResult, error) {
					return agent.UserPromptSubmitResult{Deny: true, DenyReason: "blocked by policy"}, nil
				},
			},
		}, events, agenttest.NewScriptedPrompter(GinkgoTB()))

		Expect(err).To(MatchError(ContainSubstring("blocked by policy")))
		Expect(events.HasWarning(agent.WarnPIIRedacted)).To(BeFalse())
	})

	// The off mode leaves the text as the operator wrote it.
	It("Should scan nothing when off", func() {
		app := agenttest.NewFakeApp(GinkgoTB(), exampleApp())
		provider := agenttest.NewScriptedProvider(GinkgoTB(), agenttest.TextResponse("sent"))
		events := agenttest.NewRecordingEvents()

		_, err := agent.Run(context.Background(), agent.Options{
			Config:     agenttest.Config(GinkgoTB(), app, agenttest.WithPII(config.PIIModeOff)),
			ConfigFile: "agent.yaml",
			Prompt:     []string{promptText},
			Provider:   provider,
		}, events, agenttest.NewScriptedPrompter(GinkgoTB()))

		Expect(err).NotTo(HaveOccurred())
		Expect(requestText(provider.Requests())).To(ContainSubstring(piiAddress))
		Expect(events.Warnings()).To(BeEmpty())
	})
})
