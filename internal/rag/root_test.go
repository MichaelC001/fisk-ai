//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package rag

import (
	"context"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
)

var _ = Describe("The configured root directory", func() {
	ctx := context.Background()

	rootCfg := func(root, dir string) *config.Config {
		return &config.Config{
			Identity:      "agent",
			RootDirectory: root,
			Harness:       config.HarnessConfig{RAG: &config.RAGConfig{Enabled: true, Directory: dir}},
		}
	}

	Describe("resolveDir", func() {
		It("rebases the default knowledge directory under the root", func() {
			Expect(resolveDir(rootCfg("/srv/agent", ""), "")).To(Equal(filepath.Join("/srv/agent", "knowledge", "agent")))
		})

		It("rebases a relative configured directory under the root", func() {
			Expect(resolveDir(rootCfg("/srv/agent", "kb"), "")).To(Equal(filepath.Join("/srv/agent", "kb")))
		})

		It("honors an absolute configured directory regardless of the root", func() {
			Expect(resolveDir(rootCfg("/srv/agent", "/abs/kb"), "")).To(Equal("/abs/kb"))
		})

		It("lets an explicit store base replace the root", func() {
			Expect(resolveDir(rootCfg("/srv/agent", "kb"), "/var/lib/fisk")).To(Equal(filepath.Join("/var/lib/fisk", "kb")))
			Expect(StorePath(rootCfg("/srv/agent", "kb"), "/var/lib/fisk")).To(Equal(filepath.Join("/var/lib/fisk", "kb", dbFileName)))
		})

		It("keeps the working-directory behavior when neither is set", func() {
			Expect(resolveDir(rootCfg("", ""), "")).To(Equal(filepath.Join("knowledge", "agent")))
		})
	})

	// The stored key is what a citation and a citation rule are rendered from, and
	// DocPath is what a reader opens. The two part company exactly when the index holds
	// relative keys and a root is set.
	Describe("the path a search reports", func() {
		It("hands back an absolute path while the citation renders from the relative key", func() {
			root := GinkgoT().TempDir()
			writeDoc(filepath.Join(root, "docs"), "backpressure.md", "# Design\n\n## Backpressure\n\nThe queue applies backpressure when the buffer is full.\n")

			// The corpus is walked as "docs" from inside the root, so the index holds
			// docs/backpressure.md, the key an index built before the root would hold.
			GinkgoT().Chdir(root)

			// Parsed rather than built by hand, since a citation rule is compiled by
			// Prepare and a rule that never compiled would match nothing.
			prepared, err := config.ParseConfig([]byte(`
application_path: /bin/ls
identity: agent
system_prompt: hello
root_directory: ` + root + `
llm:
  model: claude-opus-4-8
harness:
  knowledge:
    enabled: true
    directory: ` + filepath.Join(root, "knowledge") + `
    citations:
      - pattern: '^docs/(.+)$'
        replace: 'https://example.net/$1'
`))
			Expect(err).ToNot(HaveOccurred())

			w, err := OpenWriter(prepared, "", Options{})
			Expect(err).ToNot(HaveOccurred())
			_, err = w.Index(ctx, []string{"docs"}, IndexOptions{Reconcile: true})
			Expect(err).ToNot(HaveOccurred())
			Expect(w.Close()).To(Succeed())

			r, err := Open(prepared, "", Options{})
			Expect(err).ToNot(HaveOccurred())
			defer r.Close()

			res, err := r.Search(ctx, "how does backpressure work", 5)
			Expect(err).ToNot(HaveOccurred())
			Expect(res.Hits).ToNot(BeEmpty())

			hit := res.Hits[0]
			Expect(hit.DocPath).To(Equal(filepath.Join(root, "docs", "backpressure.md")))
			Expect(hit.Citation).To(MatchRegexp(`^docs/backpressure\.md#\d+$`))
			Expect(hit.Mapped).To(BeTrue())
			Expect(hit.MappedCitation).To(Equal("https://example.net/backpressure.md"))
		})

		It("returns the stored key unchanged when no root is set", func() {
			root := GinkgoT().TempDir()
			writeDoc(filepath.Join(root, "docs"), "backpressure.md", "# Design\n\n## Backpressure\n\nThe queue applies backpressure when the buffer is full.\n")
			GinkgoT().Chdir(root)

			cfg := rootCfg("", filepath.Join(root, "knowledge"))

			w, err := OpenWriter(cfg, "", Options{})
			Expect(err).ToNot(HaveOccurred())
			_, err = w.Index(ctx, []string{"docs"}, IndexOptions{Reconcile: true})
			Expect(err).ToNot(HaveOccurred())
			Expect(w.Close()).To(Succeed())

			r, err := Open(cfg, "", Options{})
			Expect(err).ToNot(HaveOccurred())
			defer r.Close()

			res, err := r.Search(ctx, "how does backpressure work", 5)
			Expect(err).ToNot(HaveOccurred())
			Expect(res.Hits).ToNot(BeEmpty())
			Expect(res.Hits[0].DocPath).To(Equal("docs/backpressure.md"))
			Expect(res.Hits[0].Citation).To(MatchRegexp(`^docs/backpressure\.md#\d+$`))
		})

		It("leaves an absolute stored key alone under a root", func() {
			root := GinkgoT().TempDir()
			corpus := filepath.Join(root, "docs")
			writeDoc(corpus, "backpressure.md", "# Design\n\n## Backpressure\n\nThe queue applies backpressure when the buffer is full.\n")

			cfg := rootCfg(root, filepath.Join(root, "knowledge"))

			w, err := OpenWriter(cfg, "", Options{})
			Expect(err).ToNot(HaveOccurred())
			_, err = w.Index(ctx, []string{corpus}, IndexOptions{Reconcile: true})
			Expect(err).ToNot(HaveOccurred())
			Expect(w.Close()).To(Succeed())

			r, err := Open(cfg, "", Options{})
			Expect(err).ToNot(HaveOccurred())
			defer r.Close()

			res, err := r.Search(ctx, "how does backpressure work", 5)
			Expect(err).ToNot(HaveOccurred())
			Expect(res.Hits).ToNot(BeEmpty())
			Expect(res.Hits[0].DocPath).To(Equal(filepath.Join(corpus, "backpressure.md")))
		})
	})
})
