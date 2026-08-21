//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package slack

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/agent"
	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/remotetools"
	"github.com/choria-io/fisk-ai/internal/serve"
)

// logCapture collects what the channel wrote to the worker's log, which is where the
// things that may never reach a thread are asserted on.
type logCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logCapture) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.buf.Write(p)
}

func (l *logCapture) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.buf.String()
}

func (l *logCapture) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(l, nil))
}

// assistantTurn is one turn carrying text, which is what the run reports as it works and
// once more as its answer.
func assistantTurn(text string) llm.Response {
	return llm.Response{Content: []llm.ContentBlock{{Text: &llm.TextBlock{Text: text}}}}
}

var _ = Describe("The events sink", func() {
	var (
		api     *fakeAPI
		socket  *fakeSocket
		opts    Options
		session string
	)

	BeforeEach(func() {
		api = newFakeAPI()
		socket = newFakeSocket()
		opts = testOptions()
		session = SessionFor(opts.Identity, "T1", "C1", "1700000000.000100")
	})

	// running hands over one turn's work with its status message already posted, which is
	// the state every one of these specs starts a run in.
	running := func(ch *Channel) *serve.Work {
		GinkgoHelper()

		socket.deliver(aMention().envelope())
		Eventually(textIn(api, "C1")).Should(Equal(hintThinking))

		w := nextWork(ch)
		Expect(w.Events).ToNot(BeNil(), "a run with nowhere to report is a thread that says nothing")

		return w
	}

	Describe("The status message", func() {
		It("Should move the status message through the hint each event reaches", func() {
			ch := roomyChannel(opts, api, socket)
			w := running(ch)

			w.Events.Starting(agent.RunInfo{Tools: 4, SessionID: session})
			Consistently(editsIn(api, "C1"), 100*time.Millisecond).Should(BeEmpty(),
				"a turn already reading as thinking spends no call on being told the run started")

			w.Events.ToolCall(agent.ToolTrace{ID: "tu1", Name: "memory_search"})
			Eventually(textIn(api, "C1")).Should(Equal(hintMemory))

			w.Events.ToolCall(agent.ToolTrace{ID: "tu2", Name: "knowledge_search"})
			Eventually(textIn(api, "C1")).Should(Equal(hintKnowledge))

			w.Events.ToolCall(agent.ToolTrace{ID: "tu3", Name: "restart_node"})
			Eventually(textIn(api, "C1")).Should(Equal(hintTools))

			w.Events.Message(assistantTurn("let me look at node3"), false)
			Eventually(textIn(api, "C1")).Should(Equal(hintThinking))

			Expect(api.messages()).To(HaveLen(1), "one message per turn, edited in place")
		})

		// The tool's result is what the model reads. The status message is already on the
		// hint of the call it answers, and there is nothing in a result a thread can act on.
		It("Should say nothing about a tool result or a request summary", func() {
			ch := roomyChannel(opts, api, socket)
			w := running(ch)

			w.Events.ToolCall(agent.ToolTrace{ID: "tu1", Name: "restart_node"})
			Eventually(textIn(api, "C1")).Should(Equal(hintTools))

			w.Events.ToolResult(agent.ToolResultTrace{CallID: "tu1", Output: "restarted"})
			w.Events.LLMRequest("2 tools, 1200 tokens")

			Consistently(editsIn(api, "C1"), 100*time.Millisecond).Should(Equal([]string{hintTools}))
		})

		// The ending posts the answer from Outcome.Text. A sink that posted the terminal
		// message as well would put the answer in the thread twice, once as a message
		// nobody is notified about and once as the message they are.
		It("Should not post the terminal message, which the ending posts from the outcome", func() {
			ch := roomyChannel(opts, api, socket)
			w := running(ch)

			w.Events.Message(assistantTurn("node3 was full of journal logs"), true)

			Consistently(api.messages, 100*time.Millisecond).Should(HaveLen(1), "the status message and nothing else")
			Expect(editsIn(api, "C1")()).To(BeEmpty())

			answered(w, "node3 was full of journal logs")

			var posted []fakeMessage
			Eventually(func() []fakeMessage { posted = api.messages(); return posted }).Should(HaveLen(2))

			Expect(posted[1].Text).To(Equal("node3 was full of journal logs"))
		})
	})

	Describe("The advisories", func() {
		It("Should put what the run warned about under the answer", func() {
			ch := roomyChannel(opts, api, socket)
			w := running(ch)

			w.Events.Warn(agent.Warning{Kind: agent.WarnToolTimeout, Name: "restart_node", Err: fmt.Errorf("no result within 30s")})
			w.Events.Warn(agent.Warning{Kind: agent.WarnKnowledgeIndexAbsent, Name: "/srv/agent/store/knowledge"})

			answered(w, "node3 was full of journal logs")

			var posted []fakeMessage
			Eventually(func() []fakeMessage { posted = api.messages(); return posted }).Should(HaveLen(3))

			Expect(posted[1].Text).To(Equal("node3 was full of journal logs"))

			note := posted[2]
			Expect(note.ThreadTS).To(Equal("1700000000.000100"))
			Expect(note.Text).To(HavePrefix(warnNoteMany))
			Expect(note.Text).To(ContainSubstring(warningLines[agent.WarnToolTimeout]))
			Expect(note.Text).To(ContainSubstring(warningLines[agent.WarnKnowledgeIndexAbsent]))

			// A warning names tools, paths and errors, which is the material the hint table
			// already keeps out of a channel everybody reads.
			Expect(note.Text).ToNot(ContainSubstring("restart_node"))
			Expect(note.Text).ToNot(ContainSubstring("/srv/agent/store"))
			Expect(note.Text).ToNot(ContainSubstring("within 30s"))
		})

		It("Should say a single advisory as one sentence", func() {
			ch := roomyChannel(opts, api, socket)
			w := running(ch)

			w.Events.Warn(agent.Warning{Kind: agent.WarnApprovalsDropped, Count: 2})

			answered(w, "node3 was full of journal logs")

			var posted []fakeMessage
			Eventually(func() []fakeMessage { posted = api.messages(); return posted }).Should(HaveLen(3))

			Expect(posted[2].Text).To(Equal(warnNoteOne + warningLines[agent.WarnApprovalsDropped]))
		})

		// A tool that fails the same way twenty times is one hole in the answer, not twenty.
		It("Should say the same thing once however often the run raised it", func() {
			ch := roomyChannel(opts, api, socket)
			w := running(ch)

			for range 20 {
				w.Events.Warn(agent.Warning{Kind: agent.WarnToolTimeout, Name: "restart_node"})
			}

			answered(w, "node3 was full of journal logs")

			var posted []fakeMessage
			Eventually(func() []fakeMessage { posted = api.messages(); return posted }).Should(HaveLen(3))

			Expect(posted[2].Text).To(Equal(warnNoteOne + warningLines[agent.WarnToolTimeout]))
		})

		// One run's bad turn must not become a channel nobody can read.
		It("Should list no more than the bound and count the rest", func() {
			ch := roomyChannel(opts, api, socket)
			w := running(ch)

			kinds := []agent.WarningKind{
				agent.WarnToolTimeout,
				agent.WarnKnowledgeIndexAbsent,
				agent.WarnMemoryIndex,
				agent.WarnApprovalsDropped,
				agent.WarnToolSetDrift,
				agent.WarnBudgetDrift,
				agent.WarnUnknownTool,
				agent.WarnMissingRequired,
			}
			for _, kind := range kinds {
				w.Events.Warn(agent.Warning{Kind: kind})
			}

			answered(w, "node3 was full of journal logs")

			var posted []fakeMessage
			Eventually(func() []fakeMessage { posted = api.messages(); return posted }).Should(HaveLen(3))

			note := posted[2].Text
			Expect(strings.Count(note, warnNoteItem)).To(Equal(maxThreadWarnings+1), "the bound, and the line counting what was left out")
			Expect(note).To(ContainSubstring(fmt.Sprintf(warnNoteMore, len(kinds)-maxThreadWarnings)))
			Expect(note).To(ContainSubstring(warningLines[agent.WarnToolTimeout]), "the first ones the run raised")
			Expect(note).ToNot(ContainSubstring(warningLines[agent.WarnMissingRequired]))
		})

		// no_progress takes the running commentary. An advisory is not commentary: it is
		// what a person needs to read the answer correctly.
		It("Should reach the thread where there is no status message", func() {
			opts.Progress = false

			ch := roomyChannel(opts, api, socket)

			socket.deliver(aMention().envelope())
			Eventually(socket.acked).Should(HaveLen(1))

			w := nextWork(ch)
			Expect(statusOf(ch, session)).To(BeNil())

			w.Events.Starting(agent.RunInfo{})
			w.Events.ToolCall(agent.ToolTrace{ID: "tu1", Name: "restart_node"})
			w.Events.Warn(agent.Warning{Kind: agent.WarnToolTimeout})

			answered(w, "node3 was full of journal logs")

			var posted []fakeMessage
			Eventually(func() []fakeMessage { posted = api.messages(); return posted }).Should(HaveLen(2))

			Expect(posted[0].Text).To(Equal("node3 was full of journal logs"))
			Expect(posted[1].Text).To(Equal(warnNoteOne + warningLines[agent.WarnToolTimeout]))
		})

		It("Should say nothing where the run raised nothing", func() {
			ch := roomyChannel(opts, api, socket)
			w := running(ch)

			answered(w, "node3 was full of journal logs")

			Eventually(api.messages).Should(HaveLen(2))
			Consistently(api.messages, 100*time.Millisecond).Should(HaveLen(2))
		})

		// The notes reach an implementation of the optional half or nowhere at all, so
		// without this a mistyped include filter leaves every thread short of tools with
		// nobody told.
		It("Should render what importing another agent's tools had to say", func() {
			ch := roomyChannel(opts, api, socket)
			w := running(ch)

			reporter, ok := w.Events.(agent.RemoteHostReporter)
			Expect(ok).To(BeTrue())

			reporter.RemoteHostNotes([]remotetools.HostImport{{Host: config.RemoteToolHost{Name: "peer.agent"}}})

			answered(w, "node3 was full of journal logs")

			var posted []fakeMessage
			Eventually(func() []fakeMessage { posted = api.messages(); return posted }).Should(HaveLen(3))

			Expect(posted[2].Text).To(Equal(warnNoteOne + warnRemote))
			Expect(posted[2].Text).ToNot(ContainSubstring("peer.agent"))
		})

		// An advisory nobody has written a sentence for is still a hole in the answer.
		It("Should name a kind it has no sentence for rather than dropping it", func() {
			Expect(warningLine(agent.Warning{Kind: agent.WarnPromptDenied})).To(ContainSubstring(agent.WarnPromptDenied.String()))
		})
	})

	// The stack leaks absolute paths, module layout and frame arguments, and its own
	// contract requires an implementation that forwards anywhere to keep it local.
	It("Should write a crash to the worker log and never to the thread", func() {
		captured := &logCapture{}
		opts.Logger = captured.logger()

		ch := roomyChannel(opts, api, socket)
		w := running(ch)

		stack := "goroutine 17 [running]:\n/Users/somebody/go/src/example/runner.go:412 +0x1c"
		w.Events.Panicked("assignment to entry in nil map", []byte(stack))

		Consistently(api.messages, 100*time.Millisecond).Should(HaveLen(1))
		Expect(editsIn(api, "C1")()).To(BeEmpty())

		Eventually(captured.text).Should(ContainSubstring("assignment to entry in nil map"))
		Expect(captured.text()).To(ContainSubstring("/Users/somebody/go/src/example/runner.go"))

		answered(w, "")

		for _, m := range api.messages() {
			Expect(m.Text).ToNot(ContainSubstring("goroutine"))
			Expect(m.Text).ToNot(ContainSubstring("/Users/somebody"))
		}
	})

	// A session id is what this channel derives from the thread and never publishes into
	// it, and the log is the only place the conversation it left behind can be picked up
	// from.
	It("Should log a rotated session and say nothing about it in the thread", func() {
		captured := &logCapture{}
		opts.Logger = captured.logger()

		ch := roomyChannel(opts, api, socket)
		w := running(ch)

		w.Events.SessionRotated("s-0011223344")

		Consistently(api.messages, 100*time.Millisecond).Should(HaveLen(1))
		Eventually(captured.text).Should(ContainSubstring("s-0011223344"))
	})
})
