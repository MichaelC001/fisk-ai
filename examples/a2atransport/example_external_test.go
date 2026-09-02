//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// These examples write a second a2a transport and drive the engine over it, so the
// question they answer is whether somebody outside this repository can write a binding
// at all. The one in internal/a2a/nats was the only implementation, and an interface
// with one implementation has never been asked whether it is writable.
//
// memory_transport_test.go holds the binding: about two hundred lines carrying
// discovery, a tool call, a task, a cancel and an answer between agents in one process.
// It reaches a2a through the exported surface alone, names no NATS type of its own, and
// the compile-time assertions at the top of it are what turn a method added to any of
// the three interfaces into a build failure here.
package a2atransport_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/choria-io/fisk-ai/internal/a2a"
	"github.com/choria-io/fisk-ai/internal/toolkit"
	"github.com/choria-io/fisk-ai/internal/toolkit/functool"
)

// Example_toolCall serves one tool over the memory transport and calls it from another
// agent: discovery over a2a.Transport's RoundTrip, and the call itself over
// a2a.ReplySetTransport, since every tool answer travels as a reply set.
func Example_toolCall() {
	ctx := context.Background()
	shared := newBus()

	greet, err := functool.New(functool.Spec{
		Name:        "greet",
		Description: "Greets whoever is named in the input",
		Schema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"name": map[string]any{"type": "string"}},
		},
		// A tool is carried on a surface only where it says so; a2a.NewServer drops one
		// that does not and logs why.
		Expose: &functool.ExposeSpec{A2A: true},
		Handler: func(_ context.Context, input json.RawMessage, _ *functool.CallContext) (string, error) {
			var args struct {
				Name string `json:"name"`
			}
			err := json.Unmarshal(input, &args)
			if err != nil {
				return "", err
			}

			return "hello " + args.Name, nil
		},
	})
	if err != nil {
		panic(err)
	}

	// The bus travels in Resources, where the NATS binding takes a *conns.Provider.
	// The engine names no substrate, so a binding says what it needs and nothing else
	// links it.
	serving, err := a2a.NewTransport("memory", a2a.TransportConfig{Resources: shared, Identity: "reporter"})
	if err != nil {
		panic(err)
	}
	defer serving.Close()

	srv, err := a2a.NewServer(serving, toolkit.Tools([]*functool.Tool{greet}), a2a.ServerOptions{
		Identity:  "reporter",
		Version:   "1.0.0",
		LogOutput: io.Discard,
	})
	if err != nil {
		panic(err)
	}

	calling, err := a2a.NewTransport("memory", a2a.TransportConfig{Resources: shared, Identity: "operator"})
	if err != nil {
		panic(err)
	}
	defer calling.Close()

	client, err := a2a.NewClient(calling, "operator")
	if err != nil {
		panic(err)
	}

	card, err := client.Discover(ctx, "reporter")
	if err != nil {
		panic(err)
	}
	fmt.Println("discovered:", card.Name, card.Tools[0].Name)

	reply, err := client.InvokeTool(ctx, "reporter", "greet", json.RawMessage(`{"name":"world"}`))
	if err != nil {
		panic(err)
	}
	fmt.Println("tool said:", reply.Output)

	// Describing an address is optional, and this binding implements it, so the lines
	// the CLI prints come from the transport rather than from anything that knows what
	// a subject or a URL is.
	for _, line := range srv.Describe() {
		fmt.Printf("%s: %s\n", line.Label, line.Value)
	}

	// Output:
	// discovered: reporter greet
	// tool said: hello world
	// Discovery: memory://reporter/discovery
	// Tools: memory://reporter/tools
}

// Example_task runs a task over the memory transport: one request producing an ack, the
// events of the run and a terminal result. This is the half of the binding that
// a2a.StreamingTransport adds, and the served handler is registered through
// ServeReplySet, so it is handed a stream replier rather than asserting for one.
func Example_task() {
	ctx := context.Background()
	shared := newBus()

	worker, err := a2a.NewTransport("memory", a2a.TransportConfig{Resources: shared, Identity: "worker"})
	if err != nil {
		panic(err)
	}
	defer worker.Close()

	// A binding that carries a task says so by implementing the interface; a caller
	// asks once, at startup, rather than per request.
	streaming, ok := worker.(a2a.StreamingTransport)
	if !ok {
		panic("the memory transport carries a reply set")
	}

	err = streaming.ServeReplySet(a2a.OpTask, func(_ context.Context, _ a2a.Caller, body []byte, reply a2a.StreamReplier) {
		var hdr a2a.Header
		err := json.Unmarshal(body, &hdr)
		if err != nil {
			_ = reply.Error("400", err.Error())

			return
		}

		stream := a2a.NewReplyStream(reply, &hdr, "worker")
		_ = stream.Ack(a2a.NewAck(true))
		_ = stream.Event(a2a.NewTextBlock("reading the log"))
		_ = stream.Event(a2a.NewTextBlock("counting the errors"))

		res := a2a.NewResult(a2a.StopEndTurn)
		res.Text = "four errors since midnight"
		_ = stream.Result(res)
	})
	if err != nil {
		panic(err)
	}

	calling, err := a2a.NewTransport("memory", a2a.TransportConfig{Resources: shared, Identity: "operator"})
	if err != nil {
		panic(err)
	}
	defer calling.Close()

	client, err := a2a.NewClient(calling, "operator")
	if err != nil {
		panic(err)
	}

	out, err := client.RunTask(ctx, "worker", a2a.NewRequest("how many errors today"), &printingHandler{})
	if err != nil {
		panic(err)
	}

	fmt.Println("accepted:", out.Ack.Accepted)
	fmt.Println("result:", out.Result.Text)

	// Output:
	// event: reading the log
	// event: counting the errors
	// accepted: true
	// result: four errors since midnight
}

// printingHandler is what a caller running a task supplies: somewhere for the events to
// go, and somebody to put a question to. This one has nobody, which it says rather than
// waiting, so a gated call fails closed instead of costing the run a whole window.
type printingHandler struct{}

func (h *printingHandler) Block(b a2a.Block) {
	text, ok := b.Content().(a2a.TextBlock)
	if !ok {
		return
	}

	fmt.Println("event:", text.Text)
}

func (h *printingHandler) Question(_ context.Context, ask *a2a.ElicitRequest) (*a2a.ElicitReply, error) {
	return a2a.NewNoOperatorReply(ask, "operator"), nil
}
