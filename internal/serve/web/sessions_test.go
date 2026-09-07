//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/runstate"
)

// What every conversation in these specs was journaled with, so a row's creation time
// and model are what the store was given rather than whatever the spec ran at.
var (
	journaledAt    = time.Now().Add(-time.Hour).UTC()
	journaledModel = "claude-sonnet-4-6"
)

var _ = Describe("The sessions API", func() {
	var (
		ch    *Channel
		store *agenttest.FakeSessionStore
		ctx   context.Context
	)

	BeforeEach(func() {
		ctx = context.Background()
		store = agenttest.NewFakeSessionStore(GinkgoTB())

		opts := testOptions()
		opts.Sessions = store
		opts.Formats = []Mount{{Path: "fake", Format: &fakeFormat{}}}
		ch = newTestChannel(opts)
	})

	// journal writes one conversation into the store: the meta record every run opens
	// with, and the terminal record a finished turn writes. A nil summary is a
	// conversation whose last turn ended before a turn recorded one.
	journal := func(id, agent, prompt string, summary *runstate.ConversationSummary) {
		GinkgoHelper()

		j, err := store.Create(ctx, id, runstate.MetaRecord{
			RunID:       id,
			Created:     journaledAt,
			Prompt:      prompt,
			Agent:       agent,
			Fingerprint: runstate.Fingerprint{Model: journaledModel},
		})
		Expect(err).ToNot(HaveOccurred())

		err = j.Append(ctx, 2, runstate.Record{Protocol: runstate.TerminalProtocol, Terminal: &runstate.TerminalRecord{
			Reason:  runstate.ReasonCompleted,
			Summary: summary,
		}})
		Expect(err).ToNot(HaveOccurred())
		Expect(j.Close()).To(Succeed())
	}

	// session journals a conversation this channel minted, under the thread id a page
	// would have named, and returns its session id.
	session := func(thread, prompt string, summary *runstate.ConversationSummary) string {
		GinkgoHelper()

		id := SessionFor("agent1", thread)
		journal(id, "agent1", prompt, summary)

		return id
	}

	// call drives one sessions request through the handler, so a refusal is asserted on
	// without a server behind the channel.
	call := func(method, path string, headers map[string]string) *httptest.ResponseRecorder {
		GinkgoHelper()

		req := httptest.NewRequest(method, "http://"+ch.Addr()+path, nil)
		for k, v := range headers {
			req.Header.Set(k, v)
		}

		rec := httptest.NewRecorder()
		ch.server.Handler.ServeHTTP(rec, req)

		return rec
	}

	// list reads a page of conversations, failing the spec unless the status is 200.
	list := func(query string) ConversationList {
		GinkgoHelper()

		rec := call(http.MethodGet, "/fisk/v1/sessions"+query, nil)
		Expect(rec.Code).To(Equal(http.StatusOK), rec.Body.String())
		Expect(rec.Header().Get("Content-Type")).To(Equal("application/json"))

		var out ConversationList
		Expect(json.Unmarshal(rec.Body.Bytes(), &out)).To(Succeed())

		return out
	}

	listedIDs := func(page ConversationList) []string {
		ids := make([]string, len(page.Sessions))
		for i, s := range page.Sessions {
			ids[i] = s.ID
		}

		return ids
	}

	Describe("Listing", func() {
		// One store is handed to every channel, so a Slack thread and a peer's prompt
		// are in it beside a browser's conversations. The prefix is what keeps them out
		// of the rail, where they would otherwise appear with a delete button beside
		// them.
		It("Should list the conversations this channel minted and no other channel's", func() {
			mine := session("t1", "list the streams", nil)
			journal("slack-1a2b", "agent1", "a slack thread", nil)
			journal("a2a-9f8e", "agent1", "a peer's prompt", nil)

			Expect(listedIDs(list(""))).To(ConsistOf(mine))
		})

		// Two agents pointed at one JetStream stream write into one store, so the rail
		// shows the serving agent's conversations. A conversation journaled before
		// anyone recorded an agent is shown, which is what an operator running since
		// before the field has.
		It("Should list the serving agent's conversations and the ones stamped with no agent", func() {
			mine := session("t1", "mine", nil)
			unstamped := SessionFor("agent1", "t2")
			journal(unstamped, "", "journaled before the field existed", nil)
			journal(SessionFor("agent2", "t3"), "agent2", "another agent's", nil)

			Expect(listedIDs(list(""))).To(ConsistOf(mine, unstamped))
		})

		It("Should page with the cursor, repeating no conversation and skipping none", func() {
			want := []string{}
			for _, thread := range []string{"t1", "t2", "t3", "t4", "t5"} {
				want = append(want, session(thread, thread, nil))
			}

			var got []string
			cursor := ""
			for range 10 {
				page := list("?limit=2&cursor=" + cursor)
				got = append(got, listedIDs(page)...)
				cursor = page.Cursor
				if cursor == "" {
					break
				}
			}

			Expect(cursor).To(BeEmpty(), "the walk reached the end of the store")
			Expect(got).To(Equal(want), "in creation order, oldest first")
		})

		It("Should carry the turn count, the context size and the tool call count", func() {
			session("t1", "count me", &runstate.ConversationSummary{
				Turns:         3,
				ContextTokens: 4096,
				Counters:      runstate.Counters{ToolCalls: 7, LlmCalls: 9},
			})

			page := list("")
			Expect(page.Sessions).To(HaveLen(1))
			Expect(page.Sessions[0].Summary).To(Equal(&ConversationCounts{Turns: 3, ContextTokens: 4096, ToolCalls: 7}))
		})

		// A rail draws an empty slot for a conversation nobody summarized rather than
		// counts of zero, so the field is absent from the answer rather than zeroed.
		It("Should leave the summary out for a conversation whose last turn recorded none", func() {
			session("t1", "held before a turn counted anything", nil)

			page := list("")
			Expect(page.Sessions).To(HaveLen(1))
			Expect(page.Sessions[0].Summary).To(BeNil())

			rec := call(http.MethodGet, "/fisk/v1/sessions", nil)
			Expect(rec.Body.String()).ToNot(ContainSubstring("summary"))
		})

		It("Should name a conversation by its first prompt", func() {
			session("t1", "list the streams on the production cluster", nil)

			Expect(list("").Sessions[0].Title).To(Equal("list the streams on the production cluster"))
		})

		// A rail draws these three beside the title, so a row that reached the browser
		// without them would be a listing nobody can order or label.
		It("Should carry the creation time, the last activity and the model", func() {
			session("t1", "list the streams", nil)

			row := list("").Sessions[0]
			Expect(row.Created).To(BeTemporally("==", journaledAt))
			Expect(row.Updated).To(BeTemporally(">=", journaledAt))
			Expect(row.Model).To(Equal(journaledModel))
		})

		It("Should answer an empty store with no conversations and no cursor", func() {
			page := list("")
			Expect(page.Sessions).To(BeEmpty())
			Expect(page.Cursor).To(BeEmpty())
		})

		It("Should refuse a limit that is not a number or is outside the range", func() {
			for _, query := range []string{"?limit=none", "?limit=0", "?limit=-1", "?limit=101"} {
				rec := call(http.MethodGet, "/fisk/v1/sessions"+query, nil)
				Expect(rec.Code).To(Equal(http.StatusBadRequest), query)
			}
		})

		It("Should refuse a cursor this store did not mint", func() {
			rec := call(http.MethodGet, "/fisk/v1/sessions?cursor=not-a-cursor", nil)

			Expect(rec.Code).To(Equal(http.StatusBadRequest))
			Expect(rec.Body.String()).To(ContainSubstring("list from the start"))
		})
	})

	Describe("Opening", func() {
		It("Should hand a conversation this channel minted to the format", func() {
			id := session("t1", "what is running", nil)

			rec := call(http.MethodGet, "/fisk/v1/fake/sessions/"+id, nil)

			Expect(rec.Code).To(Equal(http.StatusOK))
			Expect(rec.Header().Get("X-Fake-Format")).To(Equal("v1"), "the format wrote the head")
			Expect(strings.Split(strings.TrimSpace(rec.Body.String()), "\n")).To(Equal([]string{
				"open",
				`replay ` + id + ` "what is running"`,
				"close reason=completed taken=true err=<nil>",
			}))
		})

		It("Should refuse a conversation another channel or another agent holds", func() {
			journal("slack-1a2b", "agent1", "a slack thread", nil)
			other := SessionFor("agent2", "t1")
			journal(other, "agent2", "another agent's", nil)

			for _, id := range []string{"slack-1a2b", other, SessionFor("agent1", "never-held")} {
				rec := call(http.MethodGet, "/fisk/v1/fake/sessions/"+id, nil)

				Expect(rec.Code).To(Equal(http.StatusNotFound), id)
				Expect(rec.Body.String()).To(ContainSubstring("not one this agent holds"))
			}
		})
	})

	Describe("Deleting", func() {
		It("Should remove a conversation this channel minted", func() {
			id := session("t1", "delete me", nil)

			rec := call(http.MethodDelete, "/fisk/v1/sessions/"+id, nil)
			Expect(rec.Code).To(Equal(http.StatusNoContent))
			Expect(rec.Body.String()).To(BeEmpty())

			Expect(list("").Sessions).To(BeEmpty())

			_, err := store.Load(ctx, id)
			Expect(err).To(MatchError(runstate.ErrNotFound))
		})

		// The prefix is what stops a delete reaching another channel's conversation.
		// Nothing here stops it reaching another browser's, which is a deployment
		// requirement rather than something this endpoint answers.
		It("Should refuse to delete a conversation outside this channel's prefix and leave it stored", func() {
			journal("slack-1a2b", "agent1", "a slack thread", nil)

			rec := call(http.MethodDelete, "/fisk/v1/sessions/slack-1a2b", nil)
			Expect(rec.Code).To(Equal(http.StatusNotFound))

			_, err := store.Load(ctx, "slack-1a2b")
			Expect(err).ToNot(HaveOccurred(), "the conversation is still stored")
		})

		It("Should refuse to delete another agent's conversation and leave it stored", func() {
			other := SessionFor("agent2", "t1")
			journal(other, "agent2", "another agent's", nil)

			rec := call(http.MethodDelete, "/fisk/v1/sessions/"+other, nil)
			Expect(rec.Code).To(Equal(http.StatusNotFound))

			_, err := store.Load(ctx, other)
			Expect(err).ToNot(HaveOccurred())
		})

		It("Should answer a conversation the store does not hold with a 404", func() {
			rec := call(http.MethodDelete, "/fisk/v1/sessions/"+SessionFor("agent1", "never-held"), nil)

			Expect(rec.Code).To(Equal(http.StatusNotFound))
		})
	})

	Describe("The checks every route passes", func() {
		It("Should refuse an unlisted origin on each of the three routes", func() {
			id := session("t1", "hi", nil)
			evil := map[string]string{"Origin": "http://evil.example"}

			Expect(call(http.MethodGet, "/fisk/v1/sessions", evil).Code).To(Equal(http.StatusForbidden))
			Expect(call(http.MethodGet, "/fisk/v1/fake/sessions/"+id, evil).Code).To(Equal(http.StatusForbidden))
			Expect(call(http.MethodDelete, "/fisk/v1/sessions/"+id, evil).Code).To(Equal(http.StatusForbidden))

			_, err := store.Load(ctx, id)
			Expect(err).ToNot(HaveOccurred(), "the refused delete removed nothing")
		})

		It("Should let a listed page read each of the three routes", func() {
			id := session("t1", "hi", nil)
			listed := map[string]string{"Origin": testOrigin}

			for _, rec := range []*httptest.ResponseRecorder{
				call(http.MethodGet, "/fisk/v1/sessions", listed),
				call(http.MethodGet, "/fisk/v1/fake/sessions/"+id, listed),
				call(http.MethodDelete, "/fisk/v1/sessions/"+id, listed),
			} {
				Expect(rec.Code).To(BeNumerically("<", 300), rec.Body.String())
				Expect(rec.Header().Get("Access-Control-Allow-Origin")).To(Equal(testOrigin))
			}
		})

		// A browser refuses to send a method the preflight left out, so the delete would
		// never leave the page.
		It("Should tell a preflight that a page may delete", func() {
			rec := call(http.MethodOptions, "/fisk/v1/sessions/x", map[string]string{
				"Origin":                        testOrigin,
				"Access-Control-Request-Method": "DELETE",
			})

			Expect(rec.Code).To(Equal(http.StatusNoContent))
			Expect(rec.Header().Get("Access-Control-Allow-Methods")).To(ContainSubstring("DELETE"))
		})

		It("Should refuse a Host that is not the loopback listener", func() {
			req := httptest.NewRequest(http.MethodGet, "http://"+ch.Addr()+"/fisk/v1/sessions", nil)
			req.Host = "agent.evil.example:" + ch.port

			rec := httptest.NewRecorder()
			ch.server.Handler.ServeHTTP(rec, req)

			Expect(rec.Code).To(Equal(http.StatusForbidden))
		})

		It("Should refuse a request once the channel is draining", func() {
			Expect(ch.Close()).To(Succeed())

			rec := call(http.MethodGet, "/fisk/v1/sessions", nil)

			Expect(rec.Code).To(Equal(http.StatusServiceUnavailable))
			Expect(rec.Header().Get("Retry-After")).To(Equal(retryAfter))
		})
	})

	Describe("The methods each route takes", func() {
		It("Should refuse a method no route answers", func() {
			id := session("t1", "hi", nil)

			Expect(call(http.MethodPost, "/fisk/v1/sessions", nil).Code).To(Equal(http.StatusMethodNotAllowed))
			Expect(call(http.MethodDelete, "/fisk/v1/sessions", nil).Code).To(Equal(http.StatusMethodNotAllowed))
			Expect(call(http.MethodGet, "/fisk/v1/sessions/"+id, nil).Code).To(Equal(http.StatusMethodNotAllowed))
			Expect(call(http.MethodPost, "/fisk/v1/fake/sessions/"+id, nil).Code).To(Equal(http.StatusMethodNotAllowed))
			Expect(call(http.MethodDelete, "/fisk/v1/fake/sessions/"+id, nil).Code).To(Equal(http.StatusMethodNotAllowed))
		})

		// A page opens a conversation under the format that renders it, so a channel
		// mounting no format lists and deletes and opens nothing.
		It("Should answer no open route on a channel with no format mounted", func() {
			opts := testOptions()
			bare := newTestChannel(opts)

			req := httptest.NewRequest(http.MethodGet, "http://"+bare.Addr()+"/fisk/v1/fake/sessions/x", nil)
			rec := httptest.NewRecorder()
			bare.server.Handler.ServeHTTP(rec, req)

			Expect(rec.Code).To(Equal(http.StatusNotFound))
		})

		It("Should refuse a format mounted where the session list answers", func() {
			opts := testOptions()
			opts.Formats = []Mount{{Path: SessionsPath, Format: &fakeFormat{}}}

			_, err := New(opts)
			Expect(err).To(MatchError(ContainSubstring("where this channel lists the stored conversations")))
		})
	})
})

// failingConversations answers every call with a failure that is not a missing
// conversation, which is the answer a store the channel cannot reach gives.
type failingConversations struct{ err error }

func (c *failingConversations) List(context.Context, int, string) (runstate.RunPage, error) {
	return runstate.RunPage{}, c.err
}

func (c *failingConversations) Load(context.Context, string) (*runstate.RunState, error) {
	return nil, c.err
}

func (c *failingConversations) Delete(context.Context, string) error { return c.err }

// The channel reaches past conversations through the interface it holds rather than
// through the store, which is what lets a process fronting a worker supply them.
var _ = Describe("The conversations a channel is given", func() {
	It("Should answer the three routes and report a failure without naming it", func() {
		opts := testOptions()
		opts.Formats = []Mount{{Path: "fake", Format: &fakeFormat{}}}
		opts.Conversations = &failingConversations{err: errors.New("the stream is unreachable")}
		ch := newTestChannel(opts)

		drive := func(method, path string) *httptest.ResponseRecorder {
			GinkgoHelper()

			rec := httptest.NewRecorder()
			ch.server.Handler.ServeHTTP(rec, httptest.NewRequest(method, "http://"+ch.Addr()+path, nil))

			return rec
		}

		for _, rec := range []*httptest.ResponseRecorder{
			drive(http.MethodGet, "/fisk/v1/sessions"),
			drive(http.MethodGet, "/fisk/v1/fake/sessions/w-abc"),
			drive(http.MethodDelete, "/fisk/v1/sessions/w-abc"),
		} {
			Expect(rec.Code).To(Equal(http.StatusInternalServerError))
			Expect(rec.Body.String()).To(ContainSubstring("ask again in a moment"))
			Expect(rec.Body.String()).ToNot(ContainSubstring("unreachable"))
		}
	})
})

var _ = Describe("A session list row", func() {
	It("Should carry what a rail draws beside the title", func() {
		created := time.Now().Add(-time.Hour)
		updated := time.Now()

		row := conversationFor(runstate.RunInfo{
			RunID:    "w-abc",
			Created:  created,
			Updated:  updated,
			Model:    "claude-sonnet-4-6",
			Prompt:   "list the streams",
			Terminal: runstate.ReasonCompleted,
			Summary: &runstate.ConversationSummary{
				Turns:         2,
				ContextTokens: 1200,
				Counters:      runstate.Counters{ToolCalls: 5},
			},
		})

		Expect(row).To(Equal(Conversation{
			ID:       "w-abc",
			Title:    "list the streams",
			Created:  created,
			Updated:  updated,
			Model:    "claude-sonnet-4-6",
			Terminal: runstate.ReasonCompleted,
			Summary:  &ConversationCounts{Turns: 2, ContextTokens: 1200, ToolCalls: 5},
		}))
	})
})

var _ = Describe("A conversation title", func() {
	It("Should be the first prompt on one line", func() {
		Expect(TitleFor("list the streams\nand the consumers")).To(Equal("list the streams and the consumers"))
		Expect(TitleFor("  spaced   out  ")).To(Equal("spaced out"))
		Expect(TitleFor("")).To(BeEmpty())
	})

	It("Should cut a long prompt and mark the cut", func() {
		title := TitleFor(strings.Repeat("a", MaxTitleRunes+10))

		Expect(title).To(Equal(strings.Repeat("a", MaxTitleRunes) + "..."))
	})

	It("Should cut on a rune rather than a byte", func() {
		title := TitleFor(strings.Repeat("é", MaxTitleRunes+1))

		Expect([]rune(title)).To(HaveLen(MaxTitleRunes + 3))
	})
})
