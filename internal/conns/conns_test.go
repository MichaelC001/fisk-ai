//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package conns

import (
	"context"
	"sync"
	"testing"
	"time"

	natsd "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestConns(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Internal/Conns")
}

// runBroker starts an in-process NATS server and returns its client URL.
func runBroker() string {
	GinkgoHelper()

	ns, err := natsd.NewServer(&natsd.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true})
	Expect(err).ToNot(HaveOccurred())

	go ns.Start()
	Expect(ns.ReadyForConnections(10 * time.Second)).To(BeTrue())
	DeferCleanup(ns.Shutdown)

	return ns.ClientURL()
}

// blackhole is TEST-NET-2, which drops rather than refuses, so a dial to it hangs
// until something stops it. A refused address would return at once and prove nothing
// about a caller's context.
const blackhole = "nats://198.51.100.1:4222"

var _ = Describe("Provider", func() {
	Describe("Nats", func() {
		It("Should return the provisioned connection", func() {
			nc := &nats.Conn{}
			Expect(New(WithNats(nc)).Nats()).To(BeIdenticalTo(nc))
		})

		It("Should return nil when no NATS connection was provisioned", func() {
			Expect(New().Nats()).To(BeNil())
		})

		It("Should be nil-safe on a nil Provider", func() {
			var p *Provider
			Expect(p.Nats()).To(BeNil())
		})
	})

	Describe("Close", func() {
		It("Should leave a borrowed connection open", func() {
			nc, err := nats.Connect(runBroker())
			Expect(err).ToNot(HaveOccurred())
			DeferCleanup(nc.Close)

			p := New(WithNats(nc))
			p.Close()

			Expect(nc.IsClosed()).To(BeFalse())
			Expect(p.Nats()).To(BeIdenticalTo(nc))
		})

		It("Should release a connection handed over as owned", func() {
			nc, err := nats.Connect(runBroker())
			Expect(err).ToNot(HaveOccurred())

			p := New(WithOwnedNats(nc))
			p.Close()

			Expect(nc.IsClosed()).To(BeTrue())
			Expect(p.Nats()).To(BeNil())
		})

		It("Should be safe to call twice and on a nil Provider", func() {
			nc, err := nats.Connect(runBroker())
			Expect(err).ToNot(HaveOccurred())

			p := New(WithOwnedNats(nc))
			p.Close()
			p.Close()

			var nilProvider *Provider
			nilProvider.Close()
		})

		// The type exists to be shared across goroutines, so a shutdown racing a
		// handler that is still reading it must not be a data race. Run under -race
		// this fails on the unguarded fields it was written for.
		It("Should not race a concurrent reader", func() {
			nc, err := nats.Connect(runBroker())
			Expect(err).ToNot(HaveOccurred())

			p := New(WithOwnedNats(nc))

			var wg sync.WaitGroup
			for range 8 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer GinkgoRecover()

					for range 100 {
						p.Nats()
					}
				}()
			}

			wg.Add(1)
			go func() {
				defer wg.Done()
				defer GinkgoRecover()

				p.Close()
			}()

			wg.Wait()
			Expect(p.Nats()).To(BeNil())
		})
	})
})

var _ = Describe("Options", func() {
	It("Should name the connection for the product and the connection", func() {
		opts := nats.GetDefaultOptions()
		for _, o := range Options(Config{Product: "acme-agent", Name: "worker-3"}) {
			Expect(o(&opts)).To(Succeed())
		}

		Expect(opts.Name).To(Equal("acme-agent worker-3"))
		Expect(opts.MaxReconnect).To(Equal(reconnectAttempts))
	})

	// A caller's options are appended last, so repeating a standard one is how a
	// binding overrides it rather than a conflict it cannot resolve.
	It("Should let a caller's option override a standard one", func() {
		opts := nats.GetDefaultOptions()
		cfg := Config{Product: "acme-agent", Name: "worker-3", Options: []nats.Option{nats.Name("something else")}}
		for _, o := range Options(cfg) {
			Expect(o(&opts)).To(Succeed())
		}

		Expect(opts.Name).To(Equal("something else"))
	})
})

var _ = Describe("Connect", func() {
	It("Should return a Provider that owns the connection it dialed", func() {
		p, err := Connect(context.Background(), runBroker(), Config{Product: "acme-agent", Name: "worker-3"})
		Expect(err).ToNot(HaveOccurred())

		nc := p.Nats()
		Expect(nc).ToNot(BeNil())
		Expect(nc.Status()).To(Equal(nats.CONNECTED))

		p.Close()
		Expect(nc.IsClosed()).To(BeTrue())
	})

	It("Should refuse a context that is already done without dialing", func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		start := time.Now()
		_, err := Connect(ctx, blackhole, Config{Product: "acme-agent", Name: "worker-3"})
		Expect(err).To(MatchError(context.Canceled))
		Expect(time.Since(start)).To(BeNumerically("<", time.Second))
	})

	// The cancel is answered within one nats.Timeout rather than at once, which is why
	// this allows several seconds while still proving the dial does not run forever.
	It("Should give up on an unreachable broker when the caller's context ends", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()

		start := time.Now()
		_, err := Connect(ctx, blackhole, Config{Product: "acme-agent", Name: "worker-3"})
		Expect(err).To(MatchError(context.DeadlineExceeded))
		Expect(err.Error()).To(ContainSubstring(blackhole))
		Expect(time.Since(start)).To(BeNumerically("<", 10*time.Second))
	})

	// A rejected credential is not something retrying fixes. The server closes the
	// connection, and the reason it gives has to survive into the error rather than
	// being reported as an unexplained close.
	It("Should fail with the server's reason when the credentials are refused", func() {
		ns, err := natsd.NewServer(&natsd.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true, Username: "right", Password: "right"})
		Expect(err).ToNot(HaveOccurred())

		go ns.Start()
		Expect(ns.ReadyForConnections(10 * time.Second)).To(BeTrue())
		DeferCleanup(ns.Shutdown)

		cfg := Config{Product: "acme-agent", Name: "worker-3", Options: []nats.Option{nats.UserInfo("wrong", "wrong")}}
		_, err = Connect(context.Background(), ns.ClientURL(), cfg)
		Expect(err).To(MatchError(ErrClosed))
		Expect(err.Error()).To(ContainSubstring("Authorization Violation"))
	})
})

var _ = Describe("ConnectNatsContext", func() {
	It("Should name the context it could not read", func() {
		_, err := ConnectNatsContext(context.Background(), "no-such-context-for-a-test", Config{Product: "acme-agent", Name: "worker-3"})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("no-such-context-for-a-test"))
	})
})
