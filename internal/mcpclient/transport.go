//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package mcpclient

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/llm"
)

// maxRedirects is how many redirects an HTTP request to a server may follow.
// Overriding CheckRedirect replaces the limit net/http applies by default, so
// the same limit is applied here.
const maxRedirects = 10

// transport builds the transport for one server: the caller's Dialer when it set
// one, otherwise stdio for a server with a command and streamable HTTP for one
// with a url.
//
// Streamable HTTP is the only HTTP transport offered. It carries both the
// initialize handshake and the stateless lifecycle of protocol version
// 2026-07-28, because the SDK negotiates between them inside this one transport,
// and the legacy SSE transport is not offered at all.
func (s *Sessions) transport(ctx context.Context, server config.MCPServer) (mcp.Transport, error) {
	if s.opts.Dialer != nil {
		return s.opts.Dialer(ctx, server)
	}

	switch {
	case server.URL != "":
		return httpTransport(server, s.opts.LookupEnv)
	case server.Command != "":
		return commandTransport(server, s.opts.CredentialEnvNames, s.opts.LookupEnv)
	default:
		return nil, fmt.Errorf("mcp server %q sets neither command nor url", server.Name)
	}
}

// commandTransport builds the stdio transport for a server started as a child
// process, with the environment childEnv gives it.
func commandTransport(server config.MCPServer, credentials []string, lookup func(string) (string, bool)) (mcp.Transport, error) {
	env, err := childEnv(server, credentials, lookup)
	if err != nil {
		return nil, err
	}

	// The child belongs to the session, not to the connect, so it is not built with
	// exec.CommandContext: the context here carries the connect timeout and would
	// kill a healthy server the moment that deadline passed. Closing the session is
	// what stops the child, through the SDK's own teardown, which closes its stdin
	// and gives it a terminate window before signaling it.
	cmd := exec.Command(server.Command, server.Args...)
	cmd.Env = env

	return &mcp.CommandTransport{Command: cmd}, nil
}

// childEnv builds the environment of a stdio child: the current environment with
// the credential variables removed and the entry's own env applied on top, which
// is the environment a command tool gets from internal/toolkit/fisk.
//
// The stripped set is the union of the variables every llm provider linked into
// this build declared as secret-bearing and the operator-named credentials in
// credentials, so a server of someone else's choosing cannot read a named secret
// out of the environment it inherits. The guarantee is name-based: it cannot
// catch the same secret exported a second time under a name nobody declared.
//
// The rest of the environment is inherited rather than replaced by the entry's
// env, which was the first shape this took and is wrong twice over: it is not
// what a command tool gets, and a child with no PATH or HOME cannot run the
// npx-shaped and uvx-shaped servers that are most of what an operator wires up.
// The entry's values are appended last, and os/exec keeps the last value of a
// repeated name, so an entry setting a variable the parent also has wins.
func childEnv(server config.MCPServer, credentials []string, lookup func(string) (string, bool)) ([]string, error) {
	resolved, err := resolveValues(server.Name, "env", server.Env, lookup)
	if err != nil {
		return nil, err
	}

	provider := llm.CredentialEnvNames()
	current := os.Environ()
	out := make([]string, 0, len(current)+len(resolved))

	for _, kv := range current {
		name, _, found := strings.Cut(kv, "=")
		if !found {
			out = append(out, kv)
			continue
		}
		if name != "" && (slices.Contains(provider, name) || slices.Contains(credentials, name)) {
			continue
		}
		out = append(out, kv)
	}

	for _, name := range slices.Sorted(maps.Keys(resolved)) {
		out = append(out, name+"="+resolved[name])
	}

	return out, nil
}

// httpTransport builds the streamable HTTP transport for a server reached at a
// url, with an http.Client that carries the entry's resolved headers.
func httpTransport(server config.MCPServer, lookup func(string) (string, bool)) (mcp.Transport, error) {
	endpoint, err := url.Parse(server.URL)
	if err != nil {
		return nil, fmt.Errorf("mcp server %q has an unusable url %q: %w", server.Name, server.URL, err)
	}

	headers, err := resolveValues(server.Name, "headers", server.Headers, lookup)
	if err != nil {
		return nil, err
	}

	return &mcp.StreamableClientTransport{
		Endpoint:   server.URL,
		HTTPClient: newHTTPClient(endpoint.Host, headers),
	}, nil
}

// newHTTPClient builds the client the streamable HTTP transport makes its
// requests with: it adds the configured headers to a request for host, and to no
// other host.
//
// A redirect is where an unguarded header leaks. The SDK sets no CheckRedirect,
// so a server answering a cross-host 307 would be followed to a host of its
// choosing carrying whatever the operator configured, which for a header named
// Authorization is the token itself. The header names are therefore dropped from
// a redirected request that leaves host, and headerTransport, which runs once per
// hop and would otherwise put them back, adds them only for host.
func newHTTPClient(host string, headers map[string]string) *http.Client {
	return &http.Client{
		Transport: &headerTransport{host: host, headers: headers},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}
			if strings.EqualFold(req.URL.Host, host) {
				return nil
			}

			for name := range headers {
				req.Header.Del(name)
			}

			return nil
		},
	}
}

// headerTransport adds the configured headers to every request to host. Base is
// the round tripper it sends on, and nil sends on http.DefaultTransport.
type headerTransport struct {
	host    string
	headers map[string]string
	base    http.RoundTripper
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}

	if len(t.headers) == 0 || !strings.EqualFold(req.URL.Host, t.host) {
		return base.RoundTrip(req)
	}

	// A round tripper may not modify the request it is given.
	out := req.Clone(req.Context())
	for name, value := range t.headers {
		out.Header.Set(name, value)
	}

	return base.RoundTrip(out)
}

// resolveValues resolves the "${VAR}" references in one entry's env or headers
// against lookup, and returns an error naming the server, the key and the
// variable when a value references one lookup does not have. key names the map in
// that error, so an operator is told which of the two to fix.
//
// This is where a reference is resolved: parsing a config checks the syntax and
// reads no variable, so a command that inspects a configuration it cannot run is
// not refused a file over a credential it never uses.
func resolveValues(server string, key string, values map[string]string, lookup func(string) (string, bool)) (map[string]string, error) {
	if len(values) == 0 {
		return nil, nil
	}

	out := make(map[string]string, len(values))
	for _, name := range slices.Sorted(maps.Keys(values)) {
		resolved, err := config.ExpandEnvReferences(values[name], lookup)
		if err != nil {
			return nil, fmt.Errorf("mcp server %q: %s %q: %w", server, key, name, err)
		}
		out[name] = resolved
	}

	return out, nil
}
