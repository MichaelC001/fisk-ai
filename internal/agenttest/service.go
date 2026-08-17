//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agenttest

import (
	"sync"
	"testing"

	"github.com/choria-io/fisk-ai/internal/serve"
)

var _ serve.Service = (*Service)(nil)

// Service is a serve.Service that answers nobody and counts how often it was closed, so
// a test drives a server hosting a surface that produces no work without standing a
// transport up.
//
// Counting the closes rather than making them idempotent is deliberate: a program that
// drains on one signal and stops on the next reaches every service twice, and the
// contract says the service is what makes those two calls one. A fake that hid the
// second call would hide whether the server made it.
type Service struct {
	name string

	mu     sync.Mutex
	closes int
	faults chan error
}

// NewService builds a service that is answering.
//
// The name is not checked. A server rejecting an empty one is behavior worth a spec,
// and refusing to build the service would leave nothing to write it with.
func NewService(tb testing.TB, name string) *Service {
	tb.Helper()

	return &Service{name: name, faults: make(chan error, 1)}
}

// Faults implements serve.FaultingEndpoint, so a spec can make a hosted service report
// that it has stopped answering and assert what the server does about it.
func (s *Service) Faults() <-chan error { return s.faults }

// Fault reports that this service has stopped answering for a reason nobody asked for.
func (s *Service) Fault(err error) { s.faults <- err }

// Name identifies the service.
func (s *Service) Name() string { return s.name }

// Close records that the server released this service.
func (s *Service) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.closes++

	return nil
}

// Closes reports how many times Close was called. It is safe to call while a server is
// running.
func (s *Service) Closes() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.closes
}
