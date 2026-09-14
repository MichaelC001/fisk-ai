//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package builtin

import (
	"context"
	"encoding/base64"
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/internal/toolkit"
	"github.com/choria-io/fisk-ai/internal/toolkit/functool"
)

var _ = Describe("base64_encode tool", func() {
	ctx := context.Background()

	tool := func() *functool.Tool {
		GinkgoHelper()

		spec, err := base64EncodeSpec(nil, nil)
		Expect(err).ToNot(HaveOccurred())

		return mustNew(spec)
	}

	encode := func(input string) map[string]any {
		GinkgoHelper()

		out, err := callTool(tool(), ctx, json.RawMessage(input), nil)
		Expect(err).ToNot(HaveOccurred())

		var decoded map[string]any
		Expect(json.Unmarshal([]byte(out), &decoded)).To(Succeed())

		return decoded
	}

	It("Should encode the UTF-8 bytes of the text as standard base64", func() {
		text := "hello, wörld"
		out := encode(`{"text":"hello, wörld"}`)
		Expect(out).To(HaveKeyWithValue("encoded", base64.StdEncoding.EncodeToString([]byte(text))))

		decoded, err := base64.StdEncoding.DecodeString(out["encoded"].(string))
		Expect(err).ToNot(HaveOccurred())
		Expect(string(decoded)).To(Equal(text))
	})

	It("Should encode an empty string to an empty string", func() {
		Expect(encode(`{"text":""}`)).To(HaveKeyWithValue("encoded", ""))
	})

	It("Should refuse an options block with a key and accept null and an empty object", func() {
		_, err := base64EncodeSpec(nil, json.RawMessage(`{"x": 1}`))
		Expect(err).To(MatchError(ContainSubstring("takes no options; remove the options block (it sets x)")))
		Expect(err).To(MatchError(ContainSubstring("x")))

		_, err = base64EncodeSpec(nil, json.RawMessage(`null`))
		Expect(err).ToNot(HaveOccurred())

		_, err = base64EncodeSpec(nil, json.RawMessage(`{}`))
		Expect(err).ToNot(HaveOccurred())
	})

	It("Should report a call without text as missing the required argument", func() {
		t := tool()
		missing := t.MissingRequired(json.RawMessage(`{}`))
		Expect(missing).To(Equal([]string{"text"}))
		Expect(t.MissingRequiredMessage(missing)).To(Equal(`tool "base64_encode" was called without required parameter(s): text. required: text`))
	})

	It("Should render the trace line as the byte count rather than the text", func() {
		t := tool()
		Expect(t.TraceLine(json.RawMessage(`{"text":"hello, wörld"}`))).To(Equal("base64_encode (13 bytes)"))
		Expect(t.TraceLine(json.RawMessage(`not json`))).To(Equal("base64_encode"))
	})

	It("Should declare MCP exposure, not a2a, and read-only behavior", func() {
		t := tool()
		Expect(t.MCPExposable()).To(BeTrue())
		Expect(t.A2AExposable()).To(BeFalse())
		Expect(t.Behavior().ReadOnly).To(Equal(toolkit.HintTrue))
		Expect(t.Describe(nil).Kind).To(Equal(toolkit.KindBuiltin))
	})
})
