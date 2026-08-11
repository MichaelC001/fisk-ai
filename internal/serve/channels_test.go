//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package serve

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/agenttest"
)

// closableChannel records that it was released, so a spec can prove the ones already
// built are torn down when a sibling fails.
type closableChannel struct {
	*scriptedChannel

	closed int
}

func (c *closableChannel) Close() error {
	c.closed++

	return nil
}

// boundedChannel states a concurrency of its own, which is what a channel claiming
// work before a run starts has to do.
type boundedChannel struct {
	*scriptedChannel

	concurrency int
}

func (c *boundedChannel) Concurrency() int { return c.concurrency }

// servedConfig is a parsed configuration for a served agent, which every builder is
// handed whether or not it looks at one.
func servedConfig() *config.Config {
	GinkgoHelper()

	return agenttest.Config(GinkgoTB(), agenttest.NewFakeApp(GinkgoTB(), servedApp()))
}

var _ = Describe("Channels", func() {
	var cfg *config.Config

	BeforeEach(func() {
		cfg = servedConfig()
	})

	It("Should build only what the configuration enables", func() {
		var built []string

		builders := []ChannelBuilder{
			{
				Name:    "on",
				Enabled: func(*config.Config) bool { return true },
				Build: func(*config.Config, BuildOptions) (Channel, error) {
					built = append(built, "on")
					return newScriptedChannel("on"), nil
				},
			},
			{
				Name:    "off",
				Enabled: func(*config.Config) bool { return false },
				Build: func(*config.Config, BuildOptions) (Channel, error) {
					built = append(built, "off")
					return newScriptedChannel("off"), nil
				},
			},
		}

		channels, err := Channels(cfg, builders, BuildOptions{})
		Expect(err).ToNot(HaveOccurred())
		Expect(channels).To(HaveLen(1))
		Expect(channels[0].Name()).To(Equal("on"))
		Expect(built).To(Equal([]string{"on"}), "a disabled surface is never constructed")
	})

	It("Should build nothing when nothing is enabled", func() {
		channels, err := Channels(cfg, []ChannelBuilder{{
			Name:    "off",
			Enabled: func(*config.Config) bool { return false },
		}}, BuildOptions{})

		Expect(err).ToNot(HaveOccurred())
		Expect(channels).To(BeEmpty())
	})

	// Several channels hold connections, so a failure part way through has to release
	// what it already built or it leaks them somewhere the caller cannot reach.
	It("Should release what it built when a later channel fails", func() {
		first := &closableChannel{scriptedChannel: newScriptedChannel("first")}

		builders := []ChannelBuilder{
			{
				Name:    "first",
				Enabled: func(*config.Config) bool { return true },
				Build:   func(*config.Config, BuildOptions) (Channel, error) { return first, nil },
			},
			{
				Name:    "second",
				Enabled: func(*config.Config) bool { return true },
				Build: func(*config.Config, BuildOptions) (Channel, error) {
					return nil, fmt.Errorf("no queue")
				},
			},
		}

		_, err := Channels(cfg, builders, BuildOptions{Logger: quietLogger()})
		Expect(err).To(MatchError(ContainSubstring("building the second channel")))
		Expect(err).To(MatchError(ContainSubstring("no queue")))
		Expect(first.closed).To(Equal(1))
	})

	It("Should pass the process's decisions to every builder", func() {
		var got BuildOptions

		_, err := Channels(cfg, []ChannelBuilder{{
			Name:    "one",
			Enabled: func(*config.Config) bool { return true },
			Build: func(_ *config.Config, opts BuildOptions) (Channel, error) {
				got = opts
				return newScriptedChannel("one"), nil
			},
		}}, BuildOptions{Workers: 7})

		Expect(err).ToNot(HaveOccurred())
		Expect(got.Workers).To(Equal(7))
	})
})

var _ = Describe("Per-channel concurrency", func() {
	It("Should take a channel's own bound when it states one", func() {
		srv, err := New(Options{
			Channels:    []Channel{&boundedChannel{scriptedChannel: newScriptedChannel("bounded"), concurrency: 9}},
			Config:      servedConfig(),
			Concurrency: 2,
			Logger:      quietLogger(),
		})
		Expect(err).ToNot(HaveOccurred())

		Expect(srv.concurrencyFor(srv.opts.Channels[0])).To(Equal(9))
	})

	It("Should fall back to the configured default", func() {
		srv, err := New(Options{
			Channels:    []Channel{newScriptedChannel("plain")},
			Config:      servedConfig(),
			Concurrency: 3,
			Logger:      quietLogger(),
		})
		Expect(err).ToNot(HaveOccurred())

		Expect(srv.concurrencyFor(srv.opts.Channels[0])).To(Equal(3))

		// A channel that answers with nothing useful is the same as one that does not
		// answer at all, rather than a server with no slots that never runs anything.
		zero := &boundedChannel{scriptedChannel: newScriptedChannel("zero"), concurrency: 0}
		Expect(srv.concurrencyFor(zero)).To(Equal(3))
	})
})
