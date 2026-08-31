//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package serve

import (
	"bytes"
	"context"
	"errors"
	"log/slog"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/llm"
	"github.com/choria-io/fisk-ai/internal/runstate"
)

// stubStore is the smallest runstate.Store the decorator can wrap. agenttest holds a
// fuller one and this package cannot import it: the fakes import serve, so an in-package
// test reaching for one is an import cycle.
type stubStore struct {
	backend string
	state   *runstate.RunState
	journal runstate.Journal
	err     error
}

func (s *stubStore) Info() runstate.Info { return runstate.Info{Backend: s.backend} }

func (s *stubStore) Create(context.Context, string, runstate.MetaRecord) (runstate.Journal, error) {
	return s.journal, s.err
}

func (s *stubStore) Open(context.Context, string) (runstate.Journal, error) {
	return s.journal, s.err
}

func (s *stubStore) Load(context.Context, string) (*runstate.RunState, error) {
	if s.err != nil {
		return nil, s.err
	}

	return s.state, nil
}

func (s *stubStore) List(context.Context) ([]runstate.RunInfo, error) { return nil, s.err }

func (s *stubStore) Delete(context.Context, string) error { return s.err }

// stubJournal is a journal that records nothing and can be told to fail its release.
type stubJournal struct {
	closeErr error
	closed   bool
}

func (j *stubJournal) Append(context.Context, uint64, runstate.Record) error { return nil }
func (j *stubJournal) Records(context.Context) ([]runstate.Record, error)    { return nil, nil }
func (j *stubJournal) LastSeq() uint64                                       { return 0 }
func (j *stubJournal) CheckHeld(context.Context) error                       { return nil }
func (j *stubJournal) Close() error                                          { j.closed = true; return j.closeErr }

var _ = Describe("withStoreLogging", func() {
	var (
		out     *bytes.Buffer
		log     *slog.Logger
		store   *stubStore
		journal *stubJournal
		ctx     = context.Background()
	)

	BeforeEach(func() {
		out = &bytes.Buffer{}
		log = slog.New(slog.NewJSONHandler(out, nil))
		journal = &stubJournal{}
		store = &stubStore{
			backend: "jetstream",
			state:   &runstate.RunState{Messages: []llm.Message{{}, {}, {}}},
			journal: journal,
		}
	})

	// A caller that wants no output pays nothing for the decision, and the store it gets
	// back is the one it passed in rather than a wrapper forwarding to a discarded log.
	It("Should leave the store alone without a logger", func() {
		Expect(withStoreLogging(store, nil)).To(BeIdenticalTo(runstate.Store(store)))
	})

	// The point of the line: a worker keeps no conversation in memory, so this is the
	// network round trip and the fold that every turn pays before the model is called.
	It("Should report a conversation read from the store", func() {
		rs, err := withStoreLogging(store, log).Load(ctx, "t-abc")
		Expect(err).ToNot(HaveOccurred())
		Expect(rs).To(Equal(store.state))

		Expect(out.String()).To(ContainSubstring(`"msg":"Read a conversation from the store"`))
		Expect(out.String()).To(ContainSubstring(`"session":"t-abc"`))
		Expect(out.String()).To(ContainSubstring(`"backend":"jetstream"`))
		Expect(out.String()).To(ContainSubstring(`"messages":3`))
		Expect(out.String()).To(ContainSubstring(`"duration":`))
		Expect(out.String()).To(ContainSubstring(`"level":"INFO"`))
	})

	It("Should report the journal a resume opens", func() {
		_, err := withStoreLogging(store, log).Open(ctx, "t-abc")
		Expect(err).ToNot(HaveOccurred())

		Expect(out.String()).To(ContainSubstring(`"msg":"Opened a conversation journal"`))
		Expect(out.String()).To(ContainSubstring(`"session":"t-abc"`))
		Expect(out.String()).To(ContainSubstring(`"duration":`))
	})

	// The release side of the pair: while the journal is open this worker holds the
	// conversation, and closing it is the point after which the next turn reads the
	// store again.
	It("Should report a conversation released back to the store", func() {
		j, err := withStoreLogging(store, log).Open(ctx, "t-abc")
		Expect(err).ToNot(HaveOccurred())

		Expect(j.Close()).To(Succeed())
		Expect(journal.closed).To(BeTrue(), "the release reaches the journal it wraps")

		Expect(out.String()).To(ContainSubstring(`"msg":"Released a conversation, the next turn reads it back"`))
		Expect(out.String()).To(ContainSubstring(`"session":"t-abc"`))
		Expect(out.String()).To(ContainSubstring(`"held":`))
	})

	// A conversation that did not exist yet is held the same way, so its acquisition is
	// reported too rather than leaving a release with nothing in front of it.
	It("Should report a conversation created and then released", func() {
		j, err := withStoreLogging(store, log).Create(ctx, "t-new", runstate.MetaRecord{})
		Expect(err).ToNot(HaveOccurred())
		Expect(j.Close()).To(Succeed())

		Expect(out.String()).To(ContainSubstring(`"msg":"Created a conversation journal"`))
		Expect(out.String()).To(ContainSubstring(`"msg":"Released a conversation, the next turn reads it back"`))
	})

	It("Should report a failed release as a warning and pass the error on", func() {
		journal.closeErr = errors.New("lock already taken")

		j, err := withStoreLogging(store, log).Open(ctx, "t-abc")
		Expect(err).ToNot(HaveOccurred())
		Expect(j.Close()).To(MatchError(ContainSubstring("lock already taken")))

		Expect(out.String()).To(ContainSubstring(`"msg":"Releasing a conversation failed"`))
		Expect(out.String()).To(ContainSubstring(`"level":"WARN"`))
	})

	// A store that failed took time too, and how long it took before failing is what
	// separates a refusal from a timeout.
	It("Should report a failed read as a warning and pass the error on", func() {
		store.err = errors.New("stream not found")

		_, err := withStoreLogging(store, log).Load(ctx, "t-abc")
		Expect(err).To(MatchError(ContainSubstring("stream not found")))

		Expect(out.String()).To(ContainSubstring(`"msg":"Reading a conversation failed"`))
		Expect(out.String()).To(ContainSubstring(`"level":"WARN"`))
		Expect(out.String()).To(ContainSubstring(`"duration":`))
	})

	It("Should report a failed open as a warning and pass the error on", func() {
		store.err = errors.New("already locked")

		_, err := withStoreLogging(store, log).Open(ctx, "t-abc")
		Expect(err).To(MatchError(ContainSubstring("already locked")))

		Expect(out.String()).To(ContainSubstring(`"msg":"Opening a conversation journal failed"`))
		Expect(out.String()).To(ContainSubstring(`"level":"WARN"`))
	})

	// Everything the decorator does not report still reaches the store it wraps, Info
	// among them: agent.Run compares the injected store's backend against the configured
	// one, so a wrapper that answered for itself would refuse the run.
	It("Should pass the wrapped store's own answers through", func() {
		Expect(withStoreLogging(store, log).Info().Backend).To(Equal("jetstream"))
	})
})
