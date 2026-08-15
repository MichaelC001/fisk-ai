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

var _ = Describe("Surfaces", func() {
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

		services := []serve.ServiceBuilder{
			{
				Name:    "service-on",
				Enabled: func(*config.Config) bool { return true },
				Build: func(*config.Config, serve.BuildOptions) (serve.Service, error) {
					built = append(built, "service-on")
					return agenttest.NewService(GinkgoTB(), "service-on"), nil
				},
			},
			{
				Name:    "service-off",
				Enabled: func(*config.Config) bool { return false },
				Build: func(*config.Config, serve.BuildOptions) (serve.Service, error) {
					built = append(built, "service-off")
					return agenttest.NewService(GinkgoTB(), "service-off"), nil
				},
			},
		}

		channels, builtServices, err := serve.Surfaces(cfg, serve.BuildOptions{}, builders, services)
		Expect(err).ToNot(HaveOccurred())
		Expect(channels).To(HaveLen(1))
		Expect(channels[0].Name()).To(Equal("on"))
		Expect(builtServices).To(HaveLen(1))
		Expect(builtServices[0].Name()).To(Equal("service-on"))
		Expect(built).To(Equal([]string{"on", "service-on"}), "a disabled surface is never constructed")
	})

	It("Should build nothing when nothing is enabled", func() {
		channels, services, err := serve.Surfaces(cfg, serve.BuildOptions{}, []serve.ChannelBuilder{{
			Name:    "off",
			Enabled: func(*config.Config) bool { return false },
		}}, []serve.ServiceBuilder{{
			Name:    "service-off",
			Enabled: func(*config.Config) bool { return false },
		}})

		Expect(err).ToNot(HaveOccurred())
		Expect(channels).To(BeEmpty())
		Expect(services).To(BeEmpty())
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

		_, _, err := serve.Surfaces(cfg, serve.BuildOptions{Logger: quietLogger()}, builders, nil)
		Expect(err).To(MatchError(ContainSubstring("building the second channel")))
		Expect(err).To(MatchError(ContainSubstring("no queue")))
		Expect(first.Closes()).To(Equal(1))
	})

	// The reason both kinds are built by one call: a service that fails after the
	// channels are built is the case two calls cannot clean up, since the caller holds
	// an error and no channels.
	It("Should release the channels it built when a service fails", func() {
		queue := agenttest.NewQueue(GinkgoTB(), "jobs")
		service := agenttest.NewService(GinkgoTB(), "first")

		channels := []serve.ChannelBuilder{{
			Name:    "jobs",
			Enabled: func(*config.Config) bool { return true },
			Build:   func(*config.Config, serve.BuildOptions) (serve.Channel, error) { return queue, nil },
		}}

		services := []serve.ServiceBuilder{
			{
				Name:    "first",
				Enabled: func(*config.Config) bool { return true },
				Build:   func(*config.Config, serve.BuildOptions) (serve.Service, error) { return service, nil },
			},
			{
				Name:    "second",
				Enabled: func(*config.Config) bool { return true },
				Build: func(*config.Config, serve.BuildOptions) (serve.Service, error) {
					return nil, fmt.Errorf("no transport")
				},
			},
		}

		_, _, err := serve.Surfaces(cfg, serve.BuildOptions{Logger: quietLogger()}, channels, services)
		Expect(err).To(MatchError(ContainSubstring("building the second service")))
		Expect(err).To(MatchError(ContainSubstring("no transport")))
		Expect(queue.Closes()).To(Equal(1))
		Expect(service.Closes()).To(Equal(1))
	})

	It("Should pass the process's decisions to every builder", func() {
		var (
			channelOpts serve.BuildOptions
			serviceOpts serve.BuildOptions
		)

		_, _, err := serve.Surfaces(cfg, serve.BuildOptions{Workers: 7, ConfigFile: "agent.yaml"}, []serve.ChannelBuilder{{
			Name:    "one",
			Enabled: func(*config.Config) bool { return true },
			Build: func(_ *config.Config, opts serve.BuildOptions) (serve.Channel, error) {
				channelOpts = opts
				return agenttest.NewScriptedChannel(GinkgoTB(), "one"), nil
			},
		}}, []serve.ServiceBuilder{{
			Name:    "two",
			Enabled: func(*config.Config) bool { return true },
			Build: func(_ *config.Config, opts serve.BuildOptions) (serve.Service, error) {
				serviceOpts = opts
				return agenttest.NewService(GinkgoTB(), "two"), nil
			},
		}})

		Expect(err).ToNot(HaveOccurred())
		Expect(channelOpts.Workers).To(Equal(7))
		Expect(serviceOpts.Workers).To(Equal(7))
		Expect(serviceOpts.ConfigFile).To(Equal("agent.yaml"))
	})
})
