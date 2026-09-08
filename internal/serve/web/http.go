//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package web

import (
	"fmt"
	"net"
	"net/http"
	"strings"
)

const (
	// maxRequestBytes limits one request body. A turn is a prompt and an answer, and a
	// body past a megabyte is not one.
	maxRequestBytes = 1 << 20

	// preflightMaxAge is how long a browser may cache a preflight answer, in seconds.
	preflightMaxAge = "600"

	// allowedMethods is what a preflight is told a page may send: a turn is a POST, the
	// card and the session list and a stored conversation are GETs, and deleting a
	// conversation is a DELETE. A method left out here is one a browser refuses to send
	// cross-origin however the route answers it.
	allowedMethods = "GET, POST, DELETE, OPTIONS"

	// retryAfter is what a refused request is told to wait, in seconds, before sending
	// again.
	retryAfter = "5"
)

// The lines this channel says for itself, as opposed to anything a run produced. Each
// says what happened and what to do about it, and none names a session, a worker or an
// error.
const (
	drainingRefusal = "this worker is shutting down and has not taken this turn; send it again once it is back"
	busyRefusal     = "a turn of this conversation is already running; wait for its response to end before sending another"
	capacityRefusal = "this worker is running as many turns as it can; send this one again in a moment"
	unknownRefusal  = "this thread holds no conversation to answer; send a prompt to start one"
	storeRefusal    = "the stored conversation could not be read; send this turn again in a moment"
	stoppedRefusal  = "the worker stopped before this turn started; send it again"
)

// handler builds the routes, behind the checks every request passes first: a POST per
// mounted format under the base path, a GET beside it for a stored conversation that
// format renders, and the agent card and the session list the formats know nothing
// about.
//
// The order keeps a refusal cheap. A drain and an unlisted Origin are refused
// on the headers alone, the Host check follows, and only a request that passed all
// three reaches a format's decoder and the body.
func (c *Channel) handler(formats []Mount) http.Handler {
	mux := http.NewServeMux()

	for _, m := range formats {
		route := c.basePath + "/" + m.Path
		c.routes = append(c.routes, "POST "+route)
		mux.HandleFunc("POST "+route, c.serveTurn(m.Format))

		// A stored conversation is opened under the format that renders it, since a
		// browser reads AI SDK parts or AG-UI messages and the channel writes neither.
		openRoute := route + "/" + SessionsPath + "/{id}"
		c.routes = append(c.routes, "GET "+openRoute)
		mux.HandleFunc("GET "+openRoute, c.serveSessionOpen(m.Format))
	}

	cardRoute := c.basePath + "/" + CardPath
	c.routes = append(c.routes, "GET "+cardRoute)
	mux.HandleFunc("GET "+cardRoute, c.serveCard)

	listRoute := c.basePath + "/" + SessionsPath
	c.routes = append(c.routes, "GET "+listRoute)
	mux.HandleFunc("GET "+listRoute, c.serveSessionList)

	deleteRoute := listRoute + "/{id}"
	c.routes = append(c.routes, "DELETE "+deleteRoute)
	mux.HandleFunc("DELETE "+deleteRoute, c.serveSessionDelete)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c.draining() {
			w.Header().Set("Retry-After", retryAfter)
			http.Error(w, drainingRefusal, http.StatusServiceUnavailable)

			return
		}

		origin, ok := c.allowOrigin(w, r)
		if !ok {
			c.log.Warn("Refusing a request from an unlisted origin", "origin", origin, "remote", r.RemoteAddr)
			http.Error(w, fmt.Sprintf("origin %q may not read this agent's answers", origin), http.StatusForbidden)

			return
		}

		if c.refusesHost(r.Host) {
			c.log.Warn("Refusing a request whose Host is not the loopback listener", "host", r.Host, "remote", r.RemoteAddr)
			http.Error(w, fmt.Sprintf("host %q does not name this listener", r.Host), http.StatusForbidden)

			return
		}

		if origin != "" && r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			c.preflight(w, r)

			return
		}

		mux.ServeHTTP(w, r)
	})
}

// allowOrigin decides the CORS answer from the Origin header, and returns the origin
// with whether the request may proceed. A request carrying none is not a browser's
// cross-origin request and passes; one carrying an origin outside the list is refused,
// and one inside it is answered with the headers that let the page read the response.
//
// The list is enforced here rather than left to the browser: CORS decides who may read
// the answer, while a cross-origin POST still reaches the handler and would still run a
// turn.
func (c *Channel) allowOrigin(w http.ResponseWriter, r *http.Request) (string, bool) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return "", true
	}

	_, listed := c.origins[origin]
	if !listed {
		return origin, false
	}

	h := w.Header()
	h.Set("Access-Control-Allow-Origin", origin)
	h.Add("Vary", "Origin")
	// The page reads the protocol version off a format's own headers, and a browser
	// hides every header but the simple ones unless told otherwise.
	h.Set("Access-Control-Expose-Headers", "*")

	return origin, true
}

// preflight answers a browser's CORS preflight for an allowed origin. The headers the
// page asked to send are echoed back, since the format decides what a request carries
// and nothing here has a reason to refuse one.
func (c *Channel) preflight(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Access-Control-Allow-Methods", allowedMethods)
	h.Set("Access-Control-Max-Age", preflightMaxAge)

	asked := r.Header.Get("Access-Control-Request-Headers")
	if asked == "" {
		asked = "Content-Type"
	}
	h.Set("Access-Control-Allow-Headers", asked)

	w.WriteHeader(http.StatusNoContent)
}

// refusesHost reports whether a request's Host has to be refused: the listener is on a
// loopback address and the Host names anything but a loopback address on the bound
// port.
//
// A page reaches a loopback listener as 127.0.0.1, ::1 or localhost, and every one of
// them is accepted. A name that resolved to loopback in the browser's DNS is refused,
// which is how a page on the internet reaches a listener that was never meant to be
// reachable from it. On any other address the check is skipped: the proxy in front
// owns the public name and passes the browser's Host through unchanged.
func (c *Channel) refusesHost(host string) bool {
	if !c.loopback {
		return false
	}

	name, port, err := net.SplitHostPort(host)
	if err != nil {
		name = host
		port = "80"
	}

	name = strings.Trim(name, "[]")
	if !strings.EqualFold(name, "localhost") && !isLoopback(name) {
		return true
	}

	return port != c.port
}

// serveTurn answers one request on a mounted format: decode it, decide how it joins its
// thread, hand it to the server and hold the response open until the run reports.
//
// The response head is not written here. A run that fails before its journal is claimed
// reaches no event, and the ending then answers with a status code, so a second turn on
// a thread already running is refused as a 409 rather than a 200 with an empty body.
func (c *Channel) serveTurn(f Format) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)

		in, writer, err := f.Decode(w, r)
		if err != nil {
			c.log.Warn("Refusing a request the format could not decode", "error", err, "remote", r.RemoteAddr)
			http.Error(w, err.Error(), http.StatusBadRequest)

			return
		}

		err = checkTurn(in)
		if err != nil {
			c.log.Warn("Refusing a request", "error", err, "remote", r.RemoteAddr)
			http.Error(w, err.Error(), http.StatusBadRequest)

			return
		}

		session := SessionFor(c.identity, in.ThreadID)
		log := c.log.With("session", session, "caller", in.Caller)

		held, err := c.held(r.Context(), session)
		if err != nil {
			log.Error("Refusing a request whose conversation could not be read", "error", err)
			http.Error(w, storeRefusal, http.StatusInternalServerError)

			return
		}

		// A thread the store does not hold has no question to answer, and a prompt is
		// the only thing that opens one.
		if !held && (in.Prompt == "" || in.Answer != nil) {
			log.Info("Refusing an answer for a thread that holds no conversation")
			http.Error(w, unknownRefusal, http.StatusNotFound)

			return
		}

		if !c.admit() {
			log.Info("Refusing a request above the worker count", "workers", c.workers)
			w.Header().Set("Retry-After", retryAfter)
			http.Error(w, capacityRefusal, http.StatusServiceUnavailable)

			return
		}
		defer c.release()

		t := c.newTurn(in, session, held, w, writer, log)

		select {
		case c.work <- t.work():
		case <-c.shutdown:
			log.Info("Refusing a request admitted while the worker was stopping")
			http.Error(w, drainingRefusal, http.StatusServiceUnavailable)

			return
		case <-r.Context().Done():
			log.Info("A request left before its turn was handed over")

			return
		}

		// The run writes to the response from its own goroutine and the ending closes
		// this, so the request stays open for as long as the run does whether or not
		// the page is still reading: a closed tab does not cancel a conversation.
		<-t.done
	}
}

// checkTurn refuses a turn the channel cannot act on: no thread, nothing to do, or an
// answer naming no call.
func checkTurn(in Turn) error {
	switch {
	case in.ThreadID == "":
		return fmt.Errorf("the request names no thread")
	case in.Prompt == "" && in.Answer == nil:
		return fmt.Errorf("the request carries neither a prompt nor an answer")
	case in.Answer != nil && in.Answer.ToolUseID == "":
		return fmt.Errorf("the answer names no tool call")
	}

	return nil
}
