//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package mcpclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
)

var _ = Describe("Sessions", func() {
	var (
		ctx     context.Context
		cancel  context.CancelFunc
		servers *fakeServers
	)

	BeforeEach(func() {
		ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
		DeferCleanup(cancel)

		servers = newFakeServers()
	})

	connected := func(entries ...config.MCPServer) *Sessions {
		GinkgoHelper()

		sessions, err := Connect(ctx, Options{
			Servers:  entries,
			Identity: "fisk-test",
			Version:  "0.0.1",
			Dialer:   servers.dialer(),
		})
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(func() { Expect(sessions.Close()).To(Succeed()) })

		return sessions
	}

	Describe("Connect", func() {
		It("should connect every configured server and reach each by name", func() {
			sessions := connected(
				config.MCPServer{Name: "docs", Command: "unused"},
				config.MCPServer{Name: "issues", Command: "unused"},
			)

			Expect(sessions.Names()).To(Equal([]string{"docs", "issues"}))

			for _, name := range sessions.Names() {
				var served string
				var tools []string

				err := sessions.Use(ctx, name, func(session *mcp.ClientSession) error {
					served = session.InitializeResult().ServerInfo.Name

					res, err := session.ListTools(ctx, nil)
					if err != nil {
						return err
					}
					for _, tool := range res.Tools {
						tools = append(tools, tool.Name)
					}

					return nil
				})
				Expect(err).ToNot(HaveOccurred())
				Expect(served).To(Equal(name))
				Expect(tools).To(Equal([]string{"search"}))
			}
		})

		It("should identify this process to the server", func() {
			sessions := connected(config.MCPServer{Name: "docs", Command: "unused"})
			Expect(sessions.Names()).To(HaveLen(1))

			Expect(servers.served()).To(HaveLen(1))
			params := servers.served()[0].InitializeParams()
			Expect(params.ClientInfo.Name).To(Equal("fisk-test"))
			Expect(params.ClientInfo.Version).To(Equal("0.0.1"))
		})

		It("should error on a name that is configured twice", func() {
			_, err := Connect(ctx, Options{
				Servers: []config.MCPServer{{Name: "docs", Command: "unused"}, {Name: "docs", URL: "https://example.net/mcp"}},
				Dialer:  servers.dialer(),
			})
			Expect(err).To(MatchError(ContainSubstring(`mcp server "docs" is configured more than once`)))
		})

		It("should close the sessions it opened when a later server fails", func() {
			servers.fail["issues"] = errors.New("no such host")

			_, err := Connect(ctx, Options{
				Servers: []config.MCPServer{{Name: "docs", Command: "unused"}, {Name: "issues", URL: "https://example.net/mcp"}},
				Dialer:  servers.dialer(),
			})
			Expect(err).To(MatchError(ContainSubstring("no such host")))

			Expect(servers.served()).To(HaveLen(1))
			Eventually(ended(servers.served()[0])).Should(BeClosed())
		})

		It("should resolve a ${VAR} reference and name one that is not set", func() {
			_, err := Connect(ctx, Options{
				Servers: []config.MCPServer{{
					Name:    "docs",
					Command: "does-not-run",
					Env:     map[string]string{"DOCS_TOKEN": "prefix-${FISK_MCPCLIENT_ABSENT}"},
				}},
			})
			Expect(err).To(MatchError(ContainSubstring(`mcp server "docs": env "DOCS_TOKEN": environment variable "FISK_MCPCLIENT_ABSENT" is not set`)))
		})

		It("should bound the connect with the entry's timeout", func() {
			deaf := &deafTransport{closed: make(chan struct{})}
			DeferCleanup(deaf.stop)

			started := time.Now()
			_, err := Connect(ctx, Options{
				Servers: []config.MCPServer{{Name: "docs", Command: "unused", TimeoutParsed: 150 * time.Millisecond}},
				Dialer: func(context.Context, config.MCPServer) (mcp.Transport, error) {
					return deaf, nil
				},
			})
			Expect(err).To(MatchError(context.DeadlineExceeded))
			Expect(err).To(MatchError(ContainSubstring(`connecting to mcp server "docs"`)))
			Expect(err).To(MatchError(ContainSubstring("150ms")))
			Expect(time.Since(started)).To(BeNumerically("<", 5*time.Second))
		})

		It("should pass the connect timeout to the dialer", func() {
			var deadline time.Time
			var ok bool

			_, err := Connect(ctx, Options{
				Servers: []config.MCPServer{{Name: "docs", Command: "unused", TimeoutParsed: 250 * time.Millisecond}},
				Dialer: func(dialCtx context.Context, _ config.MCPServer) (mcp.Transport, error) {
					deadline, ok = dialCtx.Deadline()
					return nil, errors.New("not dialed")
				},
			})
			Expect(err).To(MatchError(ContainSubstring("not dialed")))
			Expect(ok).To(BeTrue())
			Expect(deadline).To(BeTemporally("~", time.Now().Add(250*time.Millisecond), 250*time.Millisecond))
		})
	})

	Describe("Use", func() {
		It("should error on a server that is not configured", func() {
			sessions := connected(config.MCPServer{Name: "docs", Command: "unused"})

			called := false
			err := sessions.Use(ctx, "issues", func(*mcp.ClientSession) error {
				called = true
				return nil
			})
			Expect(err).To(MatchError(`no mcp server named "issues" is configured`))
			Expect(called).To(BeFalse())
		})

		It("should return what the caller's function returns", func() {
			sessions := connected(config.MCPServer{Name: "docs", Command: "unused"})

			err := sessions.Use(ctx, "docs", func(*mcp.ClientSession) error {
				return errors.New("the tool call failed")
			})
			Expect(err).To(MatchError("the tool call failed"))
		})

		It("should replace a session that has ended", func() {
			sessions := connected(config.MCPServer{Name: "docs", Command: "unused"})

			Expect(servers.served()).To(HaveLen(1))
			Expect(servers.served()[0].Close()).To(Succeed())

			// The client learns the session ended asynchronously, so wait for the
			// replacement rather than for the first call to fail.
			Eventually(func() int {
				_ = sessions.Use(ctx, "docs", func(*mcp.ClientSession) error { return nil })

				servers.mu.Lock()
				defer servers.mu.Unlock()

				return servers.dials["docs"]
			}).Should(Equal(2))

			var tools int
			err := sessions.Use(ctx, "docs", func(session *mcp.ClientSession) error {
				res, err := session.ListTools(ctx, nil)
				if err != nil {
					return err
				}
				tools = len(res.Tools)

				return nil
			})
			Expect(err).ToNot(HaveOccurred())
			Expect(tools).To(Equal(1))
		})

		It("should be safe for concurrent use", func() {
			sessions := connected(
				config.MCPServer{Name: "docs", Command: "unused"},
				config.MCPServer{Name: "issues", Command: "unused"},
			)

			var wg sync.WaitGroup
			errs := make(chan error, 40)

			for i := 0; i < 20; i++ {
				for _, name := range sessions.Names() {
					wg.Add(1)
					go func(name string) {
						defer wg.Done()
						defer GinkgoRecover()

						errs <- sessions.Use(ctx, name, func(session *mcp.ClientSession) error {
							_, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "search"})
							return err
						})
					}(name)
				}
			}

			wg.Wait()
			close(errs)

			for err := range errs {
				Expect(err).ToNot(HaveOccurred())
			}
		})

		It("should refuse rather than hand fn a nil session when Close lands mid-call", func() {
			// live reads the closed flag under the Sessions lock and the session under
			// the entry lock. A Close arriving between the two clears the session before
			// the channel that reports the session ended is closed, so a caller was
			// handed a nil session. Each round releases the callers and one Close from
			// the same barrier, and each caller calls until it is told the sessions are
			// closed, so Close reaches the entry lock with callers queued behind it.
			const (
				rounds  = 50
				callers = 8
				calls   = 500
			)

			handedNil := errors.New("fn was handed a nil session")

			for round := 0; round < rounds; round++ {
				sessions, err := Connect(ctx, Options{
					Servers:  []config.MCPServer{{Name: "docs", Command: "unused"}},
					Identity: "fisk-test",
					Dialer:   servers.dialer(),
				})
				Expect(err).ToNot(HaveOccurred())

				start := make(chan struct{})
				errs := make(chan error, callers)

				var wg sync.WaitGroup

				for i := 0; i < callers; i++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						defer GinkgoRecover()

						<-start

						// The call cap only stops the loop if Close never lands, which would
						// leave the round proving nothing rather than hanging.
						for call := 0; call < calls; call++ {
							err := sessions.Use(ctx, "docs", func(session *mcp.ClientSession) error {
								if session == nil {
									return handedNil
								}

								return nil
							})
							if err != nil {
								errs <- err
								return
							}
						}
					}()
				}

				wg.Add(1)
				go func() {
					defer wg.Done()
					defer GinkgoRecover()

					<-start

					Expect(sessions.Close()).To(Succeed())
				}()

				close(start)
				wg.Wait()
				close(errs)

				// A caller that raced Close gets the same error it would get after Close
				// had finished, and never the sentinel a nil session returns.
				for err := range errs {
					Expect(err).To(MatchError(ContainSubstring("are closed")))
				}
			}
		})
	})

	Describe("Close", func() {
		It("should close every session", func() {
			sessions, err := Connect(ctx, Options{
				Servers: []config.MCPServer{{Name: "docs", Command: "unused"}, {Name: "issues", Command: "unused"}},
				Dialer:  servers.dialer(),
			})
			Expect(err).ToNot(HaveOccurred())

			served := servers.served()
			Expect(served).To(HaveLen(2))

			Expect(sessions.Close()).To(Succeed())

			for _, session := range served {
				Eventually(ended(session)).Should(BeClosed())
			}
		})

		It("should be idempotent and refuse use afterwards", func() {
			sessions, err := Connect(ctx, Options{
				Servers: []config.MCPServer{{Name: "docs", Command: "unused"}},
				Dialer:  servers.dialer(),
			})
			Expect(err).ToNot(HaveOccurred())

			Expect(sessions.Close()).To(Succeed())
			Expect(sessions.Close()).To(Succeed())

			err = sessions.Use(ctx, "docs", func(*mcp.ClientSession) error { return nil })
			Expect(err).To(MatchError(ContainSubstring("are closed")))
		})
	})

	Describe("Capabilities", func() {
		// The wire, not the SDK types, is what these read: mcp.ClientCapabilities
		// marshals on its own to {"roots":{}}, because its deprecated Roots field is a
		// non-pointer struct that encoding/json will not omit, and that is not what a
		// server receives. Both handshakes are covered because the SDK picks between
		// them by what the server answers.
		It("should advertise nothing on the stateless discover handshake", func() {
			clientSide, serverSide := mcp.NewInMemoryTransports()

			srv := mcp.NewServer(&mcp.Implementation{Name: "docs", Version: "1"}, nil)
			_, err := srv.Connect(context.Background(), serverSide, nil)
			Expect(err).ToNot(HaveOccurred())

			recorder := &recordingTransport{inner: clientSide}
			sessions, err := Connect(ctx, Options{
				Servers:  []config.MCPServer{{Name: "docs", Command: "unused"}},
				Identity: "fisk-test",
				Dialer: func(context.Context, config.MCPServer) (mcp.Transport, error) {
					return recorder, nil
				},
			})
			Expect(err).ToNot(HaveOccurred())
			DeferCleanup(func() { Expect(sessions.Close()).To(Succeed()) })

			written := recorder.written()
			Expect(written).ToNot(BeEmpty())
			Expect(written[0]).To(ContainSubstring(`"method":"server/discover"`))
			Expect(written[0]).To(ContainSubstring(`"io.modelcontextprotocol/clientCapabilities":{}`))
			Expect(written[0]).ToNot(ContainSubstring("roots"))
		})

		It("should advertise nothing on the initialize handshake", func() {
			// A server that refuses server/discover, so the client falls back to the
			// initialize handshake, hand written because the SDK's own server answers
			// discover and would never take this path.
			clientReads, serverWrites := io.Pipe()
			serverReads, clientWrites := io.Pipe()

			initialize := make(chan string, 1)

			go func() {
				defer GinkgoRecover()

				lines := bufio.NewScanner(serverReads)
				for lines.Scan() {
					var msg struct {
						ID     json.RawMessage `json:"id"`
						Method string          `json:"method"`
					}
					if json.Unmarshal(lines.Bytes(), &msg) != nil {
						continue
					}

					switch msg.Method {
					case "server/discover":
						fmt.Fprintf(serverWrites, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"no discover here"}}`+"\n", msg.ID)
					case "initialize":
						initialize <- lines.Text()
						fmt.Fprintf(serverWrites, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-06-18","capabilities":{},"serverInfo":{"name":"docs","version":"1"}}}`+"\n", msg.ID)
					}
				}
			}()

			sessions, err := Connect(ctx, Options{
				Servers:  []config.MCPServer{{Name: "docs", Command: "unused"}},
				Identity: "fisk-test",
				Dialer: func(context.Context, config.MCPServer) (mcp.Transport, error) {
					return &mcp.IOTransport{Reader: clientReads, Writer: clientWrites}, nil
				},
			})
			Expect(err).ToNot(HaveOccurred())
			DeferCleanup(func() { Expect(sessions.Close()).To(Succeed()) })

			var params string
			Eventually(initialize).Should(Receive(&params))
			Expect(params).To(ContainSubstring(`"capabilities":{}`))
			Expect(params).ToNot(ContainSubstring("roots"))
			Expect(params).To(ContainSubstring(`"name":"fisk-test"`))
		})
	})
})

// deafTransport is a transport whose connection accepts everything written to it
// and never answers, so a connect runs out of time rather than failing.
type deafTransport struct {
	closed chan struct{}
	once   sync.Once
}

func (t *deafTransport) Connect(context.Context) (mcp.Connection, error) {
	return t, nil
}

func (t *deafTransport) Read(ctx context.Context) (jsonrpc.Message, error) {
	select {
	case <-t.closed:
		return nil, io.EOF
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (t *deafTransport) Write(context.Context, jsonrpc.Message) error { return nil }
func (t *deafTransport) SessionID() string                            { return "" }

func (t *deafTransport) Close() error {
	t.stop()
	return nil
}

func (t *deafTransport) stop() {
	t.once.Do(func() { close(t.closed) })
}
