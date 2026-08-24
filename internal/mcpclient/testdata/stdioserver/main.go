//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// Command stdioserver is a minimal MCP server spoken to over stdio, built and
// started by the mcpclient tests so the stdio transport is exercised against a
// real child process. Its one tool reports the environment it was started with,
// and it writes the file named by FISK_MCPCLIENT_EXIT_MARKER as it exits, so a
// test can see that closing the session ended the child.
package main

import (
	"context"
	"encoding/json"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	srv := mcp.NewServer(&mcp.Implementation{Name: "fisk-stdio-fixture", Version: "1.2.3"}, nil)

	srv.AddTool(&mcp.Tool{
		Name:        "environment",
		Description: "Reports the environment variables the server was started with",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		body, err := json.Marshal(os.Environ())
		if err != nil {
			return nil, err
		}

		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(body)}}}, nil
	})

	// The run ends when the client closes its stdin, which is a normal shutdown,
	// so it exits zero either way: an exit status the client reports as a failure
	// to close would say nothing about the code under test.
	_ = srv.Run(context.Background(), &mcp.StdioTransport{})

	marker := os.Getenv("FISK_MCPCLIENT_EXIT_MARKER")
	if marker != "" {
		_ = os.WriteFile(marker, []byte("exited"), 0o600)
	}
}
