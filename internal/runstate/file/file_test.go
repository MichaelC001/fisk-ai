//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package file

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/segmentio/ksuid"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/runstate"
)

func newID() string {
	return ksuid.New().String()
}

func assistantWithTools(iter int64, ids ...string) *runstate.AssistantRecord {
	content := []llm.ContentBlock{{Text: &llm.TextBlock{Text: "working"}}}
	for _, id := range ids {
		content = append(content, llm.ContentBlock{ToolUse: &llm.ToolUseBlock{ID: id, Name: "shell", Input: json.RawMessage(`{"x":1}`)}})
	}
	return &runstate.AssistantRecord{
		Iteration: iter,
		Message:   llm.Message{Role: llm.RoleAssistant, Content: content},
		InTokens:  10,
		OutTokens: 5,
	}
}

func toolResult(id string) *runstate.ToolResultRecord {
	return &runstate.ToolResultRecord{ToolUseID: id, Result: llm.ToolResultBlock{ToolUseID: id, Content: "ok"}}
}

// liveForReads answers Err with nil for the first reads calls and context.Canceled after,
// which puts a cancel at a chosen point inside a loop that reads the context once per
// item. Racing a real cancel against the filesystem lands somewhere different each run.
type liveForReads struct {
	context.Context

	reads int
	seen  int
}

func (c *liveForReads) Err() error {
	c.seen++
	if c.seen <= c.reads {
		return nil
	}

	return context.Canceled
}

var _ = Describe("FileStore", func() {
	var (
		store *FileStore
		ctx   = context.Background()
	)

	BeforeEach(func() {
		s, err := NewFileStore(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())
		store = s
	})

	// No Version: Create stamps it, so every test here folds a journal whose version
	// the store put there.
	newMeta := func(id string) runstate.MetaRecord {
		return runstate.MetaRecord{RunID: id, Prompt: "hello", Fingerprint: runstate.Fingerprint{Model: "claude-opus-4-8"}}
	}

	It("stamps the record version and leaves the caller's meta record alone", func() {
		id := newID()
		meta := newMeta(id)

		j, err := store.Create(ctx, id, meta)
		Expect(err).NotTo(HaveOccurred())
		Expect(j.Close()).To(Succeed())
		Expect(meta.Version).To(BeZero())

		rs, err := store.Load(ctx, id)
		Expect(err).NotTo(HaveOccurred())
		Expect(rs.Version).To(Equal(runstate.Version))
	})

	It("refuses a meta record carrying a version it does not write", func() {
		id := newID()
		meta := newMeta(id)
		meta.Version = runstate.Version + 1

		_, err := store.Create(ctx, id, meta)
		Expect(err).To(MatchError(runstate.ErrVersion))

		_, err = store.Load(ctx, id)
		Expect(err).To(MatchError(runstate.ErrNotFound))
	})

	It("creates, appends, and folds back a run", func() {
		id := newID()
		j, err := store.Create(ctx, id, newMeta(id))
		Expect(err).NotTo(HaveOccurred())
		Expect(j.Append(ctx, 2, runstate.Record{Protocol: runstate.AssistantProtocol, Assistant: assistantWithTools(0, "tu_1")})).To(Succeed())
		Expect(j.Append(ctx, 3, runstate.Record{Protocol: runstate.ToolResultProtocol, ToolResult: toolResult("tu_1")})).To(Succeed())
		Expect(j.Close()).To(Succeed())

		rs, err := store.Load(ctx, id)
		Expect(err).NotTo(HaveOccurred())
		Expect(rs.RunID).To(Equal(id))
		Expect(rs.Messages).To(HaveLen(3))
		Expect(rs.NextIteration).To(Equal(int64(1)))
	})

	It("refuses to create a run that already exists", func() {
		id := newID()
		j, err := store.Create(ctx, id, newMeta(id))
		Expect(err).NotTo(HaveOccurred())
		Expect(j.Close()).To(Succeed())

		_, err = store.Create(ctx, id, newMeta(id))
		Expect(err).To(MatchError(runstate.ErrExists))
	})

	It("treats a duplicate seq as an idempotent no-op and rejects gaps", func() {
		id := newID()
		j, err := store.Create(ctx, id, newMeta(id))
		Expect(err).NotTo(HaveOccurred())
		defer j.Close()

		Expect(j.Append(ctx, 2, runstate.Record{Protocol: runstate.AssistantProtocol, Assistant: assistantWithTools(0, "tu_1")})).To(Succeed())
		// Re-append the same seq (crash-retry): no error, no duplicate line.
		Expect(j.Append(ctx, 2, runstate.Record{Protocol: runstate.AssistantProtocol, Assistant: assistantWithTools(0, "tu_1")})).To(Succeed())
		recs, err := j.Records(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(recs).To(HaveLen(2))
		// A seq that skips ahead is a gap.
		Expect(j.Append(ctx, 5, runstate.Record{Protocol: runstate.ToolResultProtocol, ToolResult: toolResult("tu_1")})).To(MatchError(runstate.ErrSeqGap))
	})

	It("drops a torn final line but keeps complete records", func() {
		id := newID()
		j, err := store.Create(ctx, id, newMeta(id))
		Expect(err).NotTo(HaveOccurred())
		Expect(j.Append(ctx, 2, runstate.Record{Protocol: runstate.AssistantProtocol, Assistant: assistantWithTools(0, "tu_1")})).To(Succeed())
		Expect(j.Close()).To(Succeed())

		// Simulate a crash mid-write: append a truncated, unterminated line.
		f, err := os.OpenFile(store.journalPath(id), os.O_WRONLY|os.O_APPEND, 0o600)
		Expect(err).NotTo(HaveOccurred())
		_, err = f.WriteString(`{"seq":3,"protocol":"io.choria.fisk-ai.v1.session.tool_res`)
		Expect(err).NotTo(HaveOccurred())
		Expect(f.Close()).To(Succeed())

		rs, err := store.Load(ctx, id)
		Expect(err).NotTo(HaveOccurred())
		Expect(rs.Counters.LlmCalls).To(Equal(int64(1)))
	})

	It("errors on interior corruption", func() {
		id := newID()
		j, err := store.Create(ctx, id, newMeta(id))
		Expect(err).NotTo(HaveOccurred())
		Expect(j.Close()).To(Succeed())

		f, err := os.OpenFile(store.journalPath(id), os.O_WRONLY|os.O_APPEND, 0o600)
		Expect(err).NotTo(HaveOccurred())
		_, err = f.WriteString("not json at all\n{\"seq\":3,\"protocol\":\"io.choria.fisk-ai.v1.session.terminal\",\"terminal\":{\"reason\":\"completed\"}}\n")
		Expect(err).NotTo(HaveOccurred())
		Expect(f.Close()).To(Succeed())

		_, err = store.Load(ctx, id)
		Expect(err).To(MatchError(runstate.ErrCorrupt))
	})

	It("rejects unsafe run ids (path traversal)", func() {
		_, err := store.Load(ctx, "../../etc/passwd")
		Expect(err).To(MatchError(runstate.ErrInvalidID))
		_, err = store.Create(ctx, "../evil", newMeta("../evil"))
		Expect(err).To(MatchError(runstate.ErrInvalidID))
	})

	It("lists and deletes runs", func() {
		id := newID()
		j, err := store.Create(ctx, id, newMeta(id))
		Expect(err).NotTo(HaveOccurred())
		Expect(j.Close()).To(Succeed())

		infos, err := store.List(ctx, runstate.ListFilter{})
		Expect(err).NotTo(HaveOccurred())
		Expect(infos).To(HaveLen(1))
		Expect(infos[0].RunID).To(Equal(id))
		Expect(infos[0].Model).To(Equal("claude-opus-4-8"))

		Expect(store.Delete(ctx, id)).To(Succeed())
		_, err = store.Load(ctx, id)
		Expect(err).To(MatchError(runstate.ErrNotFound))
	})

	Describe("listing by agent", func() {
		// One run each for two agents sharing this store, plus one written before
		// Store.Create stamped an agent, which is what an empty agent means.
		createFor := func(agent string) string {
			GinkgoHelper()

			id := newID()
			meta := newMeta(id)
			meta.Agent = agent

			j, err := store.Create(ctx, id, meta)
			Expect(err).NotTo(HaveOccurred())
			Expect(j.Close()).To(Succeed())

			return id
		}

		agentsByID := func(infos []runstate.RunInfo) map[string]string {
			out := map[string]string{}
			for _, info := range infos {
				out[info.RunID] = info.Agent
			}

			return out
		}

		It("carries the agent on every row and lists every run for a zero filter", func() {
			a := createFor("agent-a")
			b := createFor("agent-b")
			unstamped := createFor("")

			infos, err := store.List(ctx, runstate.ListFilter{})
			Expect(err).NotTo(HaveOccurred())
			Expect(agentsByID(infos)).To(Equal(map[string]string{a: "agent-a", b: "agent-b", unstamped: ""}))
		})

		It("lists one agent's runs and the runs nobody recorded an agent for", func() {
			a := createFor("agent-a")
			createFor("agent-b")
			unstamped := createFor("")

			infos, err := store.List(ctx, runstate.ListFilter{Agent: "agent-a"})
			Expect(err).NotTo(HaveOccurred())
			Expect(agentsByID(infos)).To(Equal(map[string]string{a: "agent-a", unstamped: ""}))
		})
	})

	// One store is handed to every channel, so a channel that derives its run ids under
	// a marker of its own lists the conversations it minted and leaves the rest out.
	Describe("listing by id prefix", func() {
		createID := func(id string) string {
			GinkgoHelper()

			j, err := store.Create(ctx, id, newMeta(id))
			Expect(err).NotTo(HaveOccurred())
			Expect(j.Close()).To(Succeed())

			return id
		}

		idsOf := func(infos []runstate.RunInfo) []string {
			out := make([]string, len(infos))
			for i, info := range infos {
				out[i] = info.RunID
			}

			return out
		}

		It("lists the runs under one prefix and pages them", func() {
			mine := createID("w-" + newID())
			also := createID("w-" + newID())
			createID("slack-" + newID())

			infos, err := store.List(ctx, runstate.ListFilter{Prefix: "w-"})
			Expect(err).NotTo(HaveOccurred())
			Expect(idsOf(infos)).To(ConsistOf(mine, also))

			page, err := store.ListPage(ctx, runstate.ListFilter{Prefix: "w-"}, 10, "")
			Expect(err).NotTo(HaveOccurred())
			Expect(idsOf(page.Runs)).To(ConsistOf(mine, also))
		})

		It("fills a page from the runs under the prefix rather than shortening it", func() {
			for range 3 {
				createID("slack-" + newID())
				createID("w-" + newID())
			}

			page, err := store.ListPage(ctx, runstate.ListFilter{Prefix: "w-"}, 3, "")
			Expect(err).NotTo(HaveOccurred())
			Expect(page.Runs).To(HaveLen(3))
			for _, info := range page.Runs {
				Expect(info.RunID).To(HavePrefix("w-"))
			}
		})
	})

	Describe("listing the conversation summary", func() {
		// A run whose terminal record carries a summary, and one whose terminal record
		// predates the field, which is every conversation journaled before this build.
		createEnded := func(summary *runstate.ConversationSummary) string {
			GinkgoHelper()

			id := newID()
			j, err := store.Create(ctx, id, newMeta(id))
			Expect(err).NotTo(HaveOccurred())

			err = j.Append(ctx, 2, runstate.Record{Protocol: runstate.TerminalProtocol, Terminal: &runstate.TerminalRecord{
				Reason:  runstate.ReasonCompleted,
				Summary: summary,
			}})
			Expect(err).NotTo(HaveOccurred())
			Expect(j.Close()).To(Succeed())

			return id
		}

		It("carries the summary off the terminal record", func() {
			want := &runstate.ConversationSummary{
				Turns:         3,
				ContextTokens: 4096,
				Counters:      runstate.Counters{LlmCalls: 5, ToolCalls: 2, InTokens: 900},
			}
			id := createEnded(want)

			infos, err := store.List(ctx, runstate.ListFilter{})
			Expect(err).NotTo(HaveOccurred())
			Expect(infos).To(HaveLen(1))
			Expect(infos[0].RunID).To(Equal(id))
			Expect(infos[0].Summary).To(Equal(want))
		})

		It("reports no summary for a conversation whose last turn wrote none", func() {
			createEnded(nil)

			infos, err := store.List(ctx, runstate.ListFilter{})
			Expect(err).NotTo(HaveOccurred())
			Expect(infos).To(HaveLen(1))
			Expect(infos[0].Terminal).To(Equal(runstate.ReasonCompleted))
			Expect(infos[0].Summary).To(BeNil(), "absent, so a rail draws an empty slot rather than a turn count of zero")
		})

		It("reports no summary for a conversation that has written no terminal record", func() {
			id := newID()
			j, err := store.Create(ctx, id, newMeta(id))
			Expect(err).NotTo(HaveOccurred())
			Expect(j.Close()).To(Succeed())

			infos, err := store.List(ctx, runstate.ListFilter{})
			Expect(err).NotTo(HaveOccurred())
			Expect(infos).To(HaveLen(1))
			Expect(infos[0].Summary).To(BeNil())
		})
	})

	It("refuses a listing on a context canceled before the call", func() {
		id := newID()
		j, err := store.Create(ctx, id, newMeta(id))
		Expect(err).NotTo(HaveOccurred())
		Expect(j.Close()).To(Succeed())

		canceled, cancel := context.WithCancel(ctx)
		cancel()

		infos, err := store.List(canceled, runstate.ListFilter{})
		Expect(err).To(MatchError(context.Canceled))
		Expect(infos).To(BeEmpty())
	})

	// The listing reads one journal per run, so it reads the context once per run too.
	// A cancel that lands after the first of them has to fail the call: the runs read so
	// far are a prefix, and returning them names a store holding fewer than it does.
	It("fails a listing canceled part way through rather than returning the runs it reached", func() {
		for range 3 {
			id := newID()
			j, err := store.Create(ctx, id, newMeta(id))
			Expect(err).NotTo(HaveOccurred())
			Expect(j.Close()).To(Succeed())
		}

		// Live for the check at the start and for the first run, canceled from the
		// second on. A real cancel here would race the filesystem and pass whether or
		// not the per-run check exists.
		partway := &liveForReads{Context: ctx, reads: 2}

		infos, err := store.List(partway, runstate.ListFilter{})
		Expect(err).To(MatchError(context.Canceled))
		Expect(infos).To(BeEmpty())
	})

	// A canceled caller is refused before the store touches the filesystem, and the
	// refusal is the context's own error so a caller can tell it from a store failure.
	It("refuses a load on a canceled context and leaves the run readable", func() {
		id := newID()
		j, err := store.Create(ctx, id, newMeta(id))
		Expect(err).NotTo(HaveOccurred())
		Expect(j.Close()).To(Succeed())

		canceled, cancel := context.WithCancel(ctx)
		cancel()

		_, err = store.Load(canceled, id)
		Expect(err).To(MatchError(context.Canceled))

		rs, err := store.Load(ctx, id)
		Expect(err).NotTo(HaveOccurred())
		Expect(rs.RunID).To(Equal(id))
	})

	// The refused append writes no line at all, so the journal is where it was and the
	// same seq is still the next one to write.
	It("refuses an append on a canceled context and writes nothing", func() {
		id := newID()
		j, err := store.Create(ctx, id, newMeta(id))
		Expect(err).NotTo(HaveOccurred())
		defer j.Close()

		canceled, cancel := context.WithCancel(ctx)
		cancel()

		rec := runstate.Record{Protocol: runstate.AssistantProtocol, Assistant: assistantWithTools(0, "tu_1")}
		Expect(j.Append(canceled, 2, rec)).To(MatchError(context.Canceled))
		Expect(j.LastSeq()).To(Equal(uint64(1)))

		recs, err := j.Records(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(recs).To(HaveLen(1), "the meta record and nothing else")

		Expect(j.Append(ctx, 2, rec)).To(Succeed(), "the seq the canceled call refused is still the next one")
	})

	Describe("paging the listing", func() {
		// Each run is created a minute after the one before, so the order the pages walk
		// is the one the meta records carry rather than the one the ids happen to sort
		// in. createAt returns the id so a spec can name the run it expects.
		base := time.Unix(1700000000, 0).UTC()

		createAt := func(minute int, agent string) string {
			GinkgoHelper()

			id := newID()
			meta := newMeta(id)
			meta.Created = base.Add(time.Duration(minute) * time.Minute)
			meta.Agent = agent

			j, err := store.Create(ctx, id, meta)
			Expect(err).NotTo(HaveOccurred())
			Expect(j.Close()).To(Succeed())

			return id
		}

		idsOf := func(page runstate.RunPage) []string {
			out := make([]string, 0, len(page.Runs))
			for _, info := range page.Runs {
				out = append(out, info.RunID)
			}

			return out
		}

		// walk pages the whole store the way a caller does: it hands back the cursor it
		// was given until a page carries none.
		walk := func(filter runstate.ListFilter, limit int) []string {
			GinkgoHelper()

			var out []string
			cursor := ""
			for range 20 {
				page, err := store.ListPage(ctx, filter, limit, cursor)
				Expect(err).NotTo(HaveOccurred())
				out = append(out, idsOf(page)...)
				if page.Cursor == "" {
					return out
				}
				cursor = page.Cursor
			}

			Fail("the walk never reached a page without a cursor")

			return nil
		}

		It("returns a page smaller than the store, oldest first, with a cursor", func() {
			first := createAt(1, "")
			second := createAt(2, "")
			createAt(3, "")

			page, err := store.ListPage(ctx, runstate.ListFilter{}, 2, "")
			Expect(err).NotTo(HaveOccurred())
			Expect(idsOf(page)).To(Equal([]string{first, second}))
			Expect(page.Cursor).ToNot(BeEmpty())
		})

		It("continues at the run the page ended on, repeating none and skipping none", func() {
			var want []string
			for i := range 5 {
				want = append(want, createAt(i+1, ""))
			}

			Expect(walk(runstate.ListFilter{}, 2)).To(Equal(want))
			Expect(walk(runstate.ListFilter{}, 3)).To(Equal(want))
			Expect(walk(runstate.ListFilter{}, 1)).To(Equal(want))
		})

		It("carries the same row a full listing does", func() {
			id := createAt(1, "agent-a")

			page, err := store.ListPage(ctx, runstate.ListFilter{}, 10, "")
			Expect(err).NotTo(HaveOccurred())

			infos, err := store.List(ctx, runstate.ListFilter{})
			Expect(err).NotTo(HaveOccurred())
			Expect(infos).To(HaveLen(1))
			Expect(page.Runs).To(Equal(infos))
			Expect(page.Runs[0].RunID).To(Equal(id))
			Expect(page.Runs[0].Created).To(Equal(base.Add(time.Minute)))
		})

		It("ends the walk on a page carrying no cursor", func() {
			createAt(1, "")
			createAt(2, "")
			createAt(3, "")

			page, err := store.ListPage(ctx, runstate.ListFilter{}, 2, "")
			Expect(err).NotTo(HaveOccurred())

			page, err = store.ListPage(ctx, runstate.ListFilter{}, 2, page.Cursor)
			Expect(err).NotTo(HaveOccurred())
			Expect(page.Runs).To(HaveLen(1))
			Expect(page.Cursor).To(BeEmpty())
		})

		// A page that fills at the last run cannot tell that nothing follows without
		// another read, so it carries a cursor and the caller finds an empty page there.
		It("hands a full page a cursor even at the end of the store, and answers it with an empty page", func() {
			createAt(1, "")
			createAt(2, "")

			page, err := store.ListPage(ctx, runstate.ListFilter{}, 2, "")
			Expect(err).NotTo(HaveOccurred())
			Expect(page.Runs).To(HaveLen(2))
			Expect(page.Cursor).ToNot(BeEmpty())

			page, err = store.ListPage(ctx, runstate.ListFilter{}, 2, page.Cursor)
			Expect(err).NotTo(HaveOccurred())
			Expect(page.Runs).To(BeEmpty())
			Expect(page.Cursor).To(BeEmpty())
		})

		// The filter is answered from the meta record, which the ordering read already
		// has, so an excluded run is dropped before its journal is folded. Folding it
		// first cost a whole-journal read per run another agent wrote, which is the read
		// a page exists to avoid. The page it produces is the same either way, so this
		// pins the enumeration carrying the agent rather than the page's rows.
		It("carries each run's agent in the order, so a filter needs no fold", func() {
			a := createAt(1, "agent-a")
			b := createAt(2, "agent-b")
			unstamped := createAt(3, "")

			// A body no fold can read. The order still names all three and their agents,
			// since it reads the meta line and stops.
			for _, id := range []string{a, b, unstamped} {
				f, err := os.OpenFile(store.journalPath(id), os.O_WRONLY|os.O_APPEND, 0o600)
				Expect(err).NotTo(HaveOccurred())
				_, err = f.WriteString("not a record\n{\"seq\":9}\n")
				Expect(err).NotTo(HaveOccurred())
				Expect(f.Close()).To(Succeed())

				_, err = store.summarize(id)
				Expect(err).To(HaveOccurred(), "the journal no longer folds")
			}

			order, err := store.creationOrder(ctx, runstate.ListFilter{})
			Expect(err).NotTo(HaveOccurred())
			Expect(order).To(HaveLen(3))

			agents := map[string]string{}
			for _, p := range order {
				agents[p.id] = p.agent
			}
			Expect(agents).To(Equal(map[string]string{a: "agent-a", b: "agent-b", unstamped: ""}))
		})

		It("skips another agent's runs without shortening the page", func() {
			a1 := createAt(1, "agent-a")
			createAt(2, "agent-b")
			a2 := createAt(3, "agent-a")
			createAt(4, "agent-b")
			a3 := createAt(5, "agent-a")

			page, err := store.ListPage(ctx, runstate.ListFilter{Agent: "agent-a"}, 2, "")
			Expect(err).NotTo(HaveOccurred())
			Expect(idsOf(page)).To(Equal([]string{a1, a2}))

			Expect(walk(runstate.ListFilter{Agent: "agent-a"}, 2)).To(Equal([]string{a1, a2, a3}))
		})

		It("returns an empty page and no cursor for an empty store", func() {
			page, err := store.ListPage(ctx, runstate.ListFilter{}, 20, "")
			Expect(err).NotTo(HaveOccurred())
			Expect(page.Runs).To(BeEmpty())
			Expect(page.Cursor).To(BeEmpty())
		})

		It("returns the whole store, and no cursor, under a limit larger than it", func() {
			var want []string
			for i := range 3 {
				want = append(want, createAt(i+1, ""))
			}

			page, err := store.ListPage(ctx, runstate.ListFilter{}, 20, "")
			Expect(err).NotTo(HaveOccurred())
			Expect(idsOf(page)).To(Equal(want))
			Expect(page.Cursor).To(BeEmpty())
		})

		// Two conversations started in the same instant are ordered by run id, so the
		// cursor lands between them rather than repeating or skipping one.
		It("orders runs sharing a creation time by run id", func() {
			ids := []string{createAt(1, ""), createAt(1, ""), createAt(1, "")}
			slices.Sort(ids)

			Expect(walk(runstate.ListFilter{}, 1)).To(Equal(ids))
		})

		It("leaves out a run deleted between two pages", func() {
			first := createAt(1, "")
			second := createAt(2, "")
			third := createAt(3, "")

			page, err := store.ListPage(ctx, runstate.ListFilter{}, 1, "")
			Expect(err).NotTo(HaveOccurred())
			Expect(idsOf(page)).To(Equal([]string{first}))

			Expect(store.Delete(ctx, second)).To(Succeed())

			page, err = store.ListPage(ctx, runstate.ListFilter{}, 1, page.Cursor)
			Expect(err).NotTo(HaveOccurred())
			Expect(idsOf(page)).To(Equal([]string{third}))
		})

		// The cursor holds the creation time the caller stamped, so a run created after a
		// page but stamped earlier sorts before the cursor and a resumed walk never
		// reaches it. RunPage.Cursor records that as this backend's limit, against the
		// JetStream store's stream sequence which cannot go backwards, and a caller that
		// must see every run walks from an empty cursor.
		It("misses a run stamped earlier than the cursor, which a walk from the start finds", func() {
			createAt(10, "")
			createAt(20, "")

			page, err := store.ListPage(ctx, runstate.ListFilter{}, 2, "")
			Expect(err).NotTo(HaveOccurred())
			Expect(page.Cursor).ToNot(BeEmpty())

			late := createAt(15, "")

			page, err = store.ListPage(ctx, runstate.ListFilter{}, 2, page.Cursor)
			Expect(err).NotTo(HaveOccurred())
			Expect(page.Runs).To(BeEmpty())

			Expect(walk(runstate.ListFilter{}, 2)).To(ContainElement(late))
		})

		It("refuses a limit no page can hold", func() {
			_, err := store.ListPage(ctx, runstate.ListFilter{}, 0, "")
			Expect(err).To(MatchError(runstate.ErrInvalidLimit))

			_, err = store.ListPage(ctx, runstate.ListFilter{}, -1, "")
			Expect(err).To(MatchError(runstate.ErrInvalidLimit))
		})

		// The JetStream backend's cursor is a stream sequence, which this store has no
		// meaning for, so it is refused rather than read as the beginning.
		It("refuses a cursor it did not mint", func() {
			createAt(1, "")

			for _, cursor := range []string{"12", "not a cursor", "*", "MTIzNA"} {
				_, err := store.ListPage(ctx, runstate.ListFilter{}, 2, cursor)
				Expect(err).To(MatchError(runstate.ErrInvalidCursor), cursor)
			}
		})

		It("refuses a page on a context canceled before the call", func() {
			createAt(1, "")

			canceled, cancel := context.WithCancel(ctx)
			cancel()

			_, err := store.ListPage(canceled, runstate.ListFilter{}, 2, "")
			Expect(err).To(MatchError(context.Canceled))
		})
	})

	It("does not leak a sensitive prompt into the fingerprint on disk", func() {
		id := newID()
		meta := newMeta(id)
		meta.Fingerprint.SystemHash = runstate.HashHex([]byte("TOP-SECRET-INSTRUCTIONS"))
		j, err := store.Create(ctx, id, meta)
		Expect(err).NotTo(HaveOccurred())
		Expect(j.Close()).To(Succeed())

		data, err := os.ReadFile(store.journalPath(id))
		Expect(err).NotTo(HaveOccurred())
		Expect(bytes.Contains(data, []byte("TOP-SECRET-INSTRUCTIONS"))).To(BeFalse())
	})
})

var _ = Describe("Info", func() {
	It("reports the registered backend name", func() {
		s, err := NewFileStore(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())
		Expect(s.Info().Backend).To(Equal(runstate.BackendFile))
	})

	It("reports no location, since the only one it has is a local path", func() {
		dir := GinkgoT().TempDir()
		s, err := NewFileStore(dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(s.Info().Location).To(BeEmpty())
		Expect(s.Info().Location).NotTo(ContainSubstring(dir))
	})
})

var _ = Describe("newStore", func() {
	It("defaults an empty directory to the core XDG default, not the working directory", func() {
		def, err := runstate.DefaultDir()
		Expect(err).NotTo(HaveOccurred())

		s, err := newStore(runstate.RuntimeEnv{}, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(s.(*FileStore).dir).To(Equal(def))
		Expect(filepath.IsAbs(s.(*FileStore).dir)).To(BeTrue())
	})

	It("uses the directory option when set", func() {
		dir := GinkgoT().TempDir()
		s, err := newStore(runstate.RuntimeEnv{}, json.RawMessage(`{"directory":"`+dir+`"}`))
		Expect(err).NotTo(HaveOccurred())
		Expect(s.(*FileStore).dir).To(Equal(dir))
	})

	It("roots journals under a store base when one is set", func() {
		base := GinkgoT().TempDir()
		s, err := newStore(runstate.RuntimeEnv{StoreDir: base}, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(s.(*FileStore).dir).To(Equal(filepath.Join(base, "runs")))
	})

	It("honors an absolute configured directory regardless of the store base", func() {
		base := GinkgoT().TempDir()
		dir := GinkgoT().TempDir()
		s, err := newStore(runstate.RuntimeEnv{StoreDir: base}, json.RawMessage(`{"directory":"`+dir+`"}`))
		Expect(err).NotTo(HaveOccurred())
		Expect(s.(*FileStore).dir).To(Equal(dir))
	})

	It("rejects an unknown option key", func() {
		_, err := newStore(runstate.RuntimeEnv{}, json.RawMessage(`{"bogus":1}`))
		Expect(err).To(MatchError(ContainSubstring("invalid file session options")))
	})

	It("is registered under the file backend name", func() {
		Expect(runstate.Backends()).To(ContainElement(runstate.BackendFile))
	})
})
