//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package serve_test

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/agenttest"
	"github.com/choria-io/fisk-ai/internal/serve"
)

var _ = Describe("Channels", func() {
	var cfg *config.Config

	BeforeEach(func() {
		cfg = servedConfig()
	})

	It("Should build only what the configuration enables", func() {
		var built []string

		builders := []serve.ChannelBuilder{
			{
				Name:    "on",
				Enabled: func(*config.Config) bool { return true },
				Build: func(*config.Config, serve.BuildOptions) (serve.Channel, error) {
					built = append(built, "on")
					return agenttest.NewScriptedChannel(GinkgoTB(), "on"), nil
				},
			},
			{
				Name:    "off",
				Enabled: func(*config.Config) bool { return false },
				Build: func(*config.Config, serve.BuildOptions) (serve.Channel, error) {
					built = append(built, "off")
					return agenttest.NewScriptedChannel(GinkgoTB(), "off"), nil
				},
			},
		}

		channels, err := serve.Channels(cfg, builders, serve.BuildOptions{})
		Expect(err).ToNot(HaveOccurred())
		Expect(channels).To(HaveLen(1))
		Expect(channels[0].Name()).To(Equal("on"))
		Expect(built).To(Equal([]string{"on"}), "a disabled surface is never constructed")
	})

	It("Should build nothing when nothing is enabled", func() {
		channels, err := serve.Channels(cfg, []serve.ChannelBuilder{{
			Name:    "off",
			Enabled: func(*config.Config) bool { return false },
		}}, serve.BuildOptions{})

		Expect(err).ToNot(HaveOccurred())
		Expect(channels).To(BeEmpty())
	})

	// Several channels hold connections, so a failure part way through has to release
	// what it already built or it leaks them somewhere the caller cannot reach.
	It("Should release what it built when a later channel fails", func() {
		first := agenttest.NewQueue(GinkgoTB(), "first")

		builders := []serve.ChannelBuilder{
			{
				Name:    "first",
				Enabled: func(*config.Config) bool { return true },
				Build:   func(*config.Config, serve.BuildOptions) (serve.Channel, error) { return first, nil },
			},
			{
				Name:    "second",
				Enabled: func(*config.Config) bool { return true },
				Build: func(*config.Config, serve.BuildOptions) (serve.Channel, error) {
					return nil, fmt.Errorf("no queue")
				},
			},
		}

		_, err := serve.Channels(cfg, builders, serve.BuildOptions{Logger: quietLogger()})
		Expect(err).To(MatchError(ContainSubstring("building the second channel")))
		Expect(err).To(MatchError(ContainSubstring("no queue")))
		Expect(first.Closes()).To(Equal(1))
	})

	It("Should pass the process's decisions to every builder", func() {
		var got serve.BuildOptions

		_, err := serve.Channels(cfg, []serve.ChannelBuilder{{
			Name:    "one",
			Enabled: func(*config.Config) bool { return true },
			Build: func(_ *config.Config, opts serve.BuildOptions) (serve.Channel, error) {
				got = opts
				return agenttest.NewScriptedChannel(GinkgoTB(), "one"), nil
			},
		}}, serve.BuildOptions{Workers: 7})

		Expect(err).ToNot(HaveOccurred())
		Expect(got.Workers).To(Equal(7))
	})
})
