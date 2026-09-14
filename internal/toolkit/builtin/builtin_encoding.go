//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package builtin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/toolkit"
	"github.com/choria-io/fisk-ai/internal/toolkit/functool"
)

// base64EncodeName is the opt-in built-in that encodes text as base64. It is
// defined in the config package on the same terms as knowledgeSearchName, since an
// operator names it under harness.tools and may name it in the MCP allowlist.
const base64EncodeName = config.Base64EncodeToolName

// base64EncodeSpec builds the base64_encode spec from its harness.tools entry. Every
// opt-in constructor shares this signature so the wiring holds them in one table;
// this one reads nothing from cfg. options is the entry's raw options block, and a
// block with any key is an error, since a key nothing reads would otherwise be
// accepted silently. An absent key, a bare options: (which arrives as null) and
// options: {} all count as none. It returns the spec
// rather than the tool so the wiring applies the confirm wrap and calls mustNew in
// one place for every opt-in tool.
func base64EncodeSpec(_ *config.Config, options json.RawMessage) (functool.Spec, error) {
	if err := refuseOptions(base64EncodeName, options); err != nil {
		return functool.Spec{}, err
	}

	return functool.Spec{
		Name: base64EncodeName,
		// A pure function over its argument, so it is safe to serve. Not a2a, for the
		// reason knowledge_search is not: there is no a2a builtins allowlist, so
		// declaring it there would serve it the moment a2a is enabled.
		Expose: &functool.ExposeSpec{MCP: true},
		// It touches nothing: the same text encodes to the same string every time and
		// no call reaches outside the process.
		Behavior: toolkit.Behavior{
			ReadOnly:   toolkit.HintTrue,
			Idempotent: toolkit.HintTrue,
			OpenWorld:  toolkit.HintFalse,
		},
		Description: "Encode the text you pass as standard base64 and return it. " +
			"It needs no file: pass the text itself. " +
			"It returns {\"encoded\": \"<base64>\"}, the standard alphabet with padding, over the UTF-8 bytes of text.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"text": map[string]any{
					"type":        "string",
					"description": "The text to encode. It may be empty, which encodes to an empty string.",
				},
			},
			"required": []any{"text"},
		},
		Handler:          withPrompter(base64EncodeHandler),
		ValidateRequired: true,
		Trace:            base64EncodeTrace,
	}, nil
}

// refuseOptions is the options check for an opt-in tool that takes none. An empty
// block, null and {} pass; a block with any key fails and names the keys, and a
// block that is not an object fails on the decode.
func refuseOptions(name string, options json.RawMessage) error {
	if len(options) == 0 || string(options) == "null" {
		return nil
	}

	var block map[string]json.RawMessage
	if err := json.Unmarshal(options, &block); err != nil {
		return fmt.Errorf("harness.tools: %s takes no options, and its options block is not an object: %w", name, err)
	}
	if len(block) == 0 {
		return nil
	}

	keys := make([]string, 0, len(block))
	for k := range block {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	return fmt.Errorf("harness.tools: %s takes no options; remove the options block (it sets %s)", name, strings.Join(keys, ", "))
}

// base64EncodeTrace renders the one-line call trace as the tool name and the size
// of the text, since the text itself may be long and is not what the operator needs
// to see.
func base64EncodeTrace(input json.RawMessage) string {
	var args struct {
		Text string `json:"text"`
	}
	if err := decodeArgs(input, &args); err != nil {
		return base64EncodeName
	}

	return fmt.Sprintf("%s (%d bytes)", base64EncodeName, len(args.Text))
}

// base64EncodeOutcome is the JSON result the base64_encode tool returns.
type base64EncodeOutcome struct {
	Encoded string `json:"encoded"`
}

// base64EncodeHandler is the base64_encode handler. The text arrives as a JSON
// string, so its bytes are UTF-8; the handler encodes them unchanged with the
// standard alphabet and padding, the form every decoder accepts.
func base64EncodeHandler(_ context.Context, input json.RawMessage, _ toolkit.Prompter) (string, error) {
	var args struct {
		Text string `json:"text"`
	}
	if err := decodeArgs(input, &args); err != nil {
		return "", fmt.Errorf("invalid %s input: %w", base64EncodeName, err)
	}

	return outcomeJSON(base64EncodeName, base64EncodeOutcome{Encoded: base64.StdEncoding.EncodeToString([]byte(args.Text))})
}
