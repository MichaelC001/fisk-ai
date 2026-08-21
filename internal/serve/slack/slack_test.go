//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package slack

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/serve"
)

func TestSlack(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Serve/Slack")
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testOptions are valid options a spec varies one field of.
func testOptions() Options {
	return Options{
		AppToken:     "xapp-test",
		BotToken:     "xoxb-test",
		Identity:     "test.agent",
		Workers:      5,
		ContextLines: 20,
		Progress:     true,
		AnswerGrace:  30 * time.Second,
		MaxWaiting:   10,
		MaxCoalesced: 5,
		Sessions:     agenttest.NewFakeSessionStore(GinkgoTB()),
		Logger:       quietLogger(),
	}
}

// newTestChannel builds a Channel over the fakes, failing the spec if it refuses. It takes
// the logger from the options the way New does, so a spec asserting on what reaches the
// worker's log supplies one of its own.
func newTestChannel(opts Options, a api, s socket) *Channel {
	GinkgoHelper()

	log := opts.Logger
	if log == nil {
		log = quietLogger()
	}

	ch, err := newChannel(opts, a, s, log)
	Expect(err).ToNot(HaveOccurred())

	return ch
}

var _ = Describe("Options", func() {
	It("Should require both credentials, an identity and a worker count", func() {
		missing := func(mutate func(*Options)) string {
			opts := testOptions()
			mutate(&opts)

			err := opts.validate()
			Expect(err).To(HaveOccurred())

			return err.Error()
		}

		Expect(missing(func(o *Options) { o.AppToken = "" })).To(ContainSubstring(AppTokenVar))
		Expect(missing(func(o *Options) { o.BotToken = "" })).To(ContainSubstring(BotTokenVar))
		Expect(missing(func(o *Options) { o.Identity = "" })).To(ContainSubstring("identity is required"))
		Expect(missing(func(o *Options) { o.Workers = 0 })).To(ContainSubstring("greater than zero"))
		Expect(missing(func(o *Options) { o.Sessions = nil })).To(ContainSubstring("session store is required"))
	})

	It("Should accept options that name everything required", func() {
		opts := testOptions()
		Expect(opts.validate()).To(Succeed())
	})
})

var _ = Describe("New", func() {
	It("Should identify the credential at construction so a bad token fails at startup", func() {
		api := newFakeAPI()
		ch := newTestChannel(testOptions(), api, newFakeSocket())

		Expect(api.auths).To(Equal(1))

		team, teamID, bot := ch.Workspace()
		Expect(team).To(Equal("Example"))
		Expect(teamID).To(Equal("T000"))
		Expect(bot).To(Equal("U0BOT"))
	})

	It("Should refuse a credential Slack does not accept, naming the variable to fix", func() {
		api := newFakeAPI()
		api.authErr = fmt.Errorf("invalid_auth")

		_, err := newChannel(testOptions(), api, newFakeSocket(), quietLogger())
		Expect(err).To(MatchError(ContainSubstring(BotTokenVar)))
		Expect(err).To(MatchError(ContainSubstring("invalid_auth")))
	})

	It("Should open no connection until it is served", func() {
		socket := newFakeSocket()
		ch := newTestChannel(testOptions(), newFakeAPI(), socket)

		Consistently(socket.ran, 100*time.Millisecond).ShouldNot(BeClosed())

		Expect(ch.Close()).To(Succeed())
	})
})

var _ = Describe("Channel", func() {
	It("Should report its name and the worker count the server bounds it by", func() {
		opts := testOptions()
		opts.Workers = 3

		ch := newTestChannel(opts, newFakeAPI(), newFakeSocket())

		Expect(ch.Name()).To(Equal("slack"))
		Expect(ch.Concurrency()).To(Equal(3))
	})

	It("Should open the connection on the first Next", func() {
		socket := newFakeSocket()
		ch := newTestChannel(testOptions(), newFakeAPI(), socket)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		go func() {
			_, _ = ch.Next(ctx)
		}()

		Eventually(socket.ran).Should(BeClosed())

		cancel()
		Expect(ch.Close()).To(Succeed())
	})

	It("Should report the channel finished once it is closed, so the server stops asking", func() {
		ch := newTestChannel(testOptions(), newFakeAPI(), newFakeSocket())

		Expect(ch.Close()).To(Succeed())

		_, err := ch.Next(context.Background())
		Expect(err).To(MatchError(serve.ErrChannelDone))
	})

	It("Should answer a canceled context with its error rather than work", func() {
		ch := newTestChannel(testOptions(), newFakeAPI(), newFakeSocket())
		defer func() { Expect(ch.Close()).To(Succeed()) }()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, err := ch.Next(ctx)
		Expect(err).To(MatchError(context.Canceled))
	})

	It("Should be safe to close twice, since a drain and a stop both release it", func() {
		ch := newTestChannel(testOptions(), newFakeAPI(), newFakeSocket())

		Expect(ch.Close()).To(Succeed())
		Expect(ch.Close()).To(Succeed())
	})

	It("Should report a refused credential as a fault and answer it from Close", func() {
		socket := newFakeSocket()
		socket.fail(fmt.Errorf("invalid_auth"))

		ch := newTestChannel(testOptions(), newFakeAPI(), socket)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		go func() {
			_, _ = ch.Next(ctx)
		}()

		var faulted error
		Eventually(ch.Faults()).Should(Receive(&faulted))
		Expect(faulted).To(MatchError(ContainSubstring("invalid_auth")))

		Expect(ch.Close()).To(MatchError(ContainSubstring("invalid_auth")))
	})

	It("Should not fault when it is closed while connected", func() {
		socket := newFakeSocket()
		ch := newTestChannel(testOptions(), newFakeAPI(), socket)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		go func() {
			_, _ = ch.Next(ctx)
		}()

		Eventually(socket.ran).Should(BeClosed())

		Expect(ch.Close()).To(Succeed())
		Consistently(ch.Faults(), 100*time.Millisecond).ShouldNot(Receive())
	})

	It("Should report that it is draining once closed", func() {
		ch := newTestChannel(testOptions(), newFakeAPI(), newFakeSocket())

		Expect(ch.draining()).To(BeFalse())
		Expect(ch.Close()).To(Succeed())
		Expect(ch.draining()).To(BeTrue())
	})
})
