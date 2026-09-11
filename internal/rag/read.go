//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package rag

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// readShare divides the injection budget into what one ReadChunks call may return,
// as a divisor.
const readShare = 2

// ParseCitation splits a <relpath>#<ordinal> citation token into the document path
// and the ordinal it names, the inverse of Citation. The path is the one the indexer
// stored, which is what ReadChunks and ChunkText look a document up by.
//
// A token with no "#", an empty path, an ordinal that is not a number and a negative
// ordinal are each an error naming the form that is accepted. A token the index does
// not hold parses here and is reported by the lookup that follows.
func ParseCitation(citation string) (string, int, error) {
	i := strings.LastIndex(citation, "#")
	if i < 0 {
		return "", 0, fmt.Errorf("citation %q is missing the '#<ordinal>' suffix; expected <relpath>#<ordinal>", citation)
	}

	relPath := citation[:i]
	ordinal, err := strconv.Atoi(citation[i+1:])
	if relPath == "" || err != nil || ordinal < 0 {
		return "", 0, fmt.Errorf("citation %q is malformed; expected <relpath>#<ordinal>, e.g. docs/design.md#3", citation)
	}

	return relPath, ordinal, nil
}

// ReadOptions carries how many chunks either side of the cited one a read takes.
// ReadOptions{} returns the cited chunk alone, and a negative count is read as zero.
type ReadOptions struct {
	// Before is how many chunks above the cited one to include. The read stops early
	// at ordinal 0 and at the quota.
	Before int
	// After is how many chunks below the cited one to include, stopping at the last
	// chunk of the document and at the quota.
	After int
}

// ReadResult is the block one ReadChunks call returns.
type ReadResult struct {
	// Citation is the <relpath>#<ordinal> token of the chunk that was asked for,
	// re-rendered from the stored path, and MappedCitation is that chunk under the
	// operator's citation rules, with Mapped reporting whether a rule matched. They
	// are the pair Hit carries, so a caller cites a read exactly as it cites a hit.
	Citation       string
	MappedCitation string
	Mapped         bool

	// DocPath is where the document sits on the filesystem, joined under
	// config.Config.RootDirectory on the terms Hit.DocPath describes.
	DocPath string

	// Ordinal is the chunk the citation named, and HeadingPath is its breadcrumb. A
	// block reaching past the section carries the cited chunk's breadcrumb, not the
	// breadcrumbs of everything in it.
	Ordinal     int
	HeadingPath string

	// Content is the text of the chunks First to Last, joined by a blank line.
	Content string

	// First and Last are the ordinals the block covers, and Span renders them as
	// <relpath>#<low>..#<high>. Span is empty for a block of one chunk. Reading on
	// from either end means citing that end's ordinal in the next call.
	First int
	Last  int
	Span  string

	// DocumentChunks is how many chunks the document holds, so its readable ordinals
	// are 0 to DocumentChunks-1.
	DocumentChunks int

	// Truncated reports that the quota stopped the block short of what ReadOptions
	// asked for. A block that stopped at an edge of the document instead leaves this
	// false, and First, Last and DocumentChunks say where it stopped.
	Truncated bool
}

// ReadChunks returns the chunk citation names along with the chunks either side of
// it that opts asks for, as one block. A model holding a citation from a search
// result reads the rest of the document through this, whether or not it has a tool
// that opens files.
//
// It crosses heading boundaries: before and after count chunks of the document, so a
// read can start in one section and end in the next. That is the difference from the
// expansion harness.knowledge.expand_to_section performs, which stops at the edge of
// the hit's section because nobody asked for the text beyond it.
//
// A citation the index does not hold is ErrCitationNotFound, which a reindex
// produces by renumbering a document's chunks. A malformed token is the error
// ParseCitation returns, and a store with no index file reports ErrIndexNotBuilt.
func (s *Store) ReadChunks(ctx context.Context, citation string, opts ReadOptions) (*ReadResult, error) {
	relPath, ordinal, err := ParseCitation(citation)
	if err != nil {
		return nil, err
	}

	if s.db == nil {
		return nil, ErrIndexNotBuilt
	}

	res := &ReadResult{Ordinal: ordinal, First: ordinal, Last: ordinal}

	var docID int64
	err = s.db.QueryRowContext(ctx,
		`SELECT d.id, c.heading_path, c.body
		 FROM chunks c JOIN documents d ON d.id = c.document_id
		 WHERE d.path = ? AND c.ordinal = ?`,
		relPath, ordinal).Scan(&docID, &res.HeadingPath, &res.Content)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrCitationNotFound, citation)
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", citation, err)
	}

	// Both citations render from the stored key, as hydrate renders them, so a rule
	// written against the corpus as it was walked matches here too.
	res.Citation = Citation(relPath, ordinal)
	res.MappedCitation, res.Mapped = s.citations.Render(relPath, ordinal, res.HeadingPath)
	res.DocPath = s.docPath(relPath)

	res.DocumentChunks, err = s.documentChunks(ctx, docID)
	if err != nil {
		return nil, err
	}

	err = s.growRead(ctx, docID, relPath, res, opts)
	if err != nil {
		return nil, err
	}

	return res, nil
}

// readQuota is how many characters one read may return. A search spends the whole
// injection budget across top_k hits and an expanded hit takes an equal share of it,
// 1200 characters at top_k 20; a read is the model spending on the one document it
// chose, so it takes more than a hit's share and never the budget itself. At the
// default max_injected_tokens of 6000 that is 12000 characters, around ten chunks,
// and two reads cost what one search does.
func (s *Store) readQuota() int {
	return s.maxInjectedTokens * approxCharsPerToken / readShare
}

// growRead steps outward from the cited chunk, one chunk up then one chunk down,
// until each direction has the count opts asked for. A direction also stops at the
// edge of the document and when the next chunk would carry the block past the quota,
// which is the one stop the caller cannot see from the text and so sets Truncated.
//
// Stepping alternately rather than taking all of before and then all of after splits
// a quota too small for both between the two sides.
func (s *Store) growRead(ctx context.Context, docID int64, relPath string, res *ReadResult, opts ReadOptions) error {
	quota := s.readQuota()
	bodies := map[int]string{res.Ordinal: res.Content}
	used := len(res.Content)

	step := func(ordinal int) (bool, error) {
		body, ok, err := s.chunkBody(ctx, docID, ordinal)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}

		cost := len(body) + len(sectionJoin)
		if used+cost > quota {
			res.Truncated = true
			return false, nil
		}

		bodies[ordinal] = body
		used += cost

		return true, nil
	}

	up, down := opts.Before > 0, opts.After > 0
	for up || down {
		if up {
			moved, err := step(res.First - 1)
			if err != nil {
				return err
			}
			if moved {
				res.First--
				up = res.Ordinal-res.First < opts.Before
			} else {
				up = false
			}
		}

		if down {
			moved, err := step(res.Last + 1)
			if err != nil {
				return err
			}
			if moved {
				res.Last++
				down = res.Last-res.Ordinal < opts.After
			} else {
				down = false
			}
		}
	}

	if res.First == res.Last {
		return nil
	}

	parts := make([]string, 0, res.Last-res.First+1)
	for i := res.First; i <= res.Last; i++ {
		parts = append(parts, bodies[i])
	}

	res.Content = strings.Join(parts, sectionJoin)
	res.Span = spanCitation(relPath, res.First, res.Last)

	return nil
}

// chunkBody returns the body of the chunk at ordinal in document docID and reports
// whether the document has a chunk there. Past either end there is no row, which
// stops a read in that direction.
func (s *Store) chunkBody(ctx context.Context, docID int64, ordinal int) (string, bool, error) {
	var body string

	err := s.db.QueryRowContext(ctx,
		`SELECT body FROM chunks WHERE document_id = ? AND ordinal = ?`, docID, ordinal).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading chunk %d: %w", ordinal, err)
	}

	return body, true, nil
}

// documentChunks counts the chunks a document holds, which is what tells a reader
// how far it can read: the ordinals are 0 to the count minus one.
func (s *Store) documentChunks(ctx context.Context, docID int64) (int, error) {
	var n int

	err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM chunks WHERE document_id = ?`, docID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("counting the chunks of a document: %w", err)
	}

	return n, nil
}
