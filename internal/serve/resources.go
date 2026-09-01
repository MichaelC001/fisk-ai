//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package serve

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/conns"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/mcpclient"
	"github.com/choria-io/fisk-ai/internal/memory"
	"github.com/choria-io/fisk-ai/internal/rag"
	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/telemetry"
)

// productName is what a connection NewResources dials for itself calls itself to a
// NATS server. It dials only when ResourceOptions.Conns is nil, so an embedder that
// cares what its connections are called supplies its own Provider.
const productName = "fisk-ai"

// ResourceOptions are what building the shared resources needs that no configuration
// can state: what the process was told on its command line, and what it already holds.
type ResourceOptions struct {
	// ConfigFile names the file the configuration was read from, so a failure can tell
	// the operator which file to edit. Diagnostics only.
	ConfigFile string

	// ConnName labels this process's NATS connection on the server, so an operator
	// looking at connections can tell one worker from another. Empty uses the identity.
	ConnName string

	// Version is the calling program's own build version, sent to the MCP servers
	// this process connects to. Empty sends no version.
	Version string

	// APIKey and BaseURL address the model provider, as they do on agent.Options. The
	// caller validates BaseURL before calling: the error wants to name the flag or
	// environment variable the operator set, and this package does not know what that
	// was.
	APIKey  string
	BaseURL string

	// Provider, when non-nil, is the model provider the runs share, and APIKey and
	// BaseURL are then unread. Nil builds the one llm_provider names, which is what
	// every fisk-ai command does.
	//
	// A caller supplies one to wrap another with retries or accounting, to script the
	// model in a test, or to reach an endpoint no configuration describes. The
	// alternative is llm.Register, which panics on a duplicate name and is documented
	// for init, so two servers in one process could not each have their own.
	//
	// It arrives with whatever middlewares it was built with. The configured path adds
	// telemetry.HTTPMiddleware for the per-call HTTP spans, and a supplied provider
	// that wants those adds it itself.
	Provider llm.Provider

	// RAG is passed to rag.Open when the knowledge index is opened. Its Embedder is the
	// one thing a knowledge store cannot take from a configuration, so a worker indexing
	// or searching against Ollama, Bedrock, a local model or a test double supplies it
	// here. The zero value opens the store the configuration alone describes.
	//
	// A supplied Embedder turns the vector tier on whatever knowledge.embeddings says,
	// and the identity it pins is checked against the index on every later open, so a
	// worker and whatever wrote the index have to agree on it. See rag.Options.
	RAG rag.Options

	// StoreDir is the base directory the persistent stores resolve relative paths
	// under. Empty keeps each store's own default, which resolves against the process
	// working directory. It must be the same value given to Options.StoreDir, or the
	// stores built here and the paths a run validates disagree.
	StoreDir string

	// Conns, when non-nil, is a NATS connection the caller already holds. It is
	// borrowed: Close leaves it open, because the caller established it and may still
	// be using it. When nil a connection is dialed from the configuration if anything
	// needs one, and that one is closed by Close.
	Conns *conns.Provider

	// Logger receives progress. Nil disables it.
	Logger *slog.Logger
}

// Resources are the values every run of a Server shares rather than building for
// itself. A worker takes work continuously, so a resource built per run is a resource
// built per job, and the expensive ones here are round trips to a NATS server on a
// path where nobody is waiting to be told it was slow.
//
// Building them here rather than in the program is what keeps an embedder from
// reimplementing it. Which resources a configuration calls for, in what order they may
// be built, what has to be validated before anything is dialed and what has to be
// released when one of them fails are all properties of the configuration rather than
// of any one command.
//
// Each field is safe for concurrent use, which is what lets every run share it. The set
// is owned by whoever built it: pass it to a Server with ApplyTo, and Close it after
// Serve returns, never before, since a run still in flight is still using them.
type Resources struct {
	// Conns is the NATS connection the runs share, or nil when nothing a run does
	// needs one. It is not the queue channel's connection, which the channel dials for
	// itself from its own context: the queue engine requires an option the rest of the
	// tree has no reason to carry, and a deployment may keep its work queue on a
	// different cluster from its stores.
	Conns *conns.Provider

	// Provider is the model provider, built once so a long-lived process reuses its
	// connections instead of completing a TLS handshake per job.
	Provider llm.Provider

	// RAGStore is the knowledge index, or nil when knowledge is disabled or the index
	// has not been built yet. Nil for the second reason is deliberate: a store opened
	// against a missing index reports "not built" for as long as it is held, so a
	// worker started before the index was written would answer every search that way
	// until it was restarted. Leaving it nil has each run open its own, which costs an
	// sqlite open per job and picks the index up as soon as it appears.
	RAGStore *rag.Store

	// MemoryStore is the memory store, or nil when memory is disabled.
	//
	// Sharing it is what makes memory.Scope necessary. The jetstream backend gates an
	// overwrite on this run having read the current value, and it can only tell which
	// run read what from the scope agent.Run puts on each run's context. Nothing here
	// has to arrange that; it is noted because the store being shared is what changed.
	MemoryStore memory.Store

	// SessionStore is the run-journal store. It is always built, since checkpointing is
	// not a feature a configuration turns off, and a host that never checkpoints simply
	// never reaches it. Building it here is what moves a missing JetStream stream from
	// the first job to startup.
	SessionStore runstate.Store

	// MCPSessions are the live sessions with the configured MCP servers, or nil when
	// the configuration declares none. One session per server backs every run: a
	// process that opened a stdio child per run would pay that server's startup cost on
	// every job, and give an HTTP server a new session per job for nothing.
	//
	// Every run shares the server's working directory, its authentication and its rate
	// limits, so a server that is stateful per client is stateful across this worker's
	// runs. That is a property of sharing one child rather than a defect in it.
	MCPSessions *mcpclient.Sessions

	// ownsConns records whether Conns was dialed here. A connection the caller supplied
	// is theirs and outlives this set, and conns.Provider cannot answer the question
	// itself: it tracks whether it owns its *nats.Conn, not whether it owns itself.
	ownsConns bool
}

// NewResources builds the resources cfg calls for.
//
// Everything the configuration can be wrong about on its own is settled before a
// connection is dialed, and the stores are built after it, since two of the backends
// need one. A failure at any point releases what was already built rather than leaving
// it to a caller who has no handle on it.
//
// It does I/O: binding a JetStream stream or KV bucket contacts the server, which is
// the point. An operator who has not provisioned their storage finds out here rather
// than one job at a time.
func NewResources(ctx context.Context, cfg *config.Config, opts ResourceOptions) (res *Resources, err error) {
	if cfg == nil {
		return nil, fmt.Errorf("a configuration is required")
	}

	r := &Resources{}

	// Every partial set is released through one path, so a resource added later cannot
	// be forgotten on the error paths of the resources built after it.
	defer func() {
		if err != nil {
			r.closeQuietly(opts.Logger)
			res = nil
		}
	}()

	needsNats := runstate.NeedsNats(cfg.SessionBackend()) ||
		memory.NeedsNats(cfg) ||
		len(cfg.RemoteTools) > 0 ||
		cfg.A2AEnabled()

	if needsNats && opts.Conns == nil && cfg.NatsContext == "" {
		return nil, fmt.Errorf("nats_context is required in %q: the session store, memory store, remote tools or served tools this configuration selects are reached over NATS", opts.ConfigFile)
	}

	// The provider is built first because it contacts nothing: a provider this build
	// does not have is a mistake worth finding before a connection is opened.
	r.Provider, err = newProvider(cfg, opts)
	if err != nil {
		return nil, err
	}

	switch {
	case opts.Conns != nil:
		r.Conns = opts.Conns
	case needsNats:
		connName := opts.ConnName
		if connName == "" {
			connName = cfg.Identity
		}

		r.Conns, err = conns.ConnectNatsContext(ctx, cfg.NatsContext, conns.Config{Product: productName, Name: connName})
		if err != nil {
			return nil, fmt.Errorf("connecting to NATS: %w", err)
		}
		r.ownsConns = true
	}

	err = r.openKnowledge(cfg, opts.StoreDir, opts.RAG, opts.Logger)
	if err != nil {
		return nil, err
	}

	if cfg.MemoryEnabled() {
		r.MemoryStore, err = memory.New(cfg, memory.RuntimeEnv{StoreDir: opts.StoreDir, Nats: r.Conns.Nats()})
		if err != nil {
			return nil, fmt.Errorf("building the memory store: %w", err)
		}
	}

	sessions, err := runstate.New(cfg.SessionBackend(), cfg.SessionRawOptions(), runstate.RuntimeEnv{StoreDir: opts.StoreDir, Nats: r.Conns.Nats()})
	if err != nil {
		return nil, fmt.Errorf("building the session store: %w", err)
	}
	// A worker holds no conversation between turns, so every turn reads its journal back
	// from here. Wrapping it is what makes that cost visible in the run log.
	r.SessionStore = withStoreLogging(sessions, opts.Logger)

	// Last, because it is the only resource here that starts other people's programs. A
	// store that cannot be built therefore fails before a child is running rather than
	// after, and the defer above closes the children when something later fails.
	err = r.connectMCP(ctx, cfg, opts.Version, opts.Logger)
	if err != nil {
		return nil, err
	}

	return r, nil
}

// connectMCP opens a session with every configured MCP server.
//
// A server that will not start or cannot be reached fails the whole build, which is
// what puts it in front of the operator starting the worker instead of in front of
// the first job to arrive. It is the same strictness a run applies, for the same
// reason: the prompts this worker serves may depend on tools that are not there.
//
// The context governs the connecting and nothing after it. A caller that gives up
// while a server is starting gets its error back, and the sessions that were opened
// outlive it, since a worker's shared resources are not scoped to any one request.
// Each entry's timeout bounds its own connect as well, so a server that never answers
// fails this naming it instead of holding startup open.
func (r *Resources) connectMCP(ctx context.Context, cfg *config.Config, version string, log *slog.Logger) error {
	if len(cfg.MCPClients) == 0 {
		return nil
	}

	sessions, err := mcpclient.Connect(ctx, mcpclient.Options{
		Servers:            cfg.MCPClients,
		Identity:           cfg.Identity,
		Version:            version,
		CredentialEnvNames: cfg.CredentialEnvNames(),
	})
	if err != nil {
		return err
	}

	r.MCPSessions = sessions

	if log != nil {
		log.Info("Connected the configured MCP servers", "servers", sessions.Names())
	}

	return nil
}

// newProvider returns the model provider the runs share: the one the caller supplied, or
// one built from the configuration.
//
// The built one carries the telemetry hook and nothing else. agent.Run assembles that hook
// only on the path where it builds a provider itself, so a provider handed to it arrives
// with whatever hooks it was built with and no others: leaving this out would turn sharing
// a provider into silently dropping every per-call HTTP span. The hook holds no state of
// its own, reading the span it annotates off the request's context, so one instance serves
// concurrent runs correctly. A supplied provider is taken as the caller built it, hook
// included or not.
//
// The debug dump and the request tracer are not here, and that is not an oversight:
// both write a per-run file, which a process serving many runs at once has nowhere to
// put.
func newProvider(cfg *config.Config, opts ResourceOptions) (llm.Provider, error) {
	if opts.Provider != nil {
		return opts.Provider, nil
	}

	provider, err := llm.NewProvider(cfg.LLMProvider(), llm.Config{
		APIKey:      opts.APIKey,
		BaseURL:     opts.BaseURL,
		Timeout:     cfg.LLM.Budget.CallTimeoutParsed,
		Middlewares: []llm.Middleware{telemetry.HTTPMiddleware()},
	})
	if err != nil {
		return nil, fmt.Errorf("building the model provider: %w", err)
	}

	return provider, nil
}

// openKnowledge opens the knowledge index, keeping it only when there is one to keep.
//
// A read-only store resolves its index once and holds it, so an index that appears
// afterwards is never seen by this store. That is right for a command that runs and
// exits and wrong for a worker that runs for weeks, so an index that is not there yet is
// released here and left to the runs, which open one each and see it the moment it
// exists. An index that is there is shared, since a reindex writes the same file and a
// reader sees the committed result.
func (r *Resources) openKnowledge(cfg *config.Config, storeDir string, ragOpts rag.Options, log *slog.Logger) error {
	if !cfg.RAGEnabled() {
		return nil
	}

	store, err := rag.Open(cfg, storeDir, ragOpts)
	if err != nil {
		return fmt.Errorf("opening the knowledge index: %w", err)
	}

	if store.Built() {
		r.RAGStore = store

		return nil
	}

	closeErr := store.Close()
	if closeErr != nil && log != nil {
		log.Warn("Releasing the unbuilt knowledge index failed", "error", closeErr)
	}

	if log != nil {
		log.Info("Knowledge is enabled but no index has been built; each run will look for one", "path", store.Path())
	}

	return nil
}

// ApplyTo puts the resources on a Server's options.
//
// It overwrites every field it sets, so a caller who wants to keep one of their own
// assigns it after this rather than before. It exists so that a resource added here
// later is not an edit in every program that builds one.
//
// It does not set Telemetry, which the program builds because reporting what was
// exported is the program's business, nor A2ATransport, which a run still constructs
// for itself. StoreDir is not set either: it is an input to building these, not a
// product of it, so pass the same value to both.
func (r *Resources) ApplyTo(o *Options) {
	if r == nil || o == nil {
		return
	}

	o.Conns = r.Conns
	o.Provider = r.Provider
	o.RAGStore = r.RAGStore
	o.MemoryStore = r.MemoryStore
	o.SessionStore = r.SessionStore
	o.MCPSessions = r.MCPSessions
}

// Close releases what was built here and nothing else. A connection the caller supplied
// through ResourceOptions.Conns is left open, since it is theirs.
//
// It must not be called until Serve has returned: the runs it hosted use these, and
// Serve does not return while one is in flight.
//
// The provider and the two stores have nothing to release. The knowledge index holds
// a file handle, the connection holds a socket, and the MCP sessions hold a child
// process each.
//
// Server.Drain releases the channels and the services and deliberately leaves these
// alone, so a run that is still in flight still has the servers its tools call. The
// MCP sessions are ended here and never by a run: a run that closed them would take
// the next run's servers down with it.
func (r *Resources) Close() error {
	if r == nil {
		return nil
	}

	var errs []error

	// First, since a stdio child gets a terminate window to exit in and nothing else
	// here is waiting on it.
	if r.MCPSessions != nil {
		err := r.MCPSessions.Close()
		if err != nil {
			errs = append(errs, fmt.Errorf("closing the mcp sessions: %w", err))
		}
		r.MCPSessions = nil
	}

	if r.RAGStore != nil {
		err := r.RAGStore.Close()
		if err != nil {
			errs = append(errs, fmt.Errorf("releasing the knowledge index: %w", err))
		}
		r.RAGStore = nil
	}

	// Last, because the stores above borrow it.
	if r.ownsConns && r.Conns != nil {
		r.Conns.Close()
		r.Conns = nil
		r.ownsConns = false
	}

	return errors.Join(errs...)
}

// closeQuietly releases a partially built set, reporting a failure to the log rather
// than returning it: the error that caused the teardown is the one the caller needs.
func (r *Resources) closeQuietly(log *slog.Logger) {
	err := r.Close()
	if err != nil && log != nil {
		log.Error("Releasing partially built resources failed", "error", err)
	}
}
