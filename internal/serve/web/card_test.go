//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/a2a"
	wire "github.com/choria-io/fisk-ai/internal/a2a/wire/v1"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/toolkit"
)

// cardTool is a tool that answers what a card asks of one: its name, what it does, and
// what calling it does to the world.
type cardTool struct {
	name     string
	behavior toolkit.Behavior
	gated    bool
}

func (t *cardTool) Name() string             { return t.name }
func (t *cardTool) Description() string      { return t.name + " does a thing" }
func (t *cardTool) ModelDescription() string { return t.Description() }
func (t *cardTool) InputSchema() map[string]any {
	return map[string]any{"type": "object"}
}
func (t *cardTool) Definition(bool) llm.ToolDef {
	return llm.ToolDef{Name: t.name, Description: t.Description(), InputSchema: t.InputSchema()}
}

func (t *cardTool) Execute(context.Context, json.RawMessage, toolkit.ExecDeps) (*toolkit.Outcome, error) {
	return &toolkit.Outcome{Output: "ok"}, nil
}

func (t *cardTool) MCPExposable() bool               { return true }
func (t *cardTool) A2AExposable() bool               { return true }
func (t *cardTool) Behavior() toolkit.Behavior       { return t.behavior }
func (t *cardTool) Command() string                  { return t.name }
func (t *cardTool) NeedsConfirm(extra []string) bool { return t.gated }
func (t *cardTool) ConfirmTrigger(extra []string) string {
	if t.gated {
		return toolkit.ConfirmTag
	}

	return ""
}

// cardTools cover the four groups a console draws: read-only, destructive, idempotent
// and a tool that declares nothing. The gated one is what a card built through a2a's
// exposure policy drops.
func cardTools() []toolkit.Tool {
	return []toolkit.Tool{
		&cardTool{name: "ls", behavior: toolkit.Behavior{ReadOnly: toolkit.HintTrue}},
		&cardTool{name: "rm", behavior: toolkit.Behavior{Destructive: toolkit.HintTrue}},
		&cardTool{name: "sync", behavior: toolkit.Behavior{Idempotent: toolkit.HintTrue}},
		&cardTool{name: "run"},
		&cardTool{name: "gated", gated: true},
	}
}

func cardToolNames(card wire.AgentCard) []string {
	names := make([]string, len(card.Tools))
	for i, t := range card.Tools {
		names[i] = t.Name
	}

	return names
}

var _ = Describe("The agent card", func() {
	It("Should answer under the base path with what the agent says about itself", func() {
		opts := testOptions()
		opts.Card = a2a.CardOptions{
			Version:     "1.2.3",
			Model:       "claude-sonnet-4-6",
			Description: "manages nats auth",
			DisplayName: "NATS Auth",
			IconURL:     "https://example.net/agent.png",
			Prompts:     []string{"who can publish to ORDERS?", "add a user for the billing service"},
		}
		opts.CardTools = cardTools()

		card := readCard(newTestChannel(opts), http.StatusOK)
		Expect(card.Name).To(Equal("agent1"), "the identity this channel derives its sessions under")
		Expect(card.Version).To(Equal("1.2.3"))
		Expect(card.Model).To(Equal("claude-sonnet-4-6"))
		Expect(card.Description).To(Equal("manages nats auth"))
		Expect(card.DisplayName).To(Equal("NATS Auth"))
		Expect(card.IconURL).To(Equal("https://example.net/agent.png"))
		Expect(card.Prompts).To(Equal([]string{"who can publish to ORDERS?", "add a user for the billing service"}))
	})

	// The whole approval flow is driven on a gated command, so a card that omitted it
	// would leave a console unable to show the tool the demonstration turns on. a2a drops
	// it because a served agent has nobody to approve it; this channel has an operator.
	It("Should list a confirm-gated tool that an a2a card omits", func() {
		opts := testOptions()
		opts.CardTools = cardTools()

		Expect(cardToolNames(readCard(newTestChannel(opts), http.StatusOK))).To(ConsistOf("ls", "rm", "sync", "run", "gated"))
	})

	// The groups a console draws come off the tri-state hints the descriptor already
	// carried: a tool that declares nothing lands in unstated rather than among the safe
	// ones, and no tag sets open_world.
	It("Should carry each tool's declared behavior and assert nothing for a tool with none", func() {
		opts := testOptions()
		opts.CardTools = cardTools()

		card := readCard(newTestChannel(opts), http.StatusOK)

		behavior := map[string]toolkit.Behavior{}
		for _, t := range card.Tools {
			behavior[t.Name] = a2a.BehaviorOf(t.Behavior)
		}

		Expect(behavior["ls"].ReadOnly).To(Equal(toolkit.HintTrue))
		Expect(behavior["rm"].Destructive).To(Equal(toolkit.HintTrue))
		Expect(behavior["sync"].Idempotent).To(Equal(toolkit.HintTrue))
		Expect(behavior["run"]).To(Equal(toolkit.Behavior{}))

		for name, b := range behavior {
			Expect(b.OpenWorld).To(Equal(toolkit.HintUnset), name)
		}
	})

	// A tool missing because its source is down must not read as a tool somebody
	// removed, so the source is named on the card a console renders.
	It("Should carry a note naming a source whose tools are missing", func() {
		opts := testOptions()
		opts.Card.Notes = []string{`the tools of the mcp server "github" could not be listed, so any tool it supplies is missing here`}

		card := readCard(newTestChannel(opts), http.StatusOK)
		Expect(card.Notes).To(ConsistOf(ContainSubstring(`mcp server "github"`)))
	})

	It("Should refuse to serve a card whose icon url is not an https url", func() {
		for _, icon := range []string{"javascript:alert(1)", "http://example.net/a.png"} {
			opts := testOptions()
			opts.Card.IconURL = icon

			readCard(newTestChannel(opts), http.StatusInternalServerError)
		}
	})

	It("Should refuse to serve a card whose icon url is over the length limit", func() {
		opts := testOptions()
		opts.Card.IconURL = "https://example.net/" + strings.Repeat("a", wire.MaxIconURLLength)

		readCard(newTestChannel(opts), http.StatusInternalServerError)
	})

	// The discovery reply schema limits both, so a card over either is one no peer could
	// read. The web channel refuses it per request, where the a2a server refuses it at
	// construction, since that one builds its card once.
	It("Should refuse to serve a card whose display name or icon is over the length limit", func() {
		opts := testOptions()
		opts.Card.DisplayName = strings.Repeat("a", wire.MaxDisplayNameLength+1)
		readCard(newTestChannel(opts), http.StatusInternalServerError)

		opts = testOptions()
		opts.Card.Icon = strings.Repeat("a", wire.MaxIconLength+1)
		readCard(newTestChannel(opts), http.StatusInternalServerError)
	})

	It("Should refuse to serve a card carrying more sample prompts than the schema will, or one too long", func() {
		opts := testOptions()
		opts.Card.Prompts = slices.Repeat([]string{"ask me"}, wire.MaxPrompts+1)
		readCard(newTestChannel(opts), http.StatusInternalServerError)

		opts = testOptions()
		opts.Card.Prompts = []string{strings.Repeat("a", wire.MaxPromptLength+1)}
		readCard(newTestChannel(opts), http.StatusInternalServerError)
	})

	It("Should refuse a format mounted where the card answers", func() {
		opts := testOptions()
		opts.Formats = []Mount{{Path: CardPath, Format: &fakeFormat{}}}

		_, err := New(opts)
		Expect(err).To(MatchError(ContainSubstring("where this channel answers the agent card")))
	})

	It("Should refuse a POST to the card", func() {
		rec := cardRequest(newTestChannel(testOptions()), http.MethodPost, nil)
		Expect(rec.Code).To(Equal(http.StatusMethodNotAllowed))
	})

	It("Should refuse a card request from an unlisted origin", func() {
		rec := cardRequest(newTestChannel(testOptions()), http.MethodGet, map[string]string{"Origin": "http://evil.example"})
		Expect(rec.Code).To(Equal(http.StatusForbidden))
	})

	It("Should let a listed page read the card", func() {
		rec := cardRequest(newTestChannel(testOptions()), http.MethodGet, map[string]string{"Origin": testOrigin})
		Expect(rec.Code).To(Equal(http.StatusOK))
		Expect(rec.Header().Get("Access-Control-Allow-Origin")).To(Equal(testOrigin))
	})
})

var _ = Describe("ResolveAgentTools", func() {
	toolNames := func(tools AgentTools) []string {
		names := make([]string, len(tools.Tools))
		for i, t := range tools.Tools {
			names[i] = t.Name()
		}

		return names
	}

	// A run injects these alongside the application's commands, so a card built from the
	// commands alone names fewer tools than the agent behind it calls.
	It("Should list the built-ins the configuration enables", func() {
		cfg := agenttest.Config(GinkgoTB(), nil, agenttest.WithHITL(), agenttest.WithMemory(), agenttest.WithRAG())

		tools, err := ResolveAgentTools(context.Background(), cfg, nil)
		Expect(err).ToNot(HaveOccurred())
		Expect(toolNames(tools)).To(ConsistOf(
			config.AskHumanConfirmToolName,
			config.AskHumanSelectToolName,
			config.AskHumanInputToolName,
			config.MemoryListToolName,
			config.MemoryReadToolName,
			config.MemoryWriteToolName,
			config.MemoryDeleteToolName,
			config.KnowledgeSearchToolName,
			config.KnowledgeEnumerateToolName,
		))
	})

	It("Should list no built-in the configuration leaves off", func() {
		tools, err := ResolveAgentTools(context.Background(), agenttest.Config(GinkgoTB(), nil), nil)
		Expect(err).ToNot(HaveOccurred())
		Expect(tools.Tools).To(BeEmpty())
	})
})
