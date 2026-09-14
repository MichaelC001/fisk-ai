//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package builtin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/toolkit"
	"github.com/choria-io/fisk-ai/internal/toolkit/functool"
)

var _ = Describe("read_file tool", func() {
	ctx := context.Background()

	var root string

	BeforeEach(func() {
		root = GinkgoT().TempDir()
		Expect(os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello, wörld"), 0o644)).To(Succeed())
	})

	// tool builds read_file over root with the options JSON given, through the same
	// mustNew the wiring uses.
	tool := func(cfg *config.Config, options string) *functool.Tool {
		GinkgoHelper()

		spec, err := readFileSpec(cfg, json.RawMessage(options))
		Expect(err).ToNot(HaveOccurred())

		return mustNew(spec)
	}

	rootTool := func() *functool.Tool {
		GinkgoHelper()

		return tool(nil, `{"root": `+jsonString(root)+`}`)
	}

	read := func(t *functool.Tool, path string) (map[string]any, error) {
		GinkgoHelper()

		input, err := json.Marshal(map[string]string{"path": path})
		Expect(err).ToNot(HaveOccurred())

		out, err := callTool(t, ctx, input, nil)
		if err != nil {
			return nil, err
		}

		var decoded map[string]any
		Expect(json.Unmarshal([]byte(out), &decoded)).To(Succeed())

		return decoded, nil
	}

	mustRead := func(t *functool.Tool, path string) map[string]any {
		GinkgoHelper()

		out, err := read(t, path)
		Expect(err).ToNot(HaveOccurred())

		return out
	}

	It("Should return a text file as utf-8 with its content", func() {
		out := mustRead(rootTool(), "a.txt")
		Expect(out).To(HaveKeyWithValue("path", "a.txt"))
		Expect(out).To(HaveKeyWithValue("found", true))
		Expect(out).To(HaveKeyWithValue("size", float64(len("hello, wörld"))))
		Expect(out).To(HaveKeyWithValue("encoding", "utf-8"))
		Expect(out).To(HaveKeyWithValue("content", "hello, wörld"))
	})

	It("Should return bytes that are not UTF-8 as base64", func() {
		raw := []byte{0xFF, 0xFE, 0x00, 0x41}
		Expect(os.WriteFile(filepath.Join(root, "bin"), raw, 0o644)).To(Succeed())

		out := mustRead(rootTool(), "bin")
		Expect(out).To(HaveKeyWithValue("encoding", "base64"))
		Expect(out).To(HaveKeyWithValue("size", float64(len(raw))))

		decoded, err := base64.StdEncoding.DecodeString(out["content"].(string))
		Expect(err).ToNot(HaveOccurred())
		Expect(decoded).To(Equal(raw))
	})

	It("Should ignore leading slashes so /a.txt, //a.txt and a.txt name one file", func() {
		t := rootTool()
		for _, path := range []string{"/a.txt", "//a.txt", "a.txt"} {
			out := mustRead(t, path)
			Expect(out).To(HaveKeyWithValue("path", path))
			Expect(out).To(HaveKeyWithValue("found", true))
			Expect(out).To(HaveKeyWithValue("content", "hello, wörld"))
		}
	})

	It("Should refuse an empty path", func() {
		_, err := read(rootTool(), "/")
		Expect(err).To(MatchError(ContainSubstring("non-empty path")))
	})

	It("Should refuse a path that escapes the root", func() {
		Expect(os.WriteFile(filepath.Join(filepath.Dir(root), "etc"), []byte("outside"), 0o644)).To(Succeed())

		_, err := read(rootTool(), "../etc")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).ToNot(ContainSubstring("outside"))
	})

	It("Should refuse a symlink whose target is outside the root", func() {
		outside := filepath.Join(GinkgoT().TempDir(), "outside.txt")
		Expect(os.WriteFile(outside, []byte("outside"), 0o644)).To(Succeed())
		Expect(os.Symlink(outside, filepath.Join(root, "out-link"))).To(Succeed())

		_, err := read(rootTool(), "out-link")
		Expect(err).To(HaveOccurred())
	})

	It("Should refuse a symlink with an absolute target even when that target is inside the root", func() {
		Expect(os.Symlink(filepath.Join(root, "a.txt"), filepath.Join(root, "abs-link"))).To(Succeed())

		_, err := read(rootTool(), "abs-link")
		Expect(err).To(HaveOccurred())
	})

	It("Should follow a relative symlink that stays inside the root", func() {
		Expect(os.Mkdir(filepath.Join(root, "sub"), 0o755)).To(Succeed())
		Expect(os.Symlink(filepath.Join("..", "a.txt"), filepath.Join(root, "sub", "rel-link"))).To(Succeed())

		out := mustRead(rootTool(), "sub/rel-link")
		Expect(out).To(HaveKeyWithValue("found", true))
		Expect(out).To(HaveKeyWithValue("content", "hello, wörld"))
	})

	It("Should refuse a directory", func() {
		Expect(os.Mkdir(filepath.Join(root, "sub"), 0o755)).To(Succeed())

		_, err := read(rootTool(), "sub")
		Expect(err).To(MatchError(ContainSubstring("not a regular file")))
	})

	It("Should report a missing file as found false rather than an error", func() {
		out := mustRead(rootTool(), "missing.txt")
		Expect(out).To(HaveKeyWithValue("path", "missing.txt"))
		Expect(out).To(HaveKeyWithValue("found", false))
	})

	It("Should refuse a file one byte over the cap and read one at the cap", func() {
		Expect(os.WriteFile(filepath.Join(root, "at"), []byte(strings.Repeat("x", 16)), 0o644)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(root, "over"), []byte(strings.Repeat("x", 17)), 0o644)).To(Succeed())

		t := tool(nil, `{"root": `+jsonString(root)+`, "max_bytes": 16}`)

		out := mustRead(t, "at")
		Expect(out).To(HaveKeyWithValue("size", float64(16)))
		Expect(out).To(HaveKeyWithValue("content", strings.Repeat("x", 16)))

		_, err := read(t, "over")
		Expect(err).To(MatchError(ContainSubstring("17 bytes")))
		Expect(err).To(MatchError(ContainSubstring("16 byte limit")))
	})

	It("Should refuse an unknown option key", func() {
		_, err := readFileSpec(nil, json.RawMessage(`{"roots": "/tmp"}`))
		Expect(err).To(MatchError(ContainSubstring("invalid read_file options")))
		Expect(err).To(MatchError(ContainSubstring("roots")))
	})

	It("Should refuse a negative max_bytes", func() {
		_, err := readFileSpec(nil, json.RawMessage(`{"root": `+jsonString(root)+`, "max_bytes": -1}`))
		Expect(err).To(MatchError(ContainSubstring("max_bytes must be zero or positive")))
	})

	It("Should fail at construction when the root does not exist", func() {
		_, err := readFileSpec(nil, json.RawMessage(`{"root": `+jsonString(filepath.Join(root, "absent"))+`}`))
		Expect(err).To(MatchError(ContainSubstring("read_file root")))
	})

	It("Should fail at construction when the root is not a directory", func() {
		_, err := readFileSpec(nil, json.RawMessage(`{"root": `+jsonString(filepath.Join(root, "a.txt"))+`}`))
		Expect(err).To(MatchError(ContainSubstring("read_file root")))
	})

	It("Should read under cfg.RootDirectory when root is empty", func() {
		cfg := &config.Config{RootDirectory: root}
		for _, options := range []string{``, `null`, `{}`} {
			out := mustRead(tool(cfg, options), "a.txt")
			Expect(out).To(HaveKeyWithValue("found", true), "options %q", options)
			Expect(out).To(HaveKeyWithValue("content", "hello, wörld"), "options %q", options)
		}
	})

	It("Should resolve a relative root under cfg.RootDirectory", func() {
		Expect(os.Mkdir(filepath.Join(root, "docs"), 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(root, "docs", "d.txt"), []byte("doc"), 0o644)).To(Succeed())

		t := tool(&config.Config{RootDirectory: root}, `{"root": "docs"}`)

		out := mustRead(t, "d.txt")
		Expect(out).To(HaveKeyWithValue("content", "doc"))

		out = mustRead(t, "a.txt")
		Expect(out).To(HaveKeyWithValue("found", false))
	})

	It("Should report a call without path as missing the required argument", func() {
		t := rootTool()
		missing := t.MissingRequired(json.RawMessage(`{}`))
		Expect(missing).To(Equal([]string{"path"}))
	})

	It("Should render the trace line as the tool name and the path", func() {
		t := rootTool()
		Expect(t.TraceLine(json.RawMessage(`{"path":"etc/hosts"}`))).To(Equal("read_file etc/hosts"))
		Expect(t.TraceLine(json.RawMessage(`not json`))).To(Equal("read_file"))
	})

	It("Should declare MCP exposure, not a2a, and read-only behavior", func() {
		t := rootTool()
		Expect(t.MCPExposable()).To(BeTrue())
		Expect(t.A2AExposable()).To(BeFalse())
		Expect(t.Behavior().ReadOnly).To(Equal(toolkit.HintTrue))
		Expect(t.Describe(nil).Kind).To(Equal(toolkit.KindBuiltin))
		Expect(t.Description()).ToNot(ContainSubstring(root))
	})
})

// jsonString is the JSON string literal for s, so a temporary directory path can be
// written into an options block.
func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}

	return string(b)
}
