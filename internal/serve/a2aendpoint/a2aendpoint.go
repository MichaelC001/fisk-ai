//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// Package a2aendpoint answers other agents over a2a, as endpoints a serve.Server hosts
// rather than as a command of its own.
//
// Two kinds of caller arrive on one transport. A peer that invokes a tool discovers a
// card and calls it directly: no prompt is involved and no agent loop runs, which makes
// it cheaper than handing that peer a prompt and gives it a different security posture,
// since the caller reaches the tools an operator chose to expose and nothing else. A
// peer that sends a prompt gets an agent run: it is acked, the events of the run stream
// back as the loop produces them, and a result or an error closes it.
//
// The two share one transport and one identity, since discovery, tools and tasks are
// paths of a single micro service. Builder opens that transport once and returns
// whichever endpoints the configuration asks for; the first of them to close stops the
// service answering, so one identity leaves its queue group once.
package a2aendpoint

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/a2a"
	wire "github.com/choria-io/fisk-ai/internal/a2a/wire/v1"
	"github.com/choria-io/fisk-ai/internal/conns"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/serve"
	"github.com/choria-io/fisk-ai/internal/telemetry"
	"github.com/choria-io/fisk-ai/internal/toolkit"
	"github.com/choria-io/fisk-ai/internal/toolkit/builtin"
	"github.com/choria-io/fisk-ai/internal/toolkit/fisktool"
)

// Builder describes these endpoints to serve.Endpoints, so a program that wants to
// answer peers links it in and a program that does not never references this package
// at all.
func Builder() serve.EndpointBuilder {
	return serve.EndpointBuilder{
		Name:    "a2a",
		Enabled: func(cfg *config.Config) bool { return cfg.A2AEnabled() },
		// No context: these endpoints take the process's connection from opts and dial
		// nothing of their own, so there is no construction I/O for one to govern.
		Build: func(_ context.Context, cfg *config.Config, opts serve.BuildOptions) ([]serve.Endpoint, error) {
			return NewFromConfig(cfg, ConfigOptions{
				Conns:      opts.Conns,
				ConfigFile: opts.ConfigFile,
				Version:    opts.Version,
				Logger:     opts.Logger,
				Telemetry:  opts.Telemetry,
				Sessions:   opts.Sessions,
			})
		},
	}
}

// ConfigOptions are what a configured endpoint needs that no configuration can state:
// what the process decided, and what it is holding.
type ConfigOptions struct {
	// Conns is the NATS connection to serve on. It is borrowed and never closed here,
	// since the runs and the stores share it.
	Conns *conns.Provider

	// ConfigFile names the file the configuration was read from, so a refusal can name
	// the file to edit.
	ConfigFile string

	// Version is the calling program's own build version, published on the agent card
	// this identity answers discovery with. An empty one publishes the card as "dev".
	Version string

	// Logger receives the endpoints' progress, which is a line per served call and per
	// prompt. Nil leaves it to each server's own default.
	Logger *slog.Logger

	// Telemetry, when non-nil, receives a span per served call and reaches the tools
	// those calls run. It is the process's provider, borrowed like the connection: the
	// program that built it flushes it.
	Telemetry *telemetry.Provider

	// Sessions is the run-journal store, borrowed like the connection. The prompt
	// channel reads it to answer a caller that asks only to be told what a conversation
	// holds, which takes no turn and calls no model. Nil refuses those requests and
	// changes nothing else: every other shape reaches the store through the run.
	Sessions runstate.Store
}

// Options is what New builds from, for a caller assembling the endpoints in Go rather
// than reading them out of a configuration file.
type Options struct {
	// Transport is the binding both endpoints answer on, already opened. New takes
	// ownership of it: the caller releases it by closing the endpoints rather than by
	// closing the transport, and a New that returned an error has already closed it, so
	// a refused caller holds nothing.
	//
	// Nothing else may serve on it. Discovery, tools and tasks are routes on one micro
	// service registration, and a second server registering discovery over the same
	// transport corrupts that service's INFO and STATS.
	Transport a2a.Transport

	// Faults is where the transport reports that it has stopped serving for a reason
	// nobody asked for. Pass the same sink as a2a.TransportConfig.OnFault when opening
	// the transport, since OnFault is fixed at construction and cannot be pointed at a
	// sink afterwards. Channel.Faults and Service.Faults return it, so a serve.Server
	// hosting these endpoints ends and a supervisor restarts the worker.
	//
	// Nil supplies a sink nothing writes to, and a registration the substrate then drops
	// leaves this worker running and answering nothing.
	Faults *FaultSink

	// Identity is the agent name this transport serves under. It is the sender on every
	// reply and the name a peer discovers, and wire.ValidIdentityName must accept it.
	Identity string

	// Version is the calling program's own build version, published on the agent card
	// this identity answers discovery with. An empty one publishes the card as "dev".
	Version string

	// Logger receives the endpoints' progress, which is a line per served call and per
	// prompt. Nil discards it, since a library that reached for a default logger would
	// write to an embedder's stderr uninvited.
	Logger *slog.Logger

	// Telemetry, when non-nil, receives a span per served call and reaches the tools
	// those calls run. It is borrowed: the program that built it flushes it.
	Telemetry *telemetry.Provider

	// Tools builds the tool service, which answers a peer that invokes a tool directly.
	// Nil serves no tools and registers the discovery route alone, so a peer can still
	// ask what this identity is and gets a card with no tools on it.
	Tools *ToolOptions

	// Prompts builds the prompt channel, which hands a peer's prompt to the agent loop.
	// Nil takes no prompts. It needs a Transport that also implements
	// a2a.StreamingTransport, since a prompt is answered with an ack, then the events of
	// the run, then a terminal message.
	Prompts *PromptOptions
}

// ToolOptions describes the tool service: the tools it offers, and the limits a served
// call gets.
type ToolOptions struct {
	// Tools are the tools to offer, of which the service carries those that declare a2a
	// exposure and are not confirmation-gated. At least one is required, and a set that
	// carries none of them is refused rather than served as an agent with no tools.
	Tools []toolkit.Tool

	// ConfirmTags are the operator's tags that, with the always-on ai:confirm, gate a
	// tool behind approval. A served call has nobody to ask, so a tool carrying any of
	// these is never exposed.
	ConfirmTags []string

	// Concurrency is how many served calls run at once; <= 0 uses
	// a2a.DefaultConcurrency. Service.Describe reports the resolved number.
	Concurrency int

	// CallTimeout stops one served call; <= 0 uses a2a.DefaultCallTimeout.
	// Service.Describe reports the resolved duration.
	CallTimeout time.Duration

	// WorkDir is the directory a served command tool runs in, shared by every call.
	// Empty runs them in the process working directory. Pass
	// config.Config.RootDirectory.
	WorkDir string

	// WithheldBuiltins names the built-in tools the program enabled and does not serve,
	// which Service.WithheldBuiltins returns for a startup banner. Tools is the served
	// set; nothing here filters it.
	WithheldBuiltins []string
}

// PromptOptions describes the prompt channel: how much work it admits, how long a
// question to its caller stays open, and what it publishes about the run it hands over.
type PromptOptions struct {
	// Workers is how many prompts run at once. The channel refuses a peer above it and
	// reports it as its concurrency, so the server's slots and the channel's acks agree.
	// It must be greater than zero.
	Workers int

	// Elicit lets a run answering a peer's prompt put questions to that peer. With it
	// false the channel supplies no prompter and the server refuses every
	// confirmation-gated tool, which is what a caller that answers nothing needs.
	Elicit bool

	// PromptWait is how long one question is held open. The channel's own prompter
	// enforces it, since a caller with a person in front of the question restarts the
	// window. It must be greater than zero.
	PromptWait time.Duration

	// MaxTokens is the cumulative token limit on a conversation, carried on every
	// accepting ack so a caller can show how much of the allowance is left. Zero limits
	// nothing. It is the local ceiling: a request that lowers its own budget is clamped
	// by the server, so the ack reports the lower of the two.
	MaxTokens int64

	// Model is what this identity answers a prompt with, published on the agent card. It
	// is required, since a peer choosing whom to send a prompt to reads it off the card.
	Model string

	// Sessions is the run-journal store, borrowed. The channel reads it to answer a
	// caller that asks only to be told what a conversation holds, which takes no turn
	// and calls no model. Nil refuses those requests and changes nothing else: every
	// other shape reaches the store through the run.
	Sessions runstate.Store
}

func (o *Options) validate() error {
	if o.Transport == nil {
		return fmt.Errorf("a Transport is required")
	}
	if !wire.ValidIdentityName(o.Identity) {
		return fmt.Errorf("the Identity %q is not an agent name: it must be letters, digits, '-' and '_', and not empty", o.Identity)
	}
	if o.Tools == nil && o.Prompts == nil {
		return fmt.Errorf("set Tools, Prompts or both: an identity that serves no tools and takes no prompts answers nothing")
	}
	if o.Tools != nil && len(o.Tools.Tools) == 0 {
		return fmt.Errorf("Tools.Tools is empty: the tool service has nothing to offer")
	}

	if o.Prompts != nil {
		if o.Prompts.Workers <= 0 {
			return fmt.Errorf("Prompts.Workers must be greater than zero")
		}
		if o.Prompts.PromptWait <= 0 {
			return fmt.Errorf("Prompts.PromptWait must be greater than zero")
		}
		if o.Prompts.Model == "" {
			return fmt.Errorf("Prompts.Model is required: it is what the agent card says this identity answers a prompt with")
		}
	}

	return nil
}

// Endpoints are what New built, each named so a caller reaches it without asserting a
// type. A field is nil where Options asked for no endpoint of that kind.
type Endpoints struct {
	Channel *Channel
	Service *Service
}

// All returns the endpoints in the order a server hosts them, the channel first, so a
// worker's banner reads in the order work arrives.
//
// An endpoint New did not build is left out rather than handed over as a nil of its
// type: serve.Endpoints sorts a typed nil into its channels, and the server then calls
// Next on a nil receiver.
func (e *Endpoints) All() []serve.Endpoint {
	var built []serve.Endpoint

	if e.Channel != nil {
		built = append(built, e.Channel)
	}
	if e.Service != nil {
		built = append(built, e.Service)
	}

	return built
}

// Close releases the transport the endpoints share, for a caller that built them and
// never handed them to a server. It stops every route this identity answers on, since
// discovery, tools and tasks are paths of one micro service, and it is idempotent.
func (e *Endpoints) Close() error {
	switch {
	case e.Channel != nil:
		return e.Channel.Close()
	case e.Service != nil:
		return e.Service.Close()
	}

	return nil
}

// New builds the endpoints Options asks for over a transport the caller opened: the
// tool service under Tools, the prompt channel under Prompts, and both when both are
// set.
//
// It takes ownership of Options.Transport. The endpoints share it, closing any of them
// closes it, and an error from New has already closed it.
//
// An identity that serves no tools still answers discovery, with a card carrying no
// tools rather than no answer at all.
func New(opts Options) (*Endpoints, error) {
	err := opts.validate()
	if err != nil {
		if opts.Transport != nil {
			releaseTransport(opts.Transport, opts.Logger)
		}

		return nil, err
	}

	faults := opts.Faults
	if faults == nil {
		faults = NewFaultSink()
	}

	held := &sharedTransport{transport: opts.Transport, faults: faults}
	built := &Endpoints{}

	// A channel that is already built is closed rather than the transport under it. Its
	// task route is registered, so a prompt admitted before the failure is waiting on a
	// handoff nothing will pull; closing the channel ends that prompt with a terminal
	// message and waits for it, where closing the transport alone leaves the peer
	// holding an ack until its own deadline runs out.
	release := func() {
		if built.Channel != nil {
			releaseTransport(built.Channel, opts.Logger)
			return
		}

		releaseTransport(held, opts.Logger)
	}

	if opts.Prompts != nil {
		ch, err := newChannel(held, opts)
		if err != nil {
			releaseTransport(held, opts.Logger)
			return nil, err
		}

		built.Channel = ch
	}

	// Discovery is one route on the one micro service this identity registers, so
	// whoever answers it answers for every endpoint here. The tool service owns it when
	// there is one, since its card is the one with tools on it; an identity that only
	// takes prompts still has to say what it is, and gets a card with no tools rather
	// than no answer at all. Registering it twice would not fail: micro would subscribe
	// again on the same subject in the same queue group, and a peer would get whichever
	// card NATS picked.
	if opts.Tools != nil {
		svc, err := newService(held, opts)
		if err != nil {
			release()
			return nil, err
		}

		built.Service = svc
	} else {
		err = serveCard(held, opts)
		if err != nil {
			release()
			return nil, err
		}
	}

	return built, nil
}

// NewFromConfig builds the endpoints expose.agent.a2a asks for: the tool service under
// serve_tools, the prompt channel under prompts, and both when the block carries both.
//
// It refuses a configuration that enables neither, since building an endpoint nobody
// asked for would put an application's commands on the network on the strength of a
// linked builder. It owns the transport it opens, and hands it to the endpoints it
// returns, whose first close stops it.
//
// The endpoints are returned in the order a server hosts them, the channel first, so a
// worker's banner reads in the order work arrives.
func NewFromConfig(cfg *config.Config, opts ConfigOptions) ([]serve.Endpoint, error) {
	if !cfg.A2AEnabled() {
		return nil, fmt.Errorf("expose.agent.a2a enables neither serve_tools nor prompts")
	}
	if opts.Conns == nil {
		return nil, fmt.Errorf("expose.agent.a2a needs a NATS connection, which nats_context is what supplies")
	}

	// The sink is built before the transport, since the transport reports through it and
	// may do so before this function returns.
	faults := NewFaultSink()

	transport, err := a2a.NewTransport(cfg.A2ATransport(), a2a.TransportConfig{
		Resources: opts.Conns,
		Identity:  cfg.Identity,
		Logger:    opts.Logger,
		OnFault:   faults.Report,
	})
	if err != nil {
		return nil, err
	}

	built := Options{
		Transport: transport,
		Faults:    faults,
		Identity:  cfg.Identity,
		Version:   opts.Version,
		Logger:    opts.Logger,
		Telemetry: opts.Telemetry,
	}

	if cfg.A2APromptsEnabled() {
		built.Prompts = &PromptOptions{
			Workers:    cfg.A2APromptsWorkers(),
			Elicit:     cfg.A2APromptsElicit(),
			PromptWait: cfg.A2ARequestTimeout(),
			MaxTokens:  cfg.LLM.Budget.MaxTokens,
			Model:      cfg.LLM.Model,
			Sessions:   opts.Sessions,
		}
	}

	if cfg.A2AServeToolsEnabled() {
		// Loaded on a background context: the process installs its signal handling after
		// the endpoints are built, so a context passed in here is one nothing would
		// cancel. Introspecting the application carries a limit of its own.
		//
		// The tool set is loaded before anything is registered, so a configuration whose
		// filters leave nothing is refused rather than served as an agent with no tools.
		tools, err := fisktool.ServedTools(context.Background(), cfg)
		if err != nil {
			releaseTransport(transport, opts.Logger)
			return nil, err
		}
		if len(tools) == 0 {
			releaseTransport(transport, opts.Logger)
			in := ""
			if opts.ConfigFile != "" {
				in = fmt.Sprintf(" in %q", opts.ConfigFile)
			}
			return nil, fmt.Errorf("no tools available after filtering; check include/exclude%s", in)
		}

		built.Tools = &ToolOptions{
			Tools:            toolkit.Tools(tools),
			ConfirmTags:      cfg.ConfirmTags(),
			Concurrency:      cfg.A2AMaxConcurrentTools(),
			CallTimeout:      cfg.A2AToolTimeout(),
			WorkDir:          cfg.RootDirectory,
			WithheldBuiltins: builtin.WithheldFromA2A(cfg),
		}
	}

	endpoints, err := New(built)
	if err != nil {
		return nil, err
	}

	return endpoints.All(), nil
}

// serveCard answers discovery for an identity that serves no tools, so a peer can ask
// what it is and a caller can read what it does with a conversation before sending one.
//
// It registers the discovery route and nothing else. The server it builds owns no tools
// and answers no tool calls, and it is released with the transport rather than being an
// endpoint of its own, since it produces no work and has nothing to close.
func serveCard(held *sharedTransport, opts Options) error {
	_, err := a2a.NewServer(held.transport, nil, a2a.ServerOptions{
		Identity:      opts.Identity,
		Version:       opts.Version,
		Model:         promptModel(opts),
		Logger:        opts.Logger,
		Telemetry:     opts.Telemetry,
		DiscoveryOnly: true,
	})

	return err
}

// promptModel is the model to publish on the agent card. An identity has one only where
// it answers prompts: serving tools runs no model, so an identity that only does that
// would publish one it never calls.
func promptModel(opts Options) string {
	if opts.Prompts == nil {
		return ""
	}

	return opts.Prompts.Model
}

// sharedTransport is the one transport both endpoints answer on, closed once however
// many of them ask.
//
// Closing it stops the micro service, which takes this identity out of its queue group
// for every path at once. A drain wants exactly that, and the second endpoint's close
// reports the first one's answer rather than a failure, so a clean shutdown prints no
// error for having released one thing twice.
//
// A prompt already accepted is unaffected: its reply inbox and its cancel subscription
// belong to the NATS connection rather than to the service registration, so it goes on
// streaming and still sends its terminal message.
type sharedTransport struct {
	transport a2a.Transport
	once      sync.Once
	err       error

	// faults is where the transport reports that it has stopped serving. Both endpoints
	// return its channel to whoever hosts them.
	faults *FaultSink
}

// FaultSink carries a transport's report that it has stopped serving to the endpoints
// built over it. Pass Report as a2a.TransportConfig.OnFault when opening the transport
// and the sink as Options.Faults; Channel.Faults and Service.Faults return its channel,
// so a serve.Server hosting them ends when the substrate drops the registration and a
// supervisor restarts the worker.
//
// New cannot make the channel itself. OnFault is read once when the transport is
// constructed and a2a.Transport has no setter for it, so only the caller that opened the
// transport can point the callback at the sink these endpoints read.
//
// Build one with NewFaultSink.
type FaultSink struct {
	faults chan error
	once   sync.Once
}

// NewFaultSink returns a sink to pass as a2a.TransportConfig.OnFault and then as
// Options.Faults.
func NewFaultSink() *FaultSink {
	return &FaultSink{faults: make(chan error, 1)}
}

// Report records that the transport has stopped answering. A binding calls it from its
// own goroutine, so it never blocks: the channel is buffered by one and written once,
// the first fault ending the worker and a binding being free to report the same stop
// through more than one handler.
//
// A nil sink, and one built as a zero value rather than by NewFaultSink, discard what
// they are given: a binding must be able to report a fault without knowing whether
// anybody wired a sink up.
func (s *FaultSink) Report(err error) {
	if s == nil || s.faults == nil {
		return
	}

	s.once.Do(func() { s.faults <- err })
}

// Faults yields at most one fault, which is all serve.FaultingEndpoint asks of an
// endpoint. A sink built as a zero value yields nil, which a reader waits on forever.
func (s *FaultSink) Faults() <-chan error { return s.faults }

// transportLines asks the transport how identity is reached. Describing an address is
// optional, so a binding that implements a2a.DescribedTransport names its own and one
// that does not leaves the banner section with only the endpoint's own rows.
func transportLines(t a2a.Transport, identity string) []a2a.DescLine {
	described, ok := t.(a2a.DescribedTransport)
	if !ok {
		return nil
	}

	return described.Describe(identity)
}

// taskLines asks the transport where tasks and their cancels are addressed, on the
// same terms as transportLines.
func taskLines(t a2a.StreamingTransport, identity string, elicits bool) []a2a.DescLine {
	described, ok := t.(a2a.DescribedTransport)
	if !ok {
		return nil
	}

	return described.DescribeTasks(identity, elicits)
}

func (s *sharedTransport) Close() error {
	s.once.Do(func() { s.err = s.transport.Close() })

	return s.err
}

// releaseTransport gives the transport back when the endpoints it was opened for could
// not be built, reporting a second failure to the log: the error that caused the
// teardown is the one the caller needs.
func releaseTransport(c io.Closer, log *slog.Logger) {
	err := c.Close()
	if err != nil && log != nil {
		log.Error("Releasing the a2a transport failed", "error", err)
	}
}
