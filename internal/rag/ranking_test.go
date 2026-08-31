//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package rag

import (
	"context"
	"fmt"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/choria-io/fisk-ai/config"
)

// rankingFixture is a fixed corpus for the BM25 ranking comparison. Every document
// puts a query term in a heading, a body, or both, because the heading is exactly
// what the ranking change moves: folding counted heading tokens twice, once in the
// body column and once in heading_path, so unfolding removes an implicit weight
// that the explicit bm25 weights have to put back.
var rankingFixture = map[string]string{
	"deprecation.md": "# Deprecation Policy\n\n" +
		"Interfaces are removed two releases after the announcement. Every removal is listed in\n" +
		"the release notes, and the replacement is named there.\n\n" +
		"## Timelines\n\nA removal is announced, then held for two releases, then carried out.\n",

	"api.md": "# API Reference\n\n" +
		"The API is versioned per endpoint.\n\n" +
		"## Deprecated Endpoints\n\n" +
		"A deprecated endpoint keeps answering until it is removed. A deprecated endpoint\n" +
		"returns a warning header, and the deprecation is recorded against the API version\n" +
		"that introduced it.\n",

	"retention.md": "# Retention\n\n" +
		"Records are held for ninety days.\n\n" +
		"## Policy\n\n" +
		"changes are announced a release ahead. The retention window is a policy setting\n" +
		"rather than a hard limit.\n",

	"backpressure.md": "# Design\n\n" +
		"## Backpressure\n\n" +
		"The queue applies backpressure when the buffer is full so producers slow down.\n",

	"auth.md": "# Authentication\n\n" +
		"Tokens are validated against the issuer before any request proceeds. An expired\n" +
		"token is rejected before the request reaches the handler.\n",
}

// rankingQueries are the queries the comparison runs. They are chosen to hit the
// heading-versus-body distinction: "policy" and "deprecation" each appear in one
// document's heading and another's body.
var rankingQueries = []string{
	"deprecation policy",
	"policy",
	"deprecated endpoint",
	"retention window",
	"backpressure buffer",
	"authentication tokens issuer",
}

// rankingBaseline is the ranking each query produced against the folded schema,
// recorded before the fold was removed. Folding stored the breadcrumb inside the
// indexed body column, so heading tokens counted twice and BM25 weighted headings
// implicitly. Unfolding removes that weight, and bm25(chunks_fts, 1.0, 2.0) is what
// puts it back deliberately. This pins the two against each other rather than
// trusting that they agree.
var rankingBaseline = map[string][]string{
	"deprecation policy":           {"deprecation.md#1", "deprecation.md#0", "api.md#1", "retention.md#1"},
	"policy":                       {"retention.md#1", "deprecation.md#1", "deprecation.md#0"},
	"deprecated endpoint":          {"api.md#1", "api.md#0", "deprecation.md#1", "deprecation.md#0"},
	"retention window":             {"retention.md#1", "retention.md#0"},
	"backpressure buffer":          {"backpressure.md#0"},
	"authentication tokens issuer": {"auth.md#0"},
}

var _ = Describe("Lexical ranking", func() {
	ctx := context.Background()

	var cfg *config.Config

	BeforeEach(func() {
		tmp := GinkgoT().TempDir()
		docsD := filepath.Join(tmp, "docs")
		cfg = lexicalConfig(filepath.Join(tmp, "knowledge"))

		for rel, body := range rankingFixture {
			writeDoc(docsD, rel, body)
		}

		w, err := OpenWriter(cfg, "", Options{})
		Expect(err).ToNot(HaveOccurred())
		defer w.Close()
		_, err = w.Index(ctx, []string{docsD}, IndexOptions{Reconcile: true})
		Expect(err).ToNot(HaveOccurred())
	})

	It("matches the ranking recorded before the heading was unfolded", func() {
		r, err := Open(cfg, "", Options{})
		Expect(err).ToNot(HaveOccurred())
		defer r.Close()

		for _, q := range rankingQueries {
			res, err := r.Search(ctx, q, 20)
			Expect(err).ToNot(HaveOccurred())

			got := make([]string, 0, len(res.Hits))
			for _, h := range res.Hits {
				got = append(got, fmt.Sprintf("%s#%d", filepath.Base(h.DocPath), h.Ordinal))
			}

			Expect(got).To(Equal(rankingBaseline[q]), "ranking changed for query %q", q)
		}
	})
})
