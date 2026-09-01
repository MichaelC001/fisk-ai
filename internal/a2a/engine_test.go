//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"

	"github.com/choria-io/fisk"
	"github.com/choria-io/fisk-ai/internal/toolkit"
	fisk2 "github.com/choria-io/fisk-ai/internal/toolkit/fisk"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// discardLogger is a slog logger that drops all output, for tests that build a
// Server directly without applyDefaults.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// toolsFor builds tools from an in-process fisk application's introspection, the
// same path the real commands use.
func toolsFor(app *fisk.Application) []toolkit.Tool {
	GinkgoHelper()

	tools, err := fisk2.ApplicationTools(introspect(app))
	Expect(err).NotTo(HaveOccurred())

	return toolkit.Tools(tools)
}

var _ = Describe("ExpectProtocol", func() {
	It("Should return the decoded message when the protocol matches", func() {
		req := NewToolRequest("ping", nil)
		StampRequest(context.Background(), &req.Header, "me", "you")
		data, err := json.Marshal(req)
		Expect(err).NotTo(HaveOccurred())

		msg, err := ExpectProtocol(data, ToolRequestProtocol)
		Expect(err).NotTo(HaveOccurred())
		Expect(msg).To(BeAssignableToTypeOf(&ToolRequest{}))
	})

	It("Should reject a message whose protocol is not the one the path carries", func() {
		req := NewDiscoveryRequest()
		StampRequest(context.Background(), &req.Header, "me", "you")
		data, err := json.Marshal(req)
		Expect(err).NotTo(HaveOccurred())

		_, err = ExpectProtocol(data, ToolRequestProtocol)
		Expect(err).To(MatchError(ErrProtocolMismatch))
	})

	It("Should reject an undecodable body", func() {
		_, err := ExpectProtocol([]byte(`{"protocol":"nope"}`), ToolRequestProtocol)
		Expect(err).To(MatchError(ErrProtocolMismatch))
	})
})

var _ = Describe("buildCard", func() {
	It("Should describe the agent and its tools", func() {
		app := fisk.New("app", "an app")
		app.Command("ping", "ping it")

		card := buildCard(ServerOptions{Identity: "svc", Version: "v1", Model: "opus"}, toolsFor(app))
		Expect(card.Name).To(Equal("svc"))
		Expect(card.Version).To(Equal("v1"))
		Expect(card.Model).To(Equal("opus"), "so a caller can see what answers its prompt")
		Expect(card.Protocols).To(ConsistOf(ProtocolNamespace))
		Expect(card.Tools).To(HaveLen(1))
		Expect(card.Tools[0].Name).To(Equal("ping"))
		Expect(card.Tools[0].InputSchema).NotTo(BeEmpty())
		Expect(card.Tools[0].Behavior.IsZero()).To(BeTrue())
	})

	It("Should carry the behavior a served tool declares so a peer reads it as structure", func() {
		app := fisk.New("app", "an app")
		app.Command("ls", "list things").Tag("ai:read_only").Tag("ai:idempotent")

		card := buildCard(ServerOptions{Identity: "svc", Version: "v1"}, toolsFor(app))
		Expect(card.Tools).To(HaveLen(1))
		Expect(card.Tools[0].Behavior).To(Equal(toolkit.Behavior{ReadOnly: toolkit.HintTrue, Idempotent: toolkit.HintTrue}))
	})

	// An agent that takes no prompts calls no model, and a card naming one would say
	// something about this agent that is not true.
	It("Should name no model where the caller supplied none", func() {
		card := buildCard(ServerOptions{Identity: "svc", Version: "v1"}, nil)
		Expect(card.Model).To(BeEmpty())
	})
})

var _ = Describe("selectExposed", func() {
	server := func() *Server {
		return &Server{opts: ServerOptions{Logger: discardLogger()}, byName: map[string]toolkit.Tool{}}
	}

	It("Should drop confirmation-gated tools and keep the rest", func() {
		app := fisk.New("app", "an app")
		app.Command("keep", "kept")
		app.Command("danger", "gated").Tag("ai:confirm")

		s := server()
		exposed := s.selectExposed(toolsFor(app))

		names := make([]string, len(exposed))
		for i, t := range exposed {
			names[i] = t.Name()
		}
		Expect(names).To(ConsistOf("keep"))
		Expect(s.byName).To(HaveKey("keep"))
		Expect(s.byName).NotTo(HaveKey("danger"))
	})

	It("Should drop tools gated by a configured confirm tag", func() {
		app := fisk.New("app", "an app")
		app.Command("keep", "kept")
		app.Command("rw", "writes").Tag("impact:rw")

		s := &Server{opts: ServerOptions{Logger: discardLogger(), ConfirmTags: []string{"impact:rw"}}, byName: map[string]toolkit.Tool{}}
		exposed := s.selectExposed(toolsFor(app))
		Expect(exposed).To(HaveLen(1))
		Expect(exposed[0].Name()).To(Equal("keep"))
	})

	It("Should drop a tool that advertises no description", func() {
		app := fisk.New("app", "an app")
		app.Command("keep", "kept")
		app.Command("bare", "")

		s := server()
		exposed := s.selectExposed(toolsFor(app))

		names := make([]string, len(exposed))
		for i, t := range exposed {
			names[i] = t.Name()
		}
		Expect(names).To(ConsistOf("keep"))
		Expect(s.byName).NotTo(HaveKey("bare"))
	})
})

var _ = Describe("resultToToolResult", func() {
	It("Should map a harness error to an error result", func() {
		res := resultToToolResult(nil, errors.New("could not run"))
		Expect(res.IsError).To(BeTrue())
		Expect(res.Output).To(Equal("could not run"))
		Expect(res.Exec).To(BeNil())
	})

	It("Should map a command outcome to a successful result with exec metadata", func() {
		res := resultToToolResult(&toolkit.Outcome{
			Output: "out",
			Exec:   &toolkit.CommandExec{Command: "ping", ExitCode: 2, Truncated: true},
		}, nil)
		Expect(res.IsError).To(BeFalse())
		Expect(res.Output).To(Equal("out"))
		Expect(res.Exec.Command).To(Equal("ping"))
		Expect(res.Exec.ExitCode).To(Equal(2))
		Expect(res.Exec.Truncated).To(BeTrue())
	})

	// An in-process tool produces no exec metadata, and its output must travel
	// verbatim: wrapping it would hand the importing agent a command envelope with a
	// fabricated exit code for a command that never ran.
	It("Should carry an in-process outcome with no exec block", func() {
		res := resultToToolResult(&toolkit.Outcome{Output: `{"status":"ok"}`}, nil)
		Expect(res.IsError).To(BeFalse())
		Expect(res.Output).To(Equal(`{"status":"ok"}`))
		Expect(res.Exec).To(BeNil())
	})

	// This runs on a goroutine serving a remote caller, where a nil dereference
	// would take the process down rather than fail one call.
	It("Should report a nil outcome as an error rather than panicking", func() {
		res := resultToToolResult(nil, nil)
		Expect(res.IsError).To(BeTrue())
		Expect(res.Output).To(Equal("tool returned no result"))
	})

	// A served call has no session to resume and a peer waiting on this reply, so
	// there is nowhere for a later answer to land. The caller is told the endpoint
	// cannot carry the call rather than being handed the tool's own account of what
	// it is waiting for, which would read as a promise the endpoint cannot keep.
	It("Should refuse a deferring tool rather than report its note as a failure", func() {
		res := resultToToolResult(nil, toolkit.DeferResult("waiting on a human", "TKT-1"))
		Expect(res.IsError).To(BeTrue())
		Expect(res.Output).To(Equal(toolkit.ServedDeferralRefusal))
		Expect(res.Output).ToNot(ContainSubstring("TKT-1"))
	})
})

var _ = Describe("normalizeInput", func() {
	It("Should drop empty and null input", func() {
		Expect(normalizeInput(nil)).To(BeNil())
		Expect(normalizeInput(json.RawMessage(`null`))).To(BeNil())
	})

	It("Should keep a real object", func() {
		Expect(normalizeInput(json.RawMessage(`{"a":1}`))).To(Equal(json.RawMessage(`{"a":1}`)))
	})
})

// introspect drives an application's --fisk-introspect handler in-process and
// returns the parsed model with its per-command schemas populated.
func introspect(app *fisk.Application) *fisk.ApplicationModel {
	GinkgoHelper()

	app.Terminate(func(int) {})

	r, w, err := os.Pipe()
	Expect(err).NotTo(HaveOccurred())

	stdout := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = stdout }()

	captured := make(chan []byte, 1)
	go func() {
		data, _ := io.ReadAll(r)
		captured <- data
	}()

	_, err = app.Parse([]string{"--fisk-introspect"})
	Expect(err).NotTo(HaveOccurred())
	Expect(w.Close()).To(Succeed())

	var model fisk.ApplicationModel
	Expect(json.Unmarshal(<-captured, &model)).To(Succeed())

	return &model
}
