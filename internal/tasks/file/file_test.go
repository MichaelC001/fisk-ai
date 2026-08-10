//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package file

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/a2a"
	"github.com/choria-io/fisk-ai/internal/tasks"
)

func TestFileTaskStore(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Tasks/File")
}

// newRequest builds a schema-valid request message, which is what a real submitter
// would hand the store.
func newRequest(prompt string) json.RawMessage {
	req := a2a.NewRequest(prompt)
	req.ID = a2a.NewID()
	req.Request = req.ID
	req.Conversation = a2a.NewID()
	req.Sequence = 1
	req.Time = time.Now().UTC()
	req.Sender = a2a.Identity{Name: "submitter"}

	body, err := json.Marshal(req)
	Expect(err).ToNot(HaveOccurred())

	return body
}

func newResult(text string) json.RawMessage {
	res := a2a.NewResult(a2a.StopEndTurn)
	res.Text = text
	res.ID = a2a.NewID()
	res.Request = a2a.NewID()
	res.Conversation = a2a.NewID()
	res.Sequence = 2
	res.Time = time.Now().UTC()
	res.Sender = a2a.Identity{Name: "worker"}

	body, err := json.Marshal(res)
	Expect(err).ToNot(HaveOccurred())

	return body
}

var _ = Describe("Store", func() {
	var (
		ctx   context.Context
		dir   string
		store *Store
	)

	BeforeEach(func() {
		ctx = context.Background()
		dir = GinkgoTB().TempDir()

		var err error
		store, err = NewStore(dir)
		Expect(err).ToNot(HaveOccurred())
	})

	Describe("The record", func() {
		It("Should round-trip a task through submit, complete and load", func() {
			request := newRequest("investigate the outage")

			submitted, err := store.Submit(ctx, request)
			Expect(err).ToNot(HaveOccurred())
			Expect(submitted.State).To(Equal(tasks.StateSubmitted))
			Expect(submitted.Result).To(BeEmpty())
			Expect(submitted.Submitted).ToNot(BeZero())

			loaded, err := store.Load(ctx, submitted.ID)
			Expect(err).ToNot(HaveOccurred())
			Expect(loaded.ID).To(Equal(submitted.ID))
			Expect(loaded.State).To(Equal(tasks.StateSubmitted))
			Expect(loaded.Completed).To(BeZero())

			Expect(store.Complete(ctx, submitted.ID, newResult("it was DNS"))).To(Succeed())

			done, err := store.Load(ctx, submitted.ID)
			Expect(err).ToNot(HaveOccurred())
			Expect(done.State).To(Equal(tasks.StateCompleted))
			Expect(done.Completed).ToNot(BeZero())
			Expect(done.Result).ToNot(BeEmpty())
		})

		It("Should store messages that are still valid against the v1 schemas", func() {
			v, err := a2a.NewValidator()
			Expect(err).ToNot(HaveOccurred())

			submitted, err := store.Submit(ctx, newRequest("check the cluster"))
			Expect(err).ToNot(HaveOccurred())
			Expect(store.Complete(ctx, submitted.ID, newResult("all healthy"))).To(Succeed())

			done, err := store.Load(ctx, submitted.ID)
			Expect(err).ToNot(HaveOccurred())

			By("the stored request still validating and decoding as a request")
			Expect(v.Validate(done.Request)).To(Succeed())
			msg, err := a2a.Decode(done.Request)
			Expect(err).ToNot(HaveOccurred())
			Expect(msg).To(BeAssignableToTypeOf(&a2a.Request{}))
			Expect(msg.(*a2a.Request).Prompt).To(Equal("check the cluster"))

			By("the stored result still validating and decoding as a result")
			Expect(v.Validate(done.Result)).To(Succeed())
			msg, err = a2a.Decode(done.Result)
			Expect(err).ToNot(HaveOccurred())
			Expect(msg).To(BeAssignableToTypeOf(&a2a.Result{}))
			Expect(msg.(*a2a.Result).Text).To(Equal("all healthy"))
		})

		It("Should key the record on the request's own id", func() {
			request := newRequest("go")

			var hdr struct {
				ID string `json:"id"`
			}
			Expect(json.Unmarshal(request, &hdr)).To(Succeed())

			submitted, err := store.Submit(ctx, request)
			Expect(err).ToNot(HaveOccurred())
			Expect(submitted.ID).To(Equal(hdr.ID), "one id names the request and the record")
		})
	})

	Describe("Refusals", func() {
		It("Should refuse a second submission of the same request", func() {
			request := newRequest("go")

			_, err := store.Submit(ctx, request)
			Expect(err).ToNot(HaveOccurred())

			_, err = store.Submit(ctx, request)
			Expect(err).To(MatchError(tasks.ErrExists))
		})

		It("Should refuse to replace an answer that is already stored", func() {
			submitted, err := store.Submit(ctx, newRequest("go"))
			Expect(err).ToNot(HaveOccurred())

			Expect(store.Complete(ctx, submitted.ID, newResult("the answer"))).To(Succeed())
			Expect(store.Complete(ctx, submitted.ID, newResult("a later failure"))).
				To(MatchError(tasks.ErrCompleted), "a redelivery that failed must not overwrite a good answer")

			done, err := store.Load(ctx, submitted.ID)
			Expect(err).ToNot(HaveOccurred())

			msg, err := a2a.Decode(done.Result)
			Expect(err).ToNot(HaveOccurred())
			Expect(msg.(*a2a.Result).Text).To(Equal("the answer"))
		})

		It("Should report an unknown id", func() {
			_, err := store.Load(ctx, a2a.NewID())
			Expect(err).To(MatchError(tasks.ErrNotFound))

			Expect(store.Complete(ctx, a2a.NewID(), newResult("x"))).To(MatchError(tasks.ErrNotFound))
		})

		It("Should refuse an id that is not a safe path component", func() {
			_, err := store.Load(ctx, "../escape")
			Expect(err).To(MatchError(tasks.ErrInvalidID))

			Expect(store.Complete(ctx, "../escape", newResult("x"))).To(MatchError(tasks.ErrInvalidID))
		})

		It("Should refuse a body that is not a request message", func() {
			_, err := store.Submit(ctx, json.RawMessage(`{"protocol":"io.choria.fisk-ai.v1.result","id":"abc"}`))
			Expect(err).To(MatchError(tasks.ErrInvalidRequest))

			_, err = store.Submit(ctx, json.RawMessage(`not json`))
			Expect(err).To(MatchError(tasks.ErrInvalidRequest))
		})
	})

	Describe("On disk", func() {
		It("Should leave no staging file behind and keep records owner-only", func() {
			submitted, err := store.Submit(ctx, newRequest("go"))
			Expect(err).ToNot(HaveOccurred())
			Expect(store.Complete(ctx, submitted.ID, newResult("done"))).To(Succeed())

			entries, err := os.ReadDir(dir)
			Expect(err).ToNot(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Name()).To(Equal(submitted.ID + ".json"))

			st, err := os.Stat(filepath.Join(dir, entries[0].Name()))
			Expect(err).ToNot(HaveOccurred())
			Expect(st.Mode().Perm()).To(Equal(os.FileMode(fileMode)))
		})

		It("Should refuse to read a record that is not a regular file", func() {
			submitted, err := store.Submit(ctx, newRequest("go"))
			Expect(err).ToNot(HaveOccurred())

			path := filepath.Join(dir, submitted.ID+".json")
			Expect(os.Remove(path)).To(Succeed())
			Expect(os.Symlink("/etc/passwd", path)).To(Succeed())

			_, err = store.Load(ctx, submitted.ID)
			Expect(err).To(HaveOccurred())
			Expect(err).ToNot(MatchError(tasks.ErrNotFound))
		})
	})

	Describe("Construction", func() {
		It("Should build through the registry and report itself", func() {
			base := GinkgoTB().TempDir()

			s, err := tasks.New(BackendName, tasks.RuntimeEnv{StoreDir: base}, nil)
			Expect(err).ToNot(HaveOccurred())
			Expect(s.Info().Backend).To(Equal(BackendName))
			Expect(s.Info().Location).To(BeEmpty(), "a path must never leave through Info")

			_, err = s.Submit(ctx, newRequest("go"))
			Expect(err).ToNot(HaveOccurred())
			Expect(filepath.Join(base, defaultDir)).To(BeADirectory())
		})

		It("Should reject an unknown option", func() {
			_, err := tasks.New(BackendName, tasks.RuntimeEnv{}, json.RawMessage(`{"bogus":1}`))
			Expect(err).To(MatchError(ContainSubstring("bogus")))
		})
	})
})
