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

		builders := []serve.SurfaceBuilder{
			{
				Name:    "on",
				Enabled: func(*config.Config) bool { return true },
				Build: func(*config.Config, serve.BuildOptions) ([]serve.Surface, error) {
					built = append(built, "on")
					return []serve.Surface{agenttest.NewScriptedChannel(GinkgoTB(), "on")}, nil
				},
			},
			{
				Name:    "off",
				Enabled: func(*config.Config) bool { return false },
				Build: func(*config.Config, serve.BuildOptions) ([]serve.Surface, error) {
					built = append(built, "off")
					return []serve.Surface{agenttest.NewScriptedChannel(GinkgoTB(), "off")}, nil
				},
			},
			{
				Name:    "service-on",
				Enabled: func(*config.Config) bool { return true },
				Build: func(*config.Config, serve.BuildOptions) ([]serve.Surface, error) {
					built = append(built, "service-on")
					return []serve.Surface{agenttest.NewService(GinkgoTB(), "service-on")}, nil
				},
			},
			{
				Name:    "service-off",
				Enabled: func(*config.Config) bool { return false },
				Build: func(*config.Config, serve.BuildOptions) ([]serve.Surface, error) {
					built = append(built, "service-off")
					return []serve.Surface{agenttest.NewService(GinkgoTB(), "service-off")}, nil
				},
			},
		}

		channels, builtServices, err := serve.Surfaces(cfg, serve.BuildOptions{}, builders)
		Expect(err).ToNot(HaveOccurred())
		Expect(channels).To(HaveLen(1))
		Expect(channels[0].Name()).To(Equal("on"))
		Expect(builtServices).To(HaveLen(1))
		Expect(builtServices[0].Name()).To(Equal("service-on"))
		Expect(built).To(Equal([]string{"on", "service-on"}), "a disabled surface is never constructed")
	})

	// One block can ask for both, which is how the a2a surfaces share a transport: the
	// builder returns them together and each lands in the list its kind is hosted from.
	It("Should sort the surfaces one builder returns", func() {
		builders := []serve.SurfaceBuilder{{
			Name:    "a2a",
			Enabled: func(*config.Config) bool { return true },
			Build: func(*config.Config, serve.BuildOptions) ([]serve.Surface, error) {
				return []serve.Surface{
					agenttest.NewQueue(GinkgoTB(), "a2a/prompts"),
					agenttest.NewService(GinkgoTB(), "a2a"),
				}, nil
			},
		}}

		channels, services, err := serve.Surfaces(cfg, serve.BuildOptions{}, builders)
		Expect(err).ToNot(HaveOccurred())
		Expect(channels).To(HaveLen(1))
		Expect(channels[0].Name()).To(Equal("a2a/prompts"))
		Expect(services).To(HaveLen(1))
		Expect(services[0].Name()).To(Equal("a2a"))
	})

	// A channel that can be released has Name and Close, which is everything Service
	// asks for, so a sort that asked both questions would host it twice: counted twice
	// at validation, closed twice on a drain and printed twice on the banner.
	It("Should host a releasable channel as a channel alone", func() {
		builders := []serve.SurfaceBuilder{{
			Name:    "jobs",
			Enabled: func(*config.Config) bool { return true },
			Build: func(*config.Config, serve.BuildOptions) ([]serve.Surface, error) {
				return []serve.Surface{agenttest.NewQueue(GinkgoTB(), "jobs")}, nil
			},
		}}

		channels, services, err := serve.Surfaces(cfg, serve.BuildOptions{}, builders)
		Expect(err).ToNot(HaveOccurred())
		Expect(channels).To(HaveLen(1))
		Expect(services).To(BeEmpty())
	})

	It("Should refuse a surface that is neither a channel nor a service", func() {
		builders := []serve.SurfaceBuilder{{
			Name:    "odd",
			Enabled: func(*config.Config) bool { return true },
			Build: func(*config.Config, serve.BuildOptions) ([]serve.Surface, error) {
				return []serve.Surface{namedSurface("odd")}, nil
			},
		}}

		_, _, err := serve.Surfaces(cfg, serve.BuildOptions{Logger: quietLogger()}, builders)
		Expect(err).To(MatchError(ContainSubstring("the odd surface built")))
		Expect(err).To(MatchError(ContainSubstring("neither a Channel nor a Service")))
	})

	It("Should build nothing when nothing is enabled", func() {
		channels, services, err := serve.Surfaces(cfg, serve.BuildOptions{}, []serve.SurfaceBuilder{{
			Name:    "off",
			Enabled: func(*config.Config) bool { return false },
		}, {
			Name:    "service-off",
			Enabled: func(*config.Config) bool { return false },
		}})

		Expect(err).ToNot(HaveOccurred())
		Expect(channels).To(BeEmpty())
		Expect(services).To(BeEmpty())
	})

	// Several channels hold connections, so a failure part way through has to release
	// what it already built or it leaks them somewhere the caller cannot reach.
	It("Should release what it built when a later surface fails", func() {
		first := agenttest.NewQueue(GinkgoTB(), "first")

		builders := []serve.SurfaceBuilder{
			{
				Name:    "first",
				Enabled: func(*config.Config) bool { return true },
				Build: func(*config.Config, serve.BuildOptions) ([]serve.Surface, error) {
					return []serve.Surface{first}, nil
				},
			},
			{
				Name:    "second",
				Enabled: func(*config.Config) bool { return true },
				Build: func(*config.Config, serve.BuildOptions) ([]serve.Surface, error) {
					return nil, fmt.Errorf("no queue")
				},
			},
		}

		_, _, err := serve.Surfaces(cfg, serve.BuildOptions{Logger: quietLogger()}, builders)
		Expect(err).To(MatchError(ContainSubstring("building the second surface")))
		Expect(err).To(MatchError(ContainSubstring("no queue")))
		Expect(first.Closes()).To(Equal(1))
	})

	// Everything is built by one call for this case: a service that fails after the
	// channels are built is what two calls cannot clean up, since the caller holds an
	// error and no channels.
	It("Should release the channels it built when a service fails", func() {
		queue := agenttest.NewQueue(GinkgoTB(), "jobs")
		service := agenttest.NewService(GinkgoTB(), "first")

		builders := []serve.SurfaceBuilder{
			{
				Name:    "jobs",
				Enabled: func(*config.Config) bool { return true },
				Build: func(*config.Config, serve.BuildOptions) ([]serve.Surface, error) {
					return []serve.Surface{queue}, nil
				},
			},
			{
				Name:    "first",
				Enabled: func(*config.Config) bool { return true },
				Build: func(*config.Config, serve.BuildOptions) ([]serve.Surface, error) {
					return []serve.Surface{service}, nil
				},
			},
			{
				Name:    "second",
				Enabled: func(*config.Config) bool { return true },
				Build: func(*config.Config, serve.BuildOptions) ([]serve.Surface, error) {
					return nil, fmt.Errorf("no transport")
				},
			},
		}

		_, _, err := serve.Surfaces(cfg, serve.BuildOptions{Logger: quietLogger()}, builders)
		Expect(err).To(MatchError(ContainSubstring("building the second surface")))
		Expect(err).To(MatchError(ContainSubstring("no transport")))
		Expect(queue.Closes()).To(Equal(1))
		Expect(service.Closes()).To(Equal(1))
	})

	It("Should pass the process's decisions to every builder", func() {
		var seen []serve.BuildOptions

		_, _, err := serve.Surfaces(cfg, serve.BuildOptions{Workers: 7, ConfigFile: "agent.yaml"}, []serve.SurfaceBuilder{{
			Name:    "one",
			Enabled: func(*config.Config) bool { return true },
			Build: func(_ *config.Config, opts serve.BuildOptions) ([]serve.Surface, error) {
				seen = append(seen, opts)
				return []serve.Surface{agenttest.NewScriptedChannel(GinkgoTB(), "one")}, nil
			},
		}, {
			Name:    "two",
			Enabled: func(*config.Config) bool { return true },
			Build: func(_ *config.Config, opts serve.BuildOptions) ([]serve.Surface, error) {
				seen = append(seen, opts)
				return []serve.Surface{agenttest.NewService(GinkgoTB(), "two")}, nil
			},
		}})

		Expect(err).ToNot(HaveOccurred())
		Expect(seen).To(HaveLen(2))
		Expect(seen[0].Workers).To(Equal(7))
		Expect(seen[1].Workers).To(Equal(7))
		Expect(seen[1].ConfigFile).To(Equal("agent.yaml"))
	})
})

// namedSurface is a value that answers Name and nothing else, which is what a builder
// returning the wrong thing produces.
type namedSurface string

func (n namedSurface) Name() string { return string(n) }
