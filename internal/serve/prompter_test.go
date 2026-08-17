//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package serve

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/toolkit"
)

// recordingPrompter reports the deadline each question was put under, which is the whole
// of what the server decides about a question.
type recordingPrompter struct {
	deadlines []time.Time
	bounded   []bool
}

func (p *recordingPrompter) record(ctx context.Context) {
	deadline, ok := ctx.Deadline()
	p.deadlines = append(p.deadlines, deadline)
	p.bounded = append(p.bounded, ok)
}

func (p *recordingPrompter) CanPrompt() bool { return true }

func (p *recordingPrompter) ApproveCommand(ctx context.Context, _ toolkit.GateRequest) (toolkit.ConfirmChoice, error) {
	p.record(ctx)

	return toolkit.ConfirmOnce, nil
}

func (p *recordingPrompter) Confirm(ctx context.Context, _ string) (bool, error) {
	p.record(ctx)

	return true, nil
}

func (p *recordingPrompter) Select(ctx context.Context, _ string, _ []string) (int, error) {
	p.record(ctx)

	return 0, nil
}

func (p *recordingPrompter) Input(ctx context.Context, _, _ string) (string, error) {
	p.record(ctx)

	return "", nil
}

// ask puts one question of every kind, so a spec asserts about all four methods.
func ask(prompter toolkit.Prompter) {
	ctx := context.Background()

	_, err := prompter.ApproveCommand(ctx, toolkit.GateRequest{Command: "stream rm"})
	Expect(err).ToNot(HaveOccurred())

	_, err = prompter.Confirm(ctx, "Proceed?")
	Expect(err).ToNot(HaveOccurred())

	_, err = prompter.Select(ctx, "Which one?", []string{"east"})
	Expect(err).ToNot(HaveOccurred())

	_, err = prompter.Input(ctx, "Which subject?", "")
	Expect(err).ToNot(HaveOccurred())
}

var _ = Describe("Prompts", func() {
	Describe("promptsThrough", func() {
		It("Should deny when the channel supplies no prompter", func() {
			prompter := promptsThrough(&Work{})
			Expect(prompter.CanPrompt()).To(BeFalse())

			_, err := prompter.ApproveCommand(context.Background(), toolkit.GateRequest{Command: "stream rm"})
			Expect(err).To(HaveOccurred())
		})

		It("Should hold a question open for a channel whose operator is attached", func() {
			channel := &recordingPrompter{}

			prompter := promptsThrough(&Work{Prompter: channel, PromptsMayBlock: true, PromptWait: time.Second})
			Expect(prompter).To(BeIdenticalTo(toolkit.Prompter(channel)), "the prompter is the channel's own")

			ask(prompter)
			Expect(channel.bounded).To(Equal([]bool{false, false, false, false}), "the run context is the only bound")
		})

		It("Should bound a question for a channel whose caller is not attached", func() {
			channel := &recordingPrompter{}

			start := time.Now()
			ask(promptsThrough(&Work{Prompter: channel, PromptWait: 30 * time.Second}))

			Expect(channel.bounded).To(Equal([]bool{true, true, true, true}))
			for _, deadline := range channel.deadlines {
				Expect(deadline).To(BeTemporally("~", start.Add(30*time.Second), time.Second))
			}
		})

		It("Should bound a question that names no wait by the default", func() {
			channel := &recordingPrompter{}

			start := time.Now()
			ask(promptsThrough(&Work{Prompter: channel}))

			Expect(channel.bounded).To(Equal([]bool{true, true, true, true}))
			for _, deadline := range channel.deadlines {
				Expect(deadline).To(BeTemporally("~", start.Add(defaultPromptWait), time.Second))
			}
		})
	})
})
