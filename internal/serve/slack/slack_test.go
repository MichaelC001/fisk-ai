//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package slack

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"
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

	ch, err := newChannel(context.Background(), opts, a, s, log)
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

		// The fields, not the environment variables: a caller assembling Options in
		// process never set either variable and cannot fix one by setting it.
		Expect(missing(func(o *Options) { o.AppToken = "" })).To(ContainSubstring("AppToken is required"))
		Expect(missing(func(o *Options) { o.BotToken = "" })).To(ContainSubstring("BotToken is required"))
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

		lines := map[string]string{}
		for _, line := range ch.Describe() {
			lines[line.Label] = line.Value
		}

		Expect(lines["Workspace"]).To(Equal("Example (T000)"))
		Expect(lines["Bot"]).To(Equal("NATS Docs (U0BOT)"))
	})

	It("Should refuse a credential Slack does not accept, naming the variable to fix", func() {
		api := newFakeAPI()
		api.authErr = fmt.Errorf("invalid_auth")

		_, err := newChannel(context.Background(), testOptions(), api, newFakeSocket(), quietLogger())
		Expect(err).To(MatchError(ContainSubstring(botTokenVar)))
		Expect(err).To(MatchError(ContainSubstring("invalid_auth")))
	})

	It("Should refuse a context that has already ended, since the identity check reaches Slack", func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		api := newFakeAPI()

		_, err := newChannel(ctx, testOptions(), api, newFakeSocket(), quietLogger())
		Expect(err).To(MatchError(context.Canceled))
		Expect(api.auths).To(Equal(0))
	})

	It("Should hold the socket open past the context it was built under", func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		socket := newFakeSocket()

		ch, err := newChannel(ctx, testOptions(), newFakeAPI(), socket, quietLogger())
		Expect(err).ToNot(HaveOccurred())

		ch.start()
		Eventually(socket.ran).Should(BeClosed())

		cancel()

		Consistently(ch.socketEnd, 100*time.Millisecond).ShouldNot(BeClosed())

		Expect(ch.Close()).To(Succeed())
	})

	It("Should open no connection until it is served", func() {
		socket := newFakeSocket()
		ch := newTestChannel(testOptions(), newFakeAPI(), socket)

		Consistently(socket.ran, 100*time.Millisecond).ShouldNot(BeClosed())

		Expect(ch.Close()).To(Succeed())
	})

	Describe("ContextLines", func() {
		// The direct path reaches neither the configuration parser nor its defaults, so a
		// caller who set no allowance would otherwise get a bot deaf to its own thread.
		It("Should read the default allowance when it is unset", func() {
			api := newFakeAPI()
			api.history["C1"] = []message{said("U1", "the deploy went out at four", "1700000000.000010")}

			opts := testOptions()
			opts.ContextLines = 0

			ch := newTestChannel(opts, api, newFakeSocket())

			m := &mention{ChannelID: "C1", ThreadTS: "1700000000.000100", TS: "1700000000.000100", UserID: "U1"}

			msgs, err := ch.preload(context.Background(), m)
			Expect(err).ToNot(HaveOccurred())
			Expect(msgs).To(HaveLen(1))

			Expect(ch.Describe()).To(ContainElement(serve.DescLine{Label: "Context Lines", Value: strconv.Itoa(defaultContextLines)}))
		})

		It("Should read none for a negative allowance, and report that as zero", func() {
			api := newFakeAPI()
			api.history["C1"] = []message{said("U1", "the deploy went out at four", "1700000000.000010")}

			opts := testOptions()
			opts.ContextLines = -1

			ch := newTestChannel(opts, api, newFakeSocket())

			m := &mention{ChannelID: "C1", ThreadTS: "1700000000.000100", TS: "1700000000.000100", UserID: "U1"}

			msgs, err := ch.preload(context.Background(), m)
			Expect(err).ToNot(HaveOccurred())
			Expect(msgs).To(BeEmpty())

			Expect(ch.Describe()).To(ContainElement(serve.DescLine{Label: "Context Lines", Value: "0"}))
		})

		It("Should read the allowance a caller set", func() {
			api := newFakeAPI()
			api.history["C1"] = []message{
				said("U1", "one", "1700000000.000010"),
				said("U1", "two", "1700000000.000020"),
				said("U1", "three", "1700000000.000030"),
			}

			opts := testOptions()
			opts.ContextLines = 2

			ch := newTestChannel(opts, api, newFakeSocket())

			m := &mention{ChannelID: "C1", ThreadTS: "1700000000.000100", TS: "1700000000.000100", UserID: "U1"}

			msgs, err := ch.preload(context.Background(), m)
			Expect(err).ToNot(HaveOccurred())
			Expect(msgs).To(HaveLen(2))

			Expect(ch.Describe()).To(ContainElement(serve.DescLine{Label: "Context Lines", Value: "2"}))
		})
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

	Describe("Describe", func() {
		It("Should name the workspace, the bot and the limits a turn answers under", func() {
			ch := newTestChannel(testOptions(), newFakeAPI(), newFakeSocket())

			Expect(ch.Describe()).To(Equal([]serve.DescLine{
				{Label: "Workspace", Value: "Example (T000)"},
				{Label: "Bot", Value: "NATS Docs (U0BOT)"},
				{Label: "Workers", Value: "5"},
				{Label: "Answer Grace", Value: "30s"},
				{Label: "Context Lines", Value: "20"},
				{Label: "Progress", Value: "on"},
			}))
		})

		// no_progress is one absent status message per turn, which is a change to what
		// every thread looks like. Reporting it by leaving the line out would read as a
		// banner that forgot rather than as a bot that stays quiet.
		It("Should report a channel posting no status message as off", func() {
			opts := testOptions()
			opts.Progress = false

			ch := newTestChannel(opts, newFakeAPI(), newFakeSocket())

			Expect(ch.Describe()).To(ContainElement(serve.DescLine{Label: "Progress", Value: "off"}))
		})
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
