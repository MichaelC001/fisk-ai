//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// Package web hosts an agent behind an HTTP listener a browser talks to: a page POSTs a
// turn, the run streams back on the response, and the thread the page named is one
// conversation for as long as the page keeps posting to it.
//
// The channel holds the listener, the routes, CORS and the rule that a question ends
// the turn. It never names a frontend protocol: each one is a Format mounted on a path
// under the base path, and the channel decodes every request through the Format the
// path belongs to.
//
// # A question ends the turn
//
// A page is not an operator. It is reading the response, so it cannot answer until the
// response is over. A question the run asks is therefore put on the response that ends
// the turn, the call it belongs to is left unanswered, and the answer arrives on the
// page's next request, which resumes the conversation and dispatches the same call
// again. Nothing persists a question or its answer: the answer is held for the one turn
// that carries it.
//
// # No per-thread state
//
// Several processes may serve one page behind a load balancer, so nothing here is
// remembered between requests. A thread is a session in the store, a held answer lives
// for one turn, and a second turn on a thread already running is refused by the
// journal's own claim rather than by a map in one process.
//
// # CORS is not an access control
//
// A cross-origin POST still reaches the handler and still runs a turn; the browser only
// withholds the answer from the page. So a request carrying an Origin outside the
// configured list is refused before its body is read, and a listener on a loopback
// address refuses a request whose Host is not that address, which is what stops a page
// reaching it through DNS rebinding. On any other address the Host check is skipped,
// since a deployment there sits behind a TLS proxy that passes the browser's Host
// through unchanged.
package web

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/choria-io/fisk-ai/internal/runstate"
	"github.com/choria-io/fisk-ai/internal/serve"
)

// Defaults for a Channel built in process. NewFromConfig reads the configuration's
// accessors, which carry the same values.
const (
	// DefaultListen is the address the channel binds when Options names none. It is
	// loopback because an empty http.Server.Addr binds every interface on port 80.
	DefaultListen = "127.0.0.1:8080"
	// DefaultBasePath is the prefix the formats are mounted under when Options names
	// none.
	DefaultBasePath = "/fisk/v1"
	// DefaultWorkers is how many turns run at once when Options sets none. It is five
	// for the reason the Slack channel's is: a turn is a person waiting, and at one
	// worker the second person to ask anything waits for the first person's run.
	DefaultWorkers = 5
)

// channelName identifies this channel in the server's logs, its metrics and a worker's
// startup banner.
const channelName = "web"

// This channel is every one of the optional shapes a channel can have: it sizes its own
// concurrency, it holds a listener to release, its listener can die while it runs, and
// it names its address on a startup banner. Declaring them makes a change to any of
// those contracts a compile error here rather than a channel the server silently stops
// asking.
var (
	_ serve.ConcurrentChannel = (*Channel)(nil)
	_ serve.ReleasableChannel = (*Channel)(nil)
	_ serve.FaultingEndpoint  = (*Channel)(nil)
	_ serve.DescribedEndpoint = (*Channel)(nil)
)

// Options configures a Channel.
type Options struct {
	// Listen is the host and port to bind. Empty binds DefaultListen.
	Listen string

	// BasePath is the prefix every Format is mounted under. It must start with a slash;
	// a trailing one is dropped. Empty takes DefaultBasePath.
	BasePath string

	// Origins lists the pages allowed to read the answers, each as a browser sends it
	// in the Origin header: scheme, host and port with no path. Required, and a
	// wildcard is refused: the list is the whole of what decides which page may read an
	// answer.
	Origins []string

	// Identity is the agent name hashed into every session this channel derives, so two
	// agents serving one page keep their conversations apart. It is normally the
	// configured agent identity and is required.
	Identity string

	// Workers is how many turns run at once, which is also how many the server is told
	// to allow through Concurrency. A request arriving above it is refused with a 503
	// rather than taken and left waiting for a slot with no response head. Zero or less
	// takes DefaultWorkers.
	Workers int

	// Formats are the frontend protocols to mount, each on its path under BasePath.
	// None mounts nothing, and the channel then refuses every path.
	Formats []Mount

	// Sessions is the run-journal store, borrowed and never closed here since the runs
	// write to the same one. It is required: a thread is a conversation, and this
	// channel reads the store to tell a thread it holds from one it is opening.
	Sessions runstate.Store

	// SuspendRequested is handed to every run and polled at a loop boundary, so a
	// worker draining stops its turns where they can be resumed from. Nil never
	// suspends.
	SuspendRequested func() bool

	// Logger receives the channel's progress, which is a line per turn. Nil builds a
	// text logger on stderr.
	Logger *slog.Logger
}

func (o *Options) applyDefaults() {
	if o.Listen == "" {
		o.Listen = DefaultListen
	}
	if o.BasePath == "" {
		o.BasePath = DefaultBasePath
	}
	if o.Workers <= 0 {
		o.Workers = DefaultWorkers
	}
}

func (o *Options) validate() error {
	if o.Identity == "" {
		return fmt.Errorf("an identity is required: it names the journals this channel's threads run in")
	}
	if o.Sessions == nil {
		return fmt.Errorf("a session store is required: a thread is a conversation, so a channel with nowhere to journal one could answer a first request and nothing after it")
	}
	if !strings.HasPrefix(o.BasePath, "/") {
		return fmt.Errorf("the base path %q must start with a slash: ServeMux reads one without as a host and a path and then 404s every request", o.BasePath)
	}

	if len(o.Origins) == 0 {
		return fmt.Errorf("at least one origin is required: it is the list of pages allowed to read the answers")
	}
	for _, origin := range o.Origins {
		err := checkOrigin(origin)
		if err != nil {
			return err
		}
	}

	seen := map[string]bool{}
	for _, m := range o.Formats {
		switch {
		case m.Path == "" || strings.Contains(m.Path, "/"):
			return fmt.Errorf("a format is mounted at %q, which is not a single path segment", m.Path)
		case m.Format == nil:
			return fmt.Errorf("the format mounted at %q is nil", m.Path)
		case seen[m.Path]:
			return fmt.Errorf("two formats are mounted at %q", m.Path)
		}

		seen[m.Path] = true
	}

	return nil
}

// checkOrigin holds one entry to what a browser sends: a scheme, a host, an optional
// port and nothing else. An entry with a path could never match, and a wildcard would
// let any page read the answers.
func checkOrigin(origin string) error {
	if origin == "*" {
		return fmt.Errorf("an origin of %q would let any page read the answers; list each page instead", origin)
	}

	u, err := url.Parse(origin)
	if err != nil {
		return fmt.Errorf("origin %q: %w", origin, err)
	}
	if u.Scheme == "" || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return fmt.Errorf("origin %q is not scheme://host[:port] as a browser sends it", origin)
	}

	return nil
}

// Channel is a serve.Channel over an HTTP listener.
type Channel struct {
	identity string
	basePath string
	workers  int
	routes   []string
	suspend  func() bool
	sessions runstate.Store
	log      *slog.Logger

	// origins is the list as a set, for the check every request makes, and originList
	// the list as configured, for the banner.
	origins    map[string]struct{}
	originList []string

	// listener is bound at construction, so a busy port fails before the banner rather
	// than after it. loopback and port are read off it for the Host check.
	listener net.Listener
	loopback bool
	port     string
	server   *http.Server

	// work hands one admitted turn to Next. It is unbuffered: admission has already
	// reserved the slot, so the wait here is for the server's puller to come round, and
	// the request's own goroutine waits it out.
	work chan *serve.Work

	// mu guards inFlight, the count admission refuses above.
	mu       sync.Mutex
	inFlight int

	// faults carries the report that the listener has stopped answering. It is buffered
	// by one and written once, the first fault being what ends the worker.
	faults    chan error
	faultOnce sync.Once

	// The listener is served from the first Next, so a channel built and never served
	// answers no request, and Close waits for the serving goroutine to end.
	startOnce sync.Once
	started   atomic.Bool
	served    chan struct{}

	closeOnce sync.Once
	closeErr  error
	shutdown  chan struct{}
}

// New binds the listener and returns a Channel. It serves nothing until the first call
// to Next, so a channel that is built and never served accepts no request, and a port
// already in use fails here rather than on the first request.
func New(opts Options) (*Channel, error) {
	opts.applyDefaults()

	err := opts.validate()
	if err != nil {
		return nil, err
	}

	log := opts.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	log = log.With("channel", channelName)

	listener, err := net.Listen("tcp", opts.Listen)
	if err != nil {
		return nil, fmt.Errorf("binding %s: %w", opts.Listen, err)
	}

	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		listener.Close()

		return nil, fmt.Errorf("reading the bound address %q: %w", listener.Addr(), err)
	}

	c := &Channel{
		identity:   opts.Identity,
		basePath:   strings.TrimSuffix(opts.BasePath, "/"),
		workers:    opts.Workers,
		suspend:    opts.SuspendRequested,
		sessions:   opts.Sessions,
		log:        log,
		origins:    make(map[string]struct{}, len(opts.Origins)),
		originList: append([]string(nil), opts.Origins...),
		listener:   listener,
		loopback:   isLoopback(host),
		port:       port,
		work:       make(chan *serve.Work),
		faults:     make(chan error, 1),
		served:     make(chan struct{}),
		shutdown:   make(chan struct{}),
	}

	for _, origin := range opts.Origins {
		c.origins[origin] = struct{}{}
	}

	c.server = &http.Server{Handler: c.handler(opts.Formats)}

	log.Info("Answering in a browser",
		"identity", opts.Identity,
		"listen", listener.Addr().String(),
		"base_path", c.basePath,
		"origins", c.originList,
		"routes", c.routes,
		"workers", opts.Workers)

	return c, nil
}

// isLoopback reports whether a bound host is a loopback address, which decides whether
// the Host header is checked.
func isLoopback(host string) bool {
	ip := net.ParseIP(host)

	return ip != nil && ip.IsLoopback()
}

// Name identifies the channel in the server's logs.
func (c *Channel) Name() string { return channelName }

// Concurrency is how many turns this channel may have running at once, which is also
// the number above which a request is refused rather than taken.
func (c *Channel) Concurrency() int { return c.workers }

// Addr is the address the listener is bound to, which for a configuration naming port
// zero is the port the system chose.
func (c *Channel) Addr() string { return c.listener.Addr().String() }

// Next blocks until a request has been admitted and returns it as work.
//
// It starts serving the listener on its first call, so nothing is accepted before the
// server is ready to run it. It returns serve.ErrChannelDone once the channel has been
// closed, so the server stops asking an endpoint that no longer answers.
func (c *Channel) Next(ctx context.Context) (*serve.Work, error) {
	c.start()

	select {
	case w := <-c.work:
		return w, nil
	case <-c.shutdown:
		return nil, serve.ErrChannelDone
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// start serves the listener once. A listener that stops for a reason nobody asked for
// faults the worker, since no request can arrive again.
func (c *Channel) start() {
	c.startOnce.Do(func() {
		c.started.Store(true)

		go func() {
			defer close(c.served)

			err := c.server.Serve(c.listener)
			if err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
				c.log.Error("The listener stopped", "error", err)
				c.fault(err)
			}
		}()
	})
}

// Faults reports that the listener has stopped answering for a reason nobody asked
// for.
func (c *Channel) Faults() <-chan error { return c.faults }

// fault records that the listener has stopped. It is called from the serving goroutine,
// so it must not block: the channel is buffered and written once.
func (c *Channel) fault(err error) {
	c.faultOnce.Do(func() { c.faults <- err })
}

// Close stops the channel taking requests and releases the listener.
//
// A request already handed to the server is a run in flight, which the server waits
// for rather than this channel: its response stays open until the run reports, and the
// connection closes after it, since keep-alives are turned off here. A request still
// waiting to be handed over is answered with a 503.
//
// It is idempotent and returns the same answer to every caller, since a program that
// drains on one signal and stops on the next releases every endpoint twice.
func (c *Channel) Close() error {
	c.closeOnce.Do(func() {
		// Shutdown first, so a request arriving while the listener is still open is
		// refused rather than admitted to a run nothing will take.
		close(c.shutdown)

		c.server.SetKeepAlivesEnabled(false)

		err := c.listener.Close()
		if err != nil && !errors.Is(err, net.ErrClosed) {
			c.closeErr = fmt.Errorf("closing the listener: %w", err)
		}

		if c.started.Load() {
			<-c.served
		}
	})

	return c.closeErr
}

// draining reports that this channel has been closed, so a request is refused and the
// turns in flight reach an ending the shutdown can wait for.
func (c *Channel) draining() bool {
	select {
	case <-c.shutdown:
		return true
	default:
		return false
	}
}

// suspendRequested is what every turn's run reads at each loop boundary: the worker's
// own drain signal, and this channel being closed. Each parks the run somewhere a later
// request on the thread continues from.
func (c *Channel) suspendRequested() bool {
	if c.suspend != nil && c.suspend() {
		return true
	}

	return c.draining()
}

// admit reserves a slot for one request, reporting false when every slot is taken.
func (c *Channel) admit() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.inFlight >= c.workers {
		return false
	}
	c.inFlight++

	return true
}

// release gives a slot back.
func (c *Channel) release() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.inFlight--
}

// Heading names this endpoint on a startup banner.
func (c *Channel) Heading() string { return "Answering in a browser" }

// Describe names the address this channel is bound to, the pages allowed to read its
// answers, the routes it mounted and how many turns it runs at once, for the banner a
// worker prints before its log takes over. The address is the bound one rather than the
// configured one, so a configuration naming port zero prints the port it got.
func (c *Channel) Describe() []serve.DescLine {
	routes := "none"
	if len(c.routes) > 0 {
		routes = strings.Join(c.routes, ", ")
	}

	return []serve.DescLine{
		{Label: "Listen", Value: c.Addr()},
		{Label: "Base Path", Value: c.basePath},
		{Label: "Origins", Value: strings.Join(c.originList, ", ")},
		{Label: "Routes", Value: routes},
		{Label: "Workers", Value: strconv.Itoa(c.workers)},
	}
}
