//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package tasks

import (
	"encoding/json"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestTasks(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Tasks")
}

var _ = Describe("ValidateID", func() {
	It("Should accept the ids a submitter or an operator produces", func() {
		for _, id := range []string{"2abc", "a", "task-1", "task_1", strings.Repeat("a", maxIDLen)} {
			Expect(ValidateID(id)).To(Succeed(), "%q should be accepted", id)
		}
	})

	It("Should refuse anything that is not a safe, bounded path component", func() {
		for _, id := range []string{
			"", "..", "../escape", "a/b", "a\\b", "-lead", "_lead", "has space", "has.dot",
			strings.Repeat("a", maxIDLen+1),
		} {
			Expect(ValidateID(id)).To(MatchError(ErrInvalidID), "%q should be refused", id)
		}
	})
})

var _ = Describe("RequestID", func() {
	It("Should take the id from a request message", func() {
		id, err := RequestID(json.RawMessage(`{"protocol":"io.choria.fisk-ai.v1.request","id":"abc123","request":"abc123"}`))
		Expect(err).ToNot(HaveOccurred())
		Expect(id).To(Equal("abc123"))
	})

	It("Should refuse a body that is not a request", func() {
		for _, body := range []string{
			`not json`,
			`{"protocol":"io.choria.fisk-ai.v1.result","id":"abc123"}`,
			`{"protocol":"io.choria.fisk-ai.v1.request"}`,
			`{"protocol":"io.choria.fisk-ai.v1.request","id":"../escape"}`,
		} {
			_, err := RequestID(json.RawMessage(body))
			Expect(err).To(MatchError(ErrInvalidRequest), "%q should be refused", body)
		}
	})
})
