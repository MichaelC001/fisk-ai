//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2aendpoint

import (
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/choria-io/fisk-ai/internal/a2a"
	"github.com/choria-io/fisk-ai/internal/serve"
)

// A tool service answers its callers directly and produces no work, it stops answering
// when its registration is dropped, and it names its subjects and its limits on a
// startup banner.
var (
	_ serve.Service           = (*Service)(nil)
	_ serve.FaultingEndpoint  = (*Service)(nil)
	_ serve.DescribedEndpoint = (*Service)(nil)
)

// Service is the tool-serving endpoint: an a2a server, the transport it answers on, and
// what a program needs to describe it at startup.
type Service struct {
	srv      *a2a.Server
	held     *sharedTransport
	exposed  []string
	withheld []string
	describe []serve.DescLine
}

// newService builds the tool-serving endpoint Options.Tools describes over the
// transport its siblings share.
func newService(held *sharedTransport, opts Options) (*Service, error) {
	// Resolved here rather than left to a2a.NewServer, which fills its own defaults in
	// and never reports them: the banner names the limits a served call will actually
	// get, and printing zero for a worker that in fact stops every call at thirty
	// seconds is worse than printing nothing.
	concurrency := opts.Tools.Concurrency
	if concurrency <= 0 {
		concurrency = a2a.DefaultConcurrency()
	}

	callTimeout := opts.Tools.CallTimeout
	if callTimeout <= 0 {
		callTimeout = a2a.DefaultCallTimeout
	}

	svc := &Service{
		held:     held,
		withheld: opts.Tools.WithheldBuiltins,
		describe: describeService(transportLines(held.transport, opts.Identity), concurrency, callTimeout),
	}

	srv, err := a2a.NewServer(held.transport, opts.Tools.Tools, a2a.ServerOptions{
		Identity:    opts.Identity,
		Version:     opts.Version,
		Model:       promptModel(opts),
		ConfirmTags: opts.Tools.ConfirmTags,
		Concurrency: concurrency,
		CallTimeout: callTimeout,
		Logger:      opts.Logger,
		Telemetry:   opts.Telemetry,
	})
	if err != nil {
		return nil, err
	}

	svc.srv = srv

	svc.exposed = svc.srv.ExposedTools()
	if len(svc.exposed) == 0 {
		return nil, fmt.Errorf("no tools available to serve over a2a; all were filtered or confirmation-gated")
	}

	return svc, nil
}

// Name identifies the endpoint. There is one tool service per identity and the identity
// is what a program has already named by the time it prints this, so nothing qualifies
// it further.
func (s *Service) Name() string { return "a2a" }

// Close stops answering, which stops the micro service and takes this identity out of
// its queue group, so a peer calling during a drain is routed to a sibling rather than
// left waiting. The prompt channel beside it stops answering at the same moment, both
// being paths of the one service.
//
// A call already in flight is not covered. The a2a server answers on a goroutine of its
// own, which only ToolOptions.CallTimeout stops, so a command it started goes on running
// with nowhere to reply to.
func (s *Service) Close() error { return s.held.Close() }

// Faults reports that this identity has stopped answering for a reason nobody asked
// for. Both endpoints share one transport, so both report the same stop and whichever
// the server reads first ends it.
func (s *Service) Faults() <-chan error { return s.held.faults.Faults() }

// ExposedTools are the tools this endpoint serves, in card order. It answers with a copy,
// so a caller that sorts or truncates the answer leaves the service's own set alone.
func (s *Service) ExposedTools() []string { return slices.Clone(s.exposed) }

// WithheldBuiltins names the built-in tools the program enabled that are not served,
// which is all of them: no built-in declares a2a exposure. An operator who enabled some
// would otherwise see a served set that silently excludes them. The list arrives on
// ToolOptions.WithheldBuiltins, and this answers with a copy of it.
func (s *Service) WithheldBuiltins() []string { return slices.Clone(s.withheld) }

// Heading names this endpoint on a startup banner. The prompt channel under the same
// identity prints a section of its own.
func (s *Service) Heading() string { return "Serving tools over a2a" }

// Describe returns the subjects this endpoint is reached on and the limits a served
// call gets, for display. It answers with a copy, so a caller that reorders the lines it
// prints leaves the service's own set alone.
func (s *Service) Describe() []serve.DescLine { return slices.Clone(s.describe) }

// describeService returns the banner lines for a service. addr names the addresses it
// answers on, concurrency is how many calls it runs at once, and callTimeout stops one
// call.
func describeService(addr []a2a.DescLine, concurrency int, callTimeout time.Duration) []serve.DescLine {
	lines := make([]serve.DescLine, 0, len(addr)+2)
	for _, l := range addr {
		lines = append(lines, serve.DescLine{Label: l.Label, Value: l.Value})
	}

	return append(lines,
		serve.DescLine{Label: "Concurrency", Value: strconv.Itoa(concurrency)},
		serve.DescLine{Label: "Tool Timeout", Value: callTimeout.String()},
	)
}
