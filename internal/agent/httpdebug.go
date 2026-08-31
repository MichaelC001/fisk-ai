//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/choria-io/fisk-ai/internal/llm"
)

// dumpJSONBody writes raw to out, pretty-printed if it is JSON.
func dumpJSONBody(out io.Writer, raw []byte) {
	var pretty bytes.Buffer
	if json.Indent(&pretty, raw, "", "  ") == nil {
		fmt.Fprintf(out, "%s\n", pretty.String())
		return
	}
	fmt.Fprintf(out, "%s\n", raw)
}

// HttpDebugMiddleware returns middleware that dumps the Anthropic API request and
// response bodies to out. Bodies are read non-destructively: the request via
// GetBody (the SDK sets it on retryable requests) and the response by buffering and
// replacing it, so the SDK still parses them normally. JSON bodies are pretty-printed.
// The sink is injected so a caller can direct the dump to a file rather than stderr,
// letting debugging coexist with the full-screen UI whose alt-screen stderr would
// otherwise be corrupted.
func HttpDebugMiddleware(out io.Writer) llm.Middleware {
	return func(req *http.Request, next llm.MiddlewareNext) (*http.Response, error) {
		fmt.Fprintf(out, "\n=== HTTP request: %s %s ===\n", req.Method, req.URL)
		if req.GetBody != nil {
			body, err := req.GetBody()
			if err == nil {
				raw, _ := io.ReadAll(body)
				body.Close()
				dumpJSONBody(out, raw)
			}
		}

		resp, err := next(req)
		if err != nil {
			return resp, err
		}

		raw, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return resp, fmt.Errorf("reading response body for debug: %w", err)
		}
		resp.Body = io.NopCloser(bytes.NewReader(raw))

		fmt.Fprintf(out, "\n=== HTTP response: %s ===\n", resp.Status)
		dumpJSONBody(out, raw)

		return resp, nil
	}
}
