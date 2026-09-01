//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package asyncjobs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	natsd "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
)

// jobsContext starts an embedded JetStream server, creates the work queue the channel
// binds to, and writes a NATS context pointing at the server where natscontext.Connect
// looks. It returns the server, so a spec can ask it what names it was given, and the
// context name to configure. Everything goes away with the spec.
func jobsContext() (*natsd.Server, string) {
	GinkgoHelper()

	ns, err := natsd.NewServer(&natsd.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: GinkgoT().TempDir()})
	Expect(err).ToNot(HaveOccurred())

	go ns.Start()
	Expect(ns.ReadyForConnections(10 * time.Second)).To(BeTrue())
	DeferCleanup(ns.Shutdown)

	nc, err := nats.Connect(ns.ClientURL(), nats.UseOldRequestStyle(), nats.Name("spec setup"))
	Expect(err).ToNot(HaveOccurred())
	DeferCleanup(nc.Close)

	newQueue(nc, time.Minute, 1)

	home := GinkgoT().TempDir()
	GinkgoT().Setenv("XDG_CONFIG_HOME", home)

	dir := filepath.Join(home, "nats", "context")
	Expect(os.MkdirAll(dir, 0o700)).To(Succeed())

	body, err := json.Marshal(map[string]string{"url": ns.ClientURL()})
	Expect(err).ToNot(HaveOccurred())
	Expect(os.WriteFile(filepath.Join(dir, "spectest.json"), body, 0o600)).To(Succeed())

	return ns, "spectest"
}

// announcedNames are the connection names ns has been given, open and closed alike, so
// a spec reads what the server saw rather than what the client asked for.
func announcedNames(ns *natsd.Server) []string {
	GinkgoHelper()

	connz, err := ns.Connz(&natsd.ConnzOptions{State: natsd.ConnAll})
	Expect(err).ToNot(HaveOccurred())

	var out []string
	for _, c := range connz.Conns {
		out = append(out, c.Name)
	}

	return out
}

var _ = Describe("NewFromConfig", Label("integration"), func() {
	// The channel dials its own connection, so the product on the configuration is the
	// only thing deciding what an operator reads in nats server report connections for
	// this worker. Each case dials for real and asks the server what name it got.
	DescribeTable("Should announce the product the configuration names",
		func(product string, productVersion string, want string) {
			ns, ctxName := jobsContext()

			cfg := &config.Config{
				Identity:       "worker-3",
				NatsContext:    ctxName,
				Product:        product,
				ProductVersion: productVersion,
				Expose: &config.ExposeConfig{
					Agent: &config.AgentExpose{
						Jobs: &config.ExposedJobsConfig{Queue: testQueue, TaskType: testTaskType},
					},
				},
			}

			ch, err := NewFromConfig(context.Background(), cfg, ConfigOptions{Logger: quietLogger()})
			Expect(err).ToNot(HaveOccurred())
			DeferCleanup(ch.Close)

			Expect(announcedNames(ns)).To(ContainElement(want))
		},
		Entry("unset", "", "", "fisk-ai jobs worker-3"),
		Entry("product and version", "acme-agent", "4.5", "acme-agent/4.5 jobs worker-3"),
		Entry("product alone", "acme-agent", "", "acme-agent jobs worker-3"),
	)
})
