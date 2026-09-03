//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
)

var _ = Describe("Transports", func() {
	var (
		ctx    context.Context
		cancel context.CancelFunc
	)

	BeforeEach(func() {
		ctx, cancel = context.WithTimeout(context.Background(), 60*time.Second)
		DeferCleanup(cancel)
	})

	Describe("childEnv", func() {
		lookup := func(name string) (string, bool) {
			if name == "FISK_MCPCLIENT_TOKEN" {
				return "from-the-process", true
			}

			return "", false
		}

		It("should inherit the parent environment", func() {
			setenv("FISK_MCPCLIENT_INHERITED", "yes")

			env, err := childEnv(config.MCPServer{Name: "docs"}, nil, lookup)
			Expect(err).ToNot(HaveOccurred())
			Expect(env).To(ContainElement("FISK_MCPCLIENT_INHERITED=yes"))
			Expect(env).To(ContainElement(HavePrefix("PATH=")))
		})

		It("should remove the provider and operator credential variables", func() {
			setenv(fakeCredEnvVar, "provider-secret")
			setenv("FISK_MCPCLIENT_OPERATOR_SECRET", "operator-secret")

			env, err := childEnv(config.MCPServer{Name: "docs"}, []string{"FISK_MCPCLIENT_OPERATOR_SECRET"}, lookup)
			Expect(err).ToNot(HaveOccurred())
			Expect(env).ToNot(ContainElement(HavePrefix(fakeCredEnvVar + "=")))
			Expect(env).ToNot(ContainElement(HavePrefix("FISK_MCPCLIENT_OPERATOR_SECRET=")))
		})

		It("should apply the entry's env on top, resolving its references", func() {
			setenv("FISK_MCPCLIENT_INHERITED", "from-the-parent")

			env, err := childEnv(config.MCPServer{
				Name: "docs",
				Env: map[string]string{
					"DOCS_TOKEN":               "Bearer ${FISK_MCPCLIENT_TOKEN}",
					"FISK_MCPCLIENT_INHERITED": "from-the-entry",
				},
			}, nil, lookup)
			Expect(err).ToNot(HaveOccurred())

			// os/exec keeps the last value of a repeated name, so the entry's value is
			// the one the child sees.
			Expect(env).To(ContainElement("DOCS_TOKEN=Bearer from-the-process"))
			Expect(env[len(env)-1]).To(Equal("FISK_MCPCLIENT_INHERITED=from-the-entry"))
		})

		It("should name the variable and the server a value references but is not set", func() {
			_, err := childEnv(config.MCPServer{
				Name: "docs",
				Env:  map[string]string{"DOCS_TOKEN": "${FISK_MCPCLIENT_ABSENT}"},
			}, nil, lookup)
			Expect(err).To(MatchError(`mcp server "docs": env "DOCS_TOKEN": environment variable "FISK_MCPCLIENT_ABSENT" is not set`))
		})
	})

	Describe("The HTTP transport", func() {
		It("should send the resolved headers to the configured host", func() {
			received := make(chan http.Header, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received <- r.Header.Clone()
				w.WriteHeader(http.StatusAccepted)
			}))
			DeferCleanup(srv.Close)

			transport, err := httpTransport(config.MCPServer{
				Name:    "docs",
				URL:     srv.URL + "/mcp",
				Headers: map[string]string{"Authorization": "Bearer ${FISK_MCPCLIENT_TOKEN}"},
			}, func(string) (string, bool) { return "secret-token", true })
			Expect(err).ToNot(HaveOccurred())

			streamable, ok := transport.(*mcp.StreamableClientTransport)
			Expect(ok).To(BeTrue())

			res, err := streamable.HTTPClient.Get(srv.URL + "/mcp")
			Expect(err).ToNot(HaveOccurred())
			Expect(res.Body.Close()).To(Succeed())

			var headers http.Header
			Eventually(received).Should(Receive(&headers))
			Expect(headers.Get("Authorization")).To(Equal("Bearer secret-token"))
		})

		It("should drop the headers when a redirect leaves the configured host", func() {
			received := make(chan http.Header, 1)
			elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received <- r.Header.Clone()
				w.WriteHeader(http.StatusAccepted)
			}))
			DeferCleanup(elsewhere.Close)

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, elsewhere.URL+"/mcp", http.StatusTemporaryRedirect)
			}))
			DeferCleanup(srv.Close)

			transport, err := httpTransport(config.MCPServer{
				Name:    "docs",
				URL:     srv.URL + "/mcp",
				Headers: map[string]string{"Authorization": "Bearer ${FISK_MCPCLIENT_TOKEN}"},
			}, func(string) (string, bool) { return "secret-token", true })
			Expect(err).ToNot(HaveOccurred())

			streamable, ok := transport.(*mcp.StreamableClientTransport)
			Expect(ok).To(BeTrue())

			res, err := streamable.HTTPClient.Get(srv.URL + "/mcp")
			Expect(err).ToNot(HaveOccurred())
			Expect(res.Body.Close()).To(Succeed())

			var headers http.Header
			Eventually(received).Should(Receive(&headers))
			Expect(headers.Get("Authorization")).To(BeEmpty())
		})

		It("should resolve the references in the url", func() {
			transport, err := httpTransport(config.MCPServer{
				Name: "docs",
				URL:  "https://mcp.example.net/mcp/?apiKey=${FISK_MCPCLIENT_TOKEN}",
			}, func(string) (string, bool) { return "secret-token", true })
			Expect(err).ToNot(HaveOccurred())

			streamable, ok := transport.(*mcp.StreamableClientTransport)
			Expect(ok).To(BeTrue())
			Expect(streamable.Endpoint).To(Equal("https://mcp.example.net/mcp/?apiKey=secret-token"))
		})

		It("should resolve a reference mixed with literal text in the url", func() {
			transport, err := httpTransport(config.MCPServer{
				Name: "docs",
				URL:  "https://${FISK_MCPCLIENT_TOKEN}.example.net/mcp/?apiKey=prefix-${FISK_MCPCLIENT_TOKEN}",
			}, func(string) (string, bool) { return "docs", true })
			Expect(err).ToNot(HaveOccurred())

			streamable, ok := transport.(*mcp.StreamableClientTransport)
			Expect(ok).To(BeTrue())
			Expect(streamable.Endpoint).To(Equal("https://docs.example.net/mcp/?apiKey=prefix-docs"))
		})

		It("should name the variable and the server a url references but is not set", func() {
			_, err := httpTransport(config.MCPServer{
				Name: "docs",
				URL:  "https://mcp.example.net/mcp/?apiKey=${FISK_MCPCLIENT_ABSENT}",
			}, func(string) (string, bool) { return "", false })
			Expect(err).To(MatchError(`mcp server "docs": url: environment variable "FISK_MCPCLIENT_ABSENT" is not set`))
		})

		// A url whose scheme or host comes from a variable is not checked when the file is
		// parsed, since the configured text is not what gets dialed.
		It("should refuse a url that expands to an endpoint it cannot reach", func() {
			_, err := httpTransport(config.MCPServer{
				Name: "docs",
				URL:  "${FISK_MCPCLIENT_ENDPOINT}",
			}, func(string) (string, bool) { return "localhost:9000", true })
			Expect(err).To(MatchError(ContainSubstring(`mcp server "docs" has an unusable url "${FISK_MCPCLIENT_ENDPOINT}"`)))
			Expect(err).To(MatchError(ContainSubstring("http:// or https:// endpoint")))
		})

		It("should quote the configured url rather than the expanded one", func() {
			_, err := httpTransport(config.MCPServer{
				Name: "docs",
				URL:  "ftp://mcp.example.net/mcp/?apiKey=${FISK_MCPCLIENT_TOKEN}",
			}, func(string) (string, bool) { return "secret-token", true })
			Expect(err).To(MatchError(ContainSubstring(`has an unusable url "ftp://mcp.example.net/mcp/?apiKey=${FISK_MCPCLIENT_TOKEN}"`)))
			Expect(err.Error()).ToNot(ContainSubstring("secret-token"))
		})

		It("should name the variable and the server a header references but is not set", func() {
			_, err := httpTransport(config.MCPServer{
				Name:    "docs",
				URL:     "https://example.net/mcp",
				Headers: map[string]string{"Authorization": "Bearer ${FISK_MCPCLIENT_ABSENT}"},
			}, func(string) (string, bool) { return "", false })
			Expect(err).To(MatchError(`mcp server "docs": headers "Authorization": environment variable "FISK_MCPCLIENT_ABSENT" is not set`))
		})
	})

	// A stdio child runs in the root, which is what makes a command written with a
	// separator resolve against the root rather than against the process working
	// directory. An HTTP server is somebody else's process and is unaffected.
	Describe("The stdio child's working directory", func() {
		server := config.MCPServer{Name: "docs", Command: "docs-server"}

		It("starts the child in the directory it was given", func() {
			tr, err := commandTransport(server, "/srv/agent", nil, os.LookupEnv)
			Expect(err).ToNot(HaveOccurred())

			cmd, ok := tr.(*mcp.CommandTransport)
			Expect(ok).To(BeTrue())
			Expect(cmd.Command.Dir).To(Equal("/srv/agent"))
		})

		It("leaves the child in the process working directory when it is given none", func() {
			tr, err := commandTransport(server, "", nil, os.LookupEnv)
			Expect(err).ToNot(HaveOccurred())

			cmd, ok := tr.(*mcp.CommandTransport)
			Expect(ok).To(BeTrue())
			Expect(cmd.Command.Dir).To(BeEmpty())
		})

		It("takes the directory from the session options", func() {
			s := &Sessions{opts: Options{WorkDir: "/srv/agent", LookupEnv: os.LookupEnv}}

			tr, err := s.transport(ctx, server)
			Expect(err).ToNot(HaveOccurred())

			cmd, ok := tr.(*mcp.CommandTransport)
			Expect(ok).To(BeTrue())
			Expect(cmd.Command.Dir).To(Equal("/srv/agent"))
		})
	})

	Describe("The stdio transport", func() {
		var binary string

		BeforeEach(func() {
			binary = filepath.Join(GinkgoT().TempDir(), "stdioserver")
			if runtime.GOOS == "windows" {
				binary += ".exe"
			}

			build := exec.CommandContext(ctx, "go", "build", "-o", binary, "./testdata/stdioserver")
			out, err := build.CombinedOutput()
			Expect(err).ToNot(HaveOccurred(), fmt.Sprintf("building the fixture server: %s", out))
		})

		It("should start the child with the environment a command tool gets", func() {
			setenv(fakeCredEnvVar, "provider-secret")
			setenv("FISK_MCPCLIENT_OPERATOR_SECRET", "operator-secret")
			setenv("FISK_MCPCLIENT_TOKEN", "from-the-process")

			sessions, err := Connect(ctx, Options{
				Servers: []config.MCPServer{{
					Name:    "docs",
					Command: binary,
					Env:     map[string]string{"DOCS_TOKEN": "Bearer ${FISK_MCPCLIENT_TOKEN}"},
				}},
				Identity:           "fisk-test",
				Version:            "0.0.1",
				CredentialEnvNames: []string{"FISK_MCPCLIENT_OPERATOR_SECRET"},
			})
			Expect(err).ToNot(HaveOccurred())
			DeferCleanup(func() { Expect(sessions.Close(ctx)).To(Succeed()) })

			env := map[string]string{}
			err = sessions.Use(ctx, "docs", func(session *mcp.ClientSession) error {
				Expect(session.InitializeResult().ServerInfo.Name).To(Equal("fisk-stdio-fixture"))

				res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "environment"})
				if err != nil {
					return err
				}
				Expect(res.IsError).To(BeFalse())
				Expect(res.Content).To(HaveLen(1))

				text, ok := res.Content[0].(*mcp.TextContent)
				Expect(ok).To(BeTrue())

				var reported []string
				Expect(json.Unmarshal([]byte(text.Text), &reported)).To(Succeed())
				for _, kv := range reported {
					name, value, _ := strings.Cut(kv, "=")
					env[name] = value
				}

				return nil
			})
			Expect(err).ToNot(HaveOccurred())

			Expect(env).To(HaveKeyWithValue("DOCS_TOKEN", "Bearer from-the-process"))
			Expect(env).To(HaveKeyWithValue("FISK_MCPCLIENT_TOKEN", "from-the-process"))
			Expect(env["PATH"]).ToNot(BeEmpty())
			Expect(env).ToNot(HaveKey(fakeCredEnvVar))
			Expect(env).ToNot(HaveKey("FISK_MCPCLIENT_OPERATOR_SECRET"))
		})

		It("should end the child when the sessions are closed", func() {
			marker := filepath.Join(GinkgoT().TempDir(), "exited")

			sessions, err := Connect(ctx, Options{
				Servers: []config.MCPServer{{
					Name:    "docs",
					Command: binary,
					Env:     map[string]string{"FISK_MCPCLIENT_EXIT_MARKER": marker},
				}},
				Identity: "fisk-test",
			})
			Expect(err).ToNot(HaveOccurred())

			var tools []string
			err = sessions.Use(ctx, "docs", func(session *mcp.ClientSession) error {
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
			Expect(tools).To(Equal([]string{"environment"}))

			Expect(sessions.Close(ctx)).To(Succeed())

			Eventually(func() error {
				_, err := os.Stat(marker)
				return err
			}, 10*time.Second).Should(Succeed())
		})
	})
})
