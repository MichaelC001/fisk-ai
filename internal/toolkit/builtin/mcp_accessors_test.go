//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package builtin

import (
	"context"
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/toolkit"
)

var _ = Describe("BuiltinTool MCP accessors", func() {
	ctx := context.Background()
	cfg := &config.Config{Harness: config.HarnessConfig{RAG: &config.RAGConfig{Enabled: true}}}

	It("exposes the input schema and dispatches the handler in-process", func() {
		ks := ragToolNamed(RAGTools(cfg, nil), "knowledge_search")

		props, ok := ks.InputSchema()["properties"].(map[string]any)
		Expect(ok).To(BeTrue())
		Expect(props).To(HaveKey("query"))

		// A nil store makes the handler return an invocation error without touching
		// the network; the deny prompter is inert here since the handler ignores it.
		_, err := callTool(ks, ctx, json.RawMessage(`{"query":"x"}`), toolkit.DefaultDenyPrompter())
		Expect(err).To(HaveOccurred())
	})
})
