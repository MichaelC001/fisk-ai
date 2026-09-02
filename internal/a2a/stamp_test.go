//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package a2a

import (
	"context"
	"encoding/json"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	wire "github.com/choria-io/fisk-ai/internal/a2a/wire/v1"
)

var _ = Describe("StampRequest", func() {
	// The tag is the caller's own correlation and means nothing to a receiver, so the
	// turns of one conversation can carry one tag only if the stamp leaves it alone.
	It("Should keep a conversation tag the caller set and mint one when it did not", func() {
		fresh := wire.NewRequest("p")
		StampRequest(context.Background(), &fresh.Header, "caller1", "svc")
		Expect(fresh.Conversation).ToNot(BeEmpty())

		carried := wire.NewRequest("p")
		carried.Conversation = "conv1"
		StampRequest(context.Background(), &carried.Header, "caller1", "svc")
		Expect(carried.Conversation).To(Equal("conv1"))
	})

	// Canceling a task and answering its questions both name the request tag, so a
	// caller that only learned it when the call returned could not name the call it
	// was inside.
	It("Should keep a request tag the caller set and mint one when it did not", func() {
		fresh := wire.NewRequest("p")
		minted := fresh.Request
		Expect(minted).ToNot(BeEmpty(), "the constructor names the turn")
		StampRequest(context.Background(), &fresh.Header, "caller1", "svc")
		Expect(fresh.Request).To(Equal(minted), "the send keeps what it finds")

		carried := wire.NewRequest("p")
		carried.Request = "req1"
		StampRequest(context.Background(), &carried.Header, "caller1", "svc")
		Expect(carried.Request).To(Equal("req1"))
		Expect(carried.Conversation).ToNot(Equal("req1"), "a conversation the caller did not name is still fresh")
	})

	// The tag names the turn every reply echoes and the id names one message, so a
	// turn that sends more than one message can tell them apart.
	It("Should give the message an id of its own", func() {
		req := wire.NewRequest("p")
		req.Request = "req1"
		StampRequest(context.Background(), &req.Header, "caller1", "svc")

		Expect(req.ID).ToNot(BeEmpty())
		Expect(req.ID).ToNot(Equal("req1"))
	})

	// A tool or discovery RPC belongs to no larger task, so nothing minted a tag for
	// it and the send does, which is what its subject is built from.
	It("Should mint a request tag for a message that carries none", func() {
		tool := wire.NewToolRequest("read_file", json.RawMessage(`{}`))
		StampRequest(context.Background(), &tool.Header, "caller1", "svc")

		Expect(tool.Request).ToNot(BeEmpty())
		Expect(tool.Conversation).To(Equal(tool.Request))
	})

	// A constructor leaves four framing fields empty. The schema refuses a message
	// missing three of them, and takes a zero timestamp, which dates the message to
	// year one, so the spec checks the time itself.
	It("Should be what carries a constructed request past the schema", func() {
		validator, err := wire.NewValidator()
		Expect(err).ToNot(HaveOccurred())

		for _, req := range []*wire.Request{wire.NewRequest("do the thing"), wire.NewResume("2Ab3Cd4Ef5Gh")} {
			body, err := json.Marshal(req)
			Expect(err).ToNot(HaveOccurred())
			Expect(validator.Validate(body)).ToNot(Succeed(), string(req.Kind))

			StampRequest(context.Background(), &req.Header, "caller1", "svc")

			body, err = json.Marshal(req)
			Expect(err).ToNot(HaveOccurred())
			Expect(validator.Validate(body)).To(Succeed(), string(req.Kind))
			Expect(req.Time).To(BeTemporally("~", time.Now().UTC(), time.Minute), string(req.Kind))
		}
	})
})
