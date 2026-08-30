//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package llm

import (
	"cmp"
	"slices"
	"strings"
	"sync"
)

// BlockSource is which copy of a block's text an AssembledBlock carries.
type BlockSource string

const (
	// SourceFragments is text held as fragments alone, no whole block having arrived for
	// the index. A consumer reads it while a call is still being written, after a call
	// that failed before it returned a turn, and on an a2a stream that dropped the whole
	// block. The last of those is the one a consumer may want to act on, and only it can
	// tell the three apart.
	SourceFragments BlockSource = "fragments"

	// SourceBlock is the whole block. It replaced the fragments the index held, or the
	// index streamed none.
	SourceBlock BlockSource = "block"

	// SourceKeptFragments is the fragments, kept over a whole block that arrived
	// trimmed. A producer caps a fragment to what one message carries and never caps
	// the fragments of a block in aggregate, so on a long answer the fragments hold the
	// complete text and the whole block holds a cut copy of it.
	//
	// A consumer that rendered the fragments as they arrived already shows this text and
	// renders nothing further for the index.
	SourceKeptFragments BlockSource = "kept_fragments"
)

// WholeBlock is a complete content block of an assistant turn, for reconciling against
// the fragments that carried it.
//
// A caller of CallStream builds one from a block of Response.Content. A receiver of an
// a2a reply set builds one from the a2a.TextBlock or a2a.ThinkingBlock that closes the
// block.
type WholeBlock struct {
	// Kind is the kind of block, which decides how a consumer renders the text.
	Kind DeltaKind

	// Index is the block's position in the model call that produced it, the same number
	// the Delta values carrying its fragments have.
	Index int

	// Text is the whole block's text.
	Text string

	// Trimmed reports that Text was cut to a limit and the rest of it stayed with the
	// producer, which is what a2a.TrimBlockText does to a block over a2a.MaxBlockText. A
	// provider cuts nothing, so a caller reconciling a CallStream turn leaves it false.
	Trimmed bool
}

// AssembledBlock is one content block as a DeltaAssembler holds it: the text decided for
// the index, and which copy that text is.
type AssembledBlock struct {
	// Kind is the kind of block, taken from the whole block when one arrived and from
	// the first fragment otherwise.
	Kind DeltaKind

	// Index is the block's position in the model call that produced it.
	Index int

	// Text is the block's text as the rule decided it.
	Text string

	// Source is which copy Text is, and says whether a consumer that already rendered
	// the fragments needs to render anything further.
	Source BlockSource
}

// DeltaAssembler collects the fragments of a model call and reconciles them against the
// whole blocks that end it. A consumer of a delta stream needs a buffer per block index
// and the rule below, so both are here rather than in each adapter that renders a stream.
//
// The rule: a whole block replaces the fragments of its index, unless it arrived trimmed
// and the index holds fragments. A trimmed block is a cut copy of text the fragments
// carried in full, so taking it would turn a complete answer into a truncated one. An
// index that streamed nothing takes the block, trimmed or not, which covers both a
// producer that dropped the fragments and one that never sent any.
//
// The rule assumes every fragment of an index arrived. Fragments dropped in transit
// leave holes the assembler cannot see, and it will then keep holed fragments over a
// trimmed block that was merely cut. A consumer on a lossy path knows what it lost, an
// a2a receiver from a2a.TaskStream.Gaps, and decides for itself.
//
// One assembler holds one model call. Index restarts at 0 on every call, so a consumer
// following a run of several calls resets between them. A caller of CallStream makes an
// assembler per call. A receiver of an a2a reply set calls Reset on the a2a.StatusBlock
// that ends a call, which arrives once per call after that call's whole blocks; the
// iteration on a fragment is the same number but arrives only when a fragment does, so
// a call whose fragments were all lost would leave the previous call's text in place.
//
// The zero value is ready to use and must not be copied once used. It is safe for
// concurrent use: fragments arrive on the goroutine that called CallStream and are often
// rendered on another, so every method locks and Blocks returns a slice the caller owns.
type DeltaAssembler struct {
	blocks map[int]*blockState
	mu     sync.Mutex
}

// blockState is one index's fragments together with its whole block, once that arrives.
type blockState struct {
	kind      DeltaKind
	fragments strings.Builder
	whole     bool
	text      string
	trimmed   bool
}

// AddDelta appends a fragment's text to the block at its index. The first fragment of an
// index names the block's kind until a whole block says otherwise.
//
// A Final fragment adds its text like any other. AddBlock ends an index, not Final; a
// consumer rendering fragments as they arrive reads Final off the Delta.
func (a *DeltaAssembler) AddDelta(d Delta) {
	a.mu.Lock()
	defer a.mu.Unlock()

	b := a.stateFor(d.Index)
	if b.kind == "" {
		b.kind = d.Kind
	}

	b.fragments.WriteString(d.Text)
}

// AddBlock reconciles a whole block against the fragments held for its index and returns
// what that index now assembles to, so a consumer reads from the return whether to render
// the block or leave what it has already shown.
func (a *DeltaAssembler) AddBlock(w WholeBlock) AssembledBlock {
	a.mu.Lock()
	defer a.mu.Unlock()

	b := a.stateFor(w.Index)
	b.kind = w.Kind
	b.text = w.Text
	b.trimmed = w.Trimmed
	b.whole = true

	return b.assembled(w.Index)
}

// Blocks returns every index the assembler holds, ordered by index. The slice and the
// text in it belong to the caller and do not change as more fragments arrive.
func (a *DeltaAssembler) Blocks() []AssembledBlock {
	a.mu.Lock()
	defer a.mu.Unlock()

	out := make([]AssembledBlock, 0, len(a.blocks))
	for index, b := range a.blocks {
		out = append(out, b.assembled(index))
	}

	slices.SortFunc(out, func(x, y AssembledBlock) int { return cmp.Compare(x.Index, y.Index) })

	return out
}

// Reset drops every block, leaving the assembler ready for the next model call.
func (a *DeltaAssembler) Reset() {
	a.mu.Lock()
	defer a.mu.Unlock()

	clear(a.blocks)
}

// stateFor is the state of one index, started on first use. Callers hold a.mu.
func (a *DeltaAssembler) stateFor(index int) *blockState {
	b, held := a.blocks[index]
	if held {
		return b
	}

	if a.blocks == nil {
		a.blocks = make(map[int]*blockState, 4)
	}

	b = &blockState{}
	a.blocks[index] = b

	return b
}

// assembled applies the rule DeltaAssembler documents to one index.
func (s *blockState) assembled(index int) AssembledBlock {
	out := AssembledBlock{Kind: s.kind, Index: index}

	switch {
	case !s.whole:
		out.Text = s.fragments.String()
		out.Source = SourceFragments
	case s.trimmed && s.fragments.Len() > 0:
		out.Text = s.fragments.String()
		out.Source = SourceKeptFragments
	default:
		out.Text = s.text
		out.Source = SourceBlock
	}

	return out
}
