//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// Package embedded runs a NATS server inside this process with nothing listening, so a
// program can host an agent behind a channel and talk to it over the same protocol a
// remote caller uses.
//
// It exists so that reaching an agent is one path rather than two. A terminal that runs
// its own agent and a terminal that points at a worker somewhere else then differ in
// where the messages go, not in what happens to them, and the local path is exercised
// by everybody rather than being the one nobody tests.
//
// Nothing outside the process can reach it: no port is opened, no cluster, gateway,
// leafnode, websocket or monitoring listener is started, and the connection is made
// through the server object rather than through an address.
package embedded

import (
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	natsd "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/choria-io/fisk-ai/internal/conns"
)

// readyTimeout is how long Start waits for the server to accept connections. It is
// generous for something that binds nothing and starts no store: reaching it means the
// process is in trouble, not that the machine is slow.
const readyTimeout = 10 * time.Second

// drainTimeout is how long Close waits for the connection to finish draining. Both ends
// are in this process and neither is on a network, so a drain that has not finished by
// now is not going to.
const drainTimeout = 5 * time.Second

// productName is what this broker's own connection calls itself. Nothing outside the
// process can see it, since the server does not listen, so it is a literal rather than
// something Start asks its caller for.
const productName = "fisk-ai"

// Broker is the in-process NATS server and the connection to it.
//
// The connection is offered as a conns.Provider so that whatever is hosted on it takes
// its connection the way it takes any other. Give it only to what should be reachable
// in this process: a store or a set of remote tools configured against a real cluster
// belongs on the connection its configuration names, and this server has neither
// JetStream nor a peer.
type Broker struct {
	server *natsd.Server
	conn   *nats.Conn
	conns  *conns.Provider

	// closed is closed by the connection's ClosedHandler, which is what says a drain
	// finished. Drain returns as soon as the drain has started, so without this Close
	// would shut the server down under messages still on their way.
	closed chan struct{}

	// once and err make Close idempotent and make every call report the same outcome,
	// which a deferred close and an explicit one on an error path both need.
	once sync.Once
	err  error
}

// serverOptions is the configuration Start brings the server up with.
//
// It is a function rather than a literal inside Start so the specs can assert what this
// package asks for. That is the whole of the claim that nothing outside the process can
// reach it, and it is not observable on a running server: nats-server exposes its
// addresses but not the options it was given.
func serverOptions() *natsd.Options {
	return &natsd.Options{
		// The whole point: no listener, so the only way in is through the server object
		// below. It skips the client accept loop and nothing else, which is why every
		// other listener is left at its zero value rather than trusted to follow.
		DontListen: true,
		// A server that installs its own signal handler would take SIGINT away from the
		// program hosting it, shut itself down and exit the process, which is not what
		// an interrupt means to a program that has a run in flight.
		NoSigs: true,
		// Nothing hosts a log for this: the process using it owns the terminal, and a
		// server writing to stdout would corrupt output somebody is piping.
		NoLog: true,
	}
}

// Start brings up the server and connects to it. name identifies the connection to the
// server, which is what a person reading a log line or a monitoring page sees, and log
// receives what the connection reports. A nil logger discards it.
//
// The logger is not optional dressing here. A NATS client with no error handler writes
// asynchronous errors straight to standard error, and the pipe between this connection
// and this server closes on the way out, so without one a program that hosts an agent
// prints a line about a closed pipe after every run.
//
// The caller closes it.
func Start(name string, log *slog.Logger) (*Broker, error) {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	server, err := natsd.NewServer(serverOptions())
	if err != nil {
		return nil, fmt.Errorf("starting the in-process broker: %w", err)
	}

	go server.Start()

	if !server.ReadyForConnections(readyTimeout) {
		server.Shutdown()

		return nil, fmt.Errorf("the in-process broker did not become ready within %v", readyTimeout)
	}

	handler := nats.ErrorHandler(func(_ *nats.Conn, sub *nats.Subscription, err error) {
		subject := ""
		if sub != nil {
			subject = sub.Subject
		}

		log.Debug("The in-process broker reported an error", "error", err, "subject", subject)
	})

	// Installed at connect time rather than before the drain, so it cannot be missed:
	// the handler fires on a Close as well as on the end of a drain, and closing the
	// channel more than once would panic.
	closed := make(chan struct{})
	closeOnce := sync.OnceFunc(func() { close(closed) })
	closedHandler := nats.ClosedHandler(func(*nats.Conn) { closeOnce() })

	opts := append(conns.Options(conns.Config{Product: productName, Name: name}), nats.InProcessServer(server), handler, closedHandler)

	conn, err := nats.Connect("", opts...)
	if err != nil {
		server.Shutdown()

		return nil, fmt.Errorf("connecting to the in-process broker: %w", err)
	}

	return &Broker{
		server: server,
		conn:   conn,
		conns:  conns.New(conns.WithNats(conn)),
		closed: closed,
	}, nil
}

// Conns is the connection to this broker, for whatever is hosted on it.
func (b *Broker) Conns() *conns.Provider { return b.conns }

// Close drains the connection and stops the server. It waits for the drain to finish,
// so anything published on the way out reaches its subscriber, which for a run that has
// just ended is its own terminal message. The server is stopped only after that.
//
// The wait is bounded by drainTimeout. Reaching it closes the connection anyway, stops
// the server and returns an error naming the timeout, so a caller is told that messages
// may have been dropped rather than left to assume they were delivered. A drain that
// fails to start is reported the same way.
//
// It is safe to call more than once, which a deferred close and an explicit one on an
// error path add up to. Only the first call does anything and every call returns that
// call's result.
func (b *Broker) Close() error {
	b.once.Do(func() { b.err = b.close() })

	return b.err
}

func (b *Broker) close() error {
	var err error
	if b.conn != nil {
		err = b.drain()
	}

	// The server goes down whatever the drain did, so a failed drain does not leave a
	// server running that nobody holds a handle to any more.
	if b.server != nil {
		b.server.Shutdown()
		b.server.WaitForShutdown()
	}

	return err
}

// drain starts the drain and waits for the connection to report it finished. Drain
// returns once the drain has started, so the ClosedHandler installed in Start is what
// says it is over.
func (b *Broker) drain() error {
	err := b.conn.Drain()
	if err != nil {
		b.conn.Close()

		return fmt.Errorf("draining the in-process broker connection: %w", err)
	}

	select {
	case <-b.closed:
		return nil

	case <-time.After(drainTimeout):
		b.conn.Close()

		return fmt.Errorf("the in-process broker connection did not finish draining within %v", drainTimeout)
	}
}
