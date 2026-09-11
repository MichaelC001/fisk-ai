//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package rag

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// approxCharsPerToken converts max_injected_tokens into the character count the
// expansion quota is measured in. Four characters per token is the conventional
// rough estimate for English text, and is the figure the knowledge tool caps its
// payload with, so the quota and that cap measure the same budget.
const approxCharsPerToken = 4

// sectionJoin separates two chunks in an expanded block. It is charged to the quota
// along with the chunk it introduces, so a block of any length stays within the share
// the hit was given.
const sectionJoin = "\n\n"

// sectionQuota is how many characters one hit may grow to, an equal share of the
// injection budget across the hits a search returns. Summed over topK hits it comes
// to the budget the knowledge tool caps the whole payload at, so expansion cannot
// push a result past max_injected_tokens. At top_k 20 the share is 1200 characters,
// which one chunk already fills, and nothing grows.
func (s *Store) sectionQuota(topK int) int {
	return s.maxInjectedTokens * approxCharsPerToken / topK
}

// expandSections grows each hit from the chunk that ranked to the text around it on
// the page, in rank order. Ordinals are 0-based and contiguous within a document,
// so the chunks either side of a hit are the text either side of it.
//
// It runs after fusion, on the hits alone, so ranking is unaffected. Two hits in one
// section would otherwise return the same text twice: a hit whose chunk an
// earlier block already took is dropped, and the block keeps the better-ranked hit's
// citation.
func (s *Store) expandSections(ctx context.Context, hits []Hit, topK int) ([]Hit, error) {
	quota := s.sectionQuota(topK)
	claimed := map[int64]map[int]bool{}

	out := make([]Hit, 0, len(hits))
	for _, h := range hits {
		docID, err := s.chunkDocument(ctx, h.ChunkID)
		if err != nil {
			return nil, err
		}
		if docID == 0 {
			out = append(out, h)
			continue
		}

		taken := claimed[docID]
		if taken == nil {
			taken = map[int]bool{}
			claimed[docID] = taken
		}
		if taken[h.Ordinal] {
			continue
		}

		block, err := s.growSection(ctx, docID, h, taken, quota)
		if err != nil {
			return nil, err
		}
		out = append(out, block)
	}

	return out, nil
}

// growSection steps outward from the hit, one chunk up then one chunk down, and
// returns the hit carrying the text of the range it reached. A direction stops at
// the edge of the document, at the first chunk outside the hit's section, at a chunk
// an earlier block took, or when the next chunk would carry the block past the
// quota. A hit at ordinal 0 has nothing above it and spends the rest downward.
//
// A hit that reached no further than its own chunk is returned as it came, with no
// span, so the result is what a search with expansion off returns.
func (s *Store) growSection(ctx context.Context, docID int64, hit Hit, taken map[int]bool, quota int) (Hit, error) {
	bodies := map[int]string{hit.Ordinal: hit.Content}
	taken[hit.Ordinal] = true
	low, high := hit.Ordinal, hit.Ordinal
	used := len(hit.Content)

	step := func(ordinal int) (bool, error) {
		if taken[ordinal] {
			return false, nil
		}

		body, ok, err := s.sectionNeighbor(ctx, docID, ordinal, hit.HeadingPath)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}

		cost := len(body) + len(sectionJoin)
		if used+cost > quota {
			return false, nil
		}

		bodies[ordinal] = body
		taken[ordinal] = true
		used += cost

		return true, nil
	}

	up, down := true, true
	for up || down {
		if up {
			moved, err := step(low - 1)
			if err != nil {
				return Hit{}, err
			}
			if moved {
				low--
			} else {
				up = false
			}
		}

		if down {
			moved, err := step(high + 1)
			if err != nil {
				return Hit{}, err
			}
			if moved {
				high++
			} else {
				down = false
			}
		}
	}

	if low == high {
		return hit, nil
	}

	parts := make([]string, 0, high-low+1)
	for i := low; i <= high; i++ {
		parts = append(parts, bodies[i])
	}

	hit.Content = strings.Join(parts, sectionJoin)
	hit.Span = spanCitation(citationPath(hit.Citation, hit.Ordinal), low, high)

	return hit, nil
}

// sectionNeighbor returns the body of the chunk at ordinal in document docID and
// reports whether that chunk is in the section crumb names. Past either end of the
// document there is no row, and a chunk under a sibling or an ancestor heading is
// not in the section; both stop the walk in that direction.
func (s *Store) sectionNeighbor(ctx context.Context, docID int64, ordinal int, crumb string) (string, bool, error) {
	var got, body string

	err := s.db.QueryRowContext(ctx,
		`SELECT heading_path, body FROM chunks WHERE document_id = ? AND ordinal = ?`,
		docID, ordinal).Scan(&got, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("expanding to section: %w", err)
	}
	if !inSection(got, crumb) {
		return "", false, nil
	}

	return body, true, nil
}

// inSection reports whether a chunk whose breadcrumb is crumb sits in the section
// the breadcrumb hit names. A subsection comes with the hit, which is what the
// prefix admits; a sibling and an ancestor do not. The separator is part of the
// prefix, so "Setup Notes" does not read as nested under "Setup".
//
// One shape slips through. Two "## A" headings in a row with no heading between
// them are one breadcrumb, so the walk crosses from the first into the second and
// the quota limits what it takes. Telling them apart needs a section id stored per
// chunk, which every existing index would have to be re-embedded to gain.
func inSection(crumb, hit string) bool {
	return crumb == hit || strings.HasPrefix(crumb, hit+crumbSeparator)
}

// chunkDocument returns the document a chunk belongs to, or 0 when the chunk is
// gone. A reindex between the ranking and the walk leaves such a hit unexpanded
// rather than failing the search, which is how hydrate treats the same race.
func (s *Store) chunkDocument(ctx context.Context, chunkID int64) (int64, error) {
	var docID int64

	err := s.db.QueryRowContext(ctx, `SELECT document_id FROM chunks WHERE id = ?`, chunkID).Scan(&docID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("expanding to section: %w", err)
	}

	return docID, nil
}

// spanCitation renders the range a block covers as <relpath>#<low>..#<high>, the
// form Hit.Span carries.
func spanCitation(relPath string, low, high int) string {
	return fmt.Sprintf("%s#%d..#%d", relPath, low, high)
}

// citationPath returns the stored document path a citation token was rendered from.
// Hit.DocPath has been joined under the root by the time a hit is expanded, and a
// span has to name the same path its citation does.
func citationPath(citation string, ordinal int) string {
	return strings.TrimSuffix(citation, fmt.Sprintf("#%d", ordinal))
}
