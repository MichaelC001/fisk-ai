//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agent_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/toolkit"
)

// newSlogCapture returns a SlogEvents writing JSON to buf and the buf to read back.
// A JSON handler makes each record a parseable object so a spec asserts on the
// structured attributes rather than on prose.
func newSlogCapture(verbose bool) (*agent.SlogEvents, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	return agent.NewSlogEvents(log, verbose), buf
}

// records parses the captured buffer into one map per JSON log line.
func records(buf *bytes.Buffer) []map[string]any {
	GinkgoHelper()

	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		Expect(json.Unmarshal([]byte(line), &rec)).To(Succeed(), "bad log line %q", line)
		out = append(out, rec)
	}

	return out
}

var _ = Describe("SlogEvents", func() {
	It("Should log the run's tool count, session and resumed flag on Starting", func() {
		ev, buf := newSlogCapture(false)

		ev.Starting(agent.RunInfo{Tools: 3, SessionID: "sess-1", Resumed: true})

		recs := records(buf)
		Expect(recs).To(HaveLen(1))
		Expect(recs[0]["msg"]).To(Equal("agent run starting"))
		Expect(recs[0]["tools"]).To(BeEquivalentTo(3))
		Expect(recs[0]["session_id"]).To(Equal("sess-1"))
		Expect(recs[0]["resumed"]).To(Equal(true))
	})

	It("Should carry the kind and fields of a warning", func() {
		ev, buf := newSlogCapture(false)

		ev.Warn(agent.Warning{Kind: agent.WarnConfirmNoTerminal, Count: 2})

		recs := records(buf)
		Expect(recs).To(HaveLen(1))
		Expect(recs[0]["level"]).To(Equal("WARN"))
		Expect(recs[0]["kind"]).To(Equal("confirm_no_terminal"))
		Expect(recs[0]["count"]).To(BeEquivalentTo(2))
	})

	It("Should log an LLM request only when verbose", func() {
		quiet, quietBuf := newSlogCapture(false)
		quiet.LLMRequest("one request")
		Expect(quietBuf.Len()).To(BeZero(), "non-verbose must drop LLMRequest")

		loud, loudBuf := newSlogCapture(true)
		loud.LLMRequest("one request")
		recs := records(loudBuf)
		Expect(recs).To(HaveLen(1))
		Expect(recs[0]["level"]).To(Equal("DEBUG"))
		Expect(recs[0]["summary"]).To(Equal("one request"))
	})

	It("Should truncate a large tool result and say so", func() {
		ev, buf := newSlogCapture(false)

		big := strings.Repeat("x", 5000)
		ev.ToolResult(agent.ToolResultTrace{ProviderKind: toolkit.KindApplication, Output: big, IsError: true})

		recs := records(buf)
		Expect(recs).To(HaveLen(1))
		Expect(recs[0]["kind"]).To(Equal("application"))
		Expect(recs[0]["is_error"]).To(Equal(true))
		Expect(recs[0]["truncated"]).To(Equal(true))
		Expect(recs[0]["output"]).To(HaveLen(2048))
	})

	It("Should log a panic value and its stack at error level", func() {
		ev, buf := newSlogCapture(false)

		ev.Panicked("boom", []byte("goroutine 1 [running]:\nmain.main()"))

		recs := records(buf)
		Expect(recs).To(HaveLen(1))
		Expect(recs[0]["level"]).To(Equal("ERROR"))
		Expect(recs[0]["value"]).To(Equal("boom"))
		Expect(recs[0]["stack"]).To(ContainSubstring("goroutine 1"))
	})

	It("Should log a message's stop reason, token usage and terminal flag", func() {
		ev, buf := newSlogCapture(false)

		ev.Message(llm.Response{StopReason: llm.StopReason("end_turn"), Usage: llm.Usage{In: 10, Out: 20}}, true)

		recs := records(buf)
		Expect(recs).To(HaveLen(1))
		Expect(recs[0]["terminal"]).To(Equal(true))
		Expect(recs[0]["stop_reason"]).To(Equal("end_turn"))
		Expect(recs[0]["tokens_in"]).To(BeEquivalentTo(10))
		Expect(recs[0]["tokens_out"]).To(BeEquivalentTo(20))
	})

	// Several goroutines point at one SlogEvents, as a server aggregating many runs
	// would, and every record still lands intact.
	It("Should land every record under concurrent use", func() {
		ev, buf := newSlogCapture(false)

		const runs = 20
		var wg sync.WaitGroup
		wg.Add(runs)
		for i := 0; i < runs; i++ {
			go func() {
				defer wg.Done()
				ev.ToolCall(agent.ToolTrace{Name: "t"})
			}()
		}
		wg.Wait()

		Expect(records(buf)).To(HaveLen(runs))
	})
})
