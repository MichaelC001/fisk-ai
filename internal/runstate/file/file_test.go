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

		infos, err := store.List(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(infos).To(HaveLen(1))
		Expect(infos[0].RunID).To(Equal(id))
		Expect(infos[0].Model).To(Equal("claude-opus-4-8"))

		Expect(store.Delete(ctx, id)).To(Succeed())
		_, err = store.Load(ctx, id)
		Expect(err).To(MatchError(runstate.ErrNotFound))
	})

	It("refuses a listing on a context canceled before the call", func() {
		id := newID()
		j, err := store.Create(ctx, id, newMeta(id))
		Expect(err).NotTo(HaveOccurred())
		Expect(j.Close()).To(Succeed())

		canceled, cancel := context.WithCancel(ctx)
		cancel()

		infos, err := store.List(canceled)
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

		infos, err := store.List(partway)
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
