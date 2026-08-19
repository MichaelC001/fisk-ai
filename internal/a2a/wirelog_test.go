//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2a

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	. "github.com/onsi/gomega"
)

func TestWireLog_RecordsBothDirectionsVerbatim(t *testing.T) {
	g := NewWithT(t)

	var buf bytes.Buffer
	w := &wireLog{out: &buf, now: func() time.Time { return time.Unix(0, 0).UTC() }}

	w.send(OpTask, "agent1", "req1", []byte(`{"protocol":"x","prompt":"hello"}`))
	w.recv(OpTask, "agent1", "req1", []byte(`{"protocol":"y"}`))

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	g.Expect(lines).To(HaveLen(2))

	g.Expect(lines[0]).To(ContainSubstring("> agent1"))
	g.Expect(lines[0]).To(ContainSubstring(`{"protocol":"x","prompt":"hello"}`), "the body as it crossed, not a rendering of it")
	g.Expect(lines[1]).To(ContainSubstring("< agent1"))
}

// The address is what an operator subscribes to when they go and watch the same traffic
// with their own tools, so it is what the log names when the binding can say it.
func TestWireLog_NamesTheSubjectWhenTheBindingCan(t *testing.T) {
	g := NewWithT(t)

	var buf bytes.Buffer
	w := &wireLog{out: &buf, now: time.Now, names: fixedSubjects{}}

	w.send(OpTask, "agent1", "req1", []byte("{}"))
	w.send(OpCancel, "agent1", "req1", []byte("{}"))

	g.Expect(buf.String()).To(ContainSubstring("> task/agent1"))
	g.Expect(buf.String()).To(ContainSubstring("> cancel/agent1/req1"))
}

// A binding whose addressing is not nameable leaves the address off rather than
// stopping anything being recorded.
func TestWireLog_FallsBackToTheAgentName(t *testing.T) {
	g := NewWithT(t)

	var buf bytes.Buffer
	w := &wireLog{out: &buf, now: time.Now, names: emptySubjects{}}

	w.send(OpTask, "agent1", "req1", []byte("{}"))
	g.Expect(buf.String()).To(ContainSubstring("> agent1"))
}

type fixedSubjects struct{}

func (fixedSubjects) Subject(op RouteHint, agent, request string) string {
	if op == OpCancel {
		return "cancel/" + agent + "/" + request
	}

	return "task/" + agent
}

type emptySubjects struct{}

func (emptySubjects) Subject(RouteHint, string, string) string { return "" }

// A nil log is what every client that did not ask for one holds, and it is reached on
// every send.
func TestWireLog_NilRecordsNothing(t *testing.T) {
	g := NewWithT(t)

	var w *wireLog
	g.Expect(func() { w.send(OpTask, "agent1", "req1", []byte("{}")) }).ToNot(Panic())
}

// A task's reply set is read on one goroutine while its questions are answered and its
// acks sent on others, so more than one message can be in flight at once.
func TestWireLog_IsSafeUnderConcurrentSenders(t *testing.T) {
	g := NewWithT(t)

	var buf bytes.Buffer
	w := &wireLog{out: &buf, now: time.Now}

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.send(OpTask, "agent1", "req1", []byte(`{"a":1}`))
		}()
	}
	wg.Wait()

	g.Expect(strings.Count(buf.String(), "\n")).To(Equal(50))
}
