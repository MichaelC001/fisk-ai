//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"go.opentelemetry.io/otel/attribute"
)

// ContentOptions is what the facade tells a builder about how much to render. Both
// values are the leaf's, so a call site never branches on whether capture is on or on
// how it is configured.
type ContentOptions struct {
	// Full selects the whole conversation rather than the turn's delta. Only the
	// message builders read it.
	Full bool
	// MaxBytes caps the document a builder returns, measured on the encoded JSON
	// rather than on the Go strings behind it. That distinction is the whole reason
	// this field is documented: encoding/json escapes '<' to six bytes and replaces
	// each invalid UTF-8 byte with a three-byte U+FFFD, so a budget applied to the
	// string before encoding does not bound what is exported.
	MaxBytes int
}

// Content is one rendered content attribute and what had to be done to make it fit.
type Content struct {
	// JSON is the document, and is empty when no valid one could be produced.
	//
	// Empty is the only failure signal there is, deliberately: an error would have to
	// carry something, and encoding/json quotes the offending bytes in its own error
	// text, so returning one would put captured content on a span or on the operator's
	// terminal by the back door.
	JSON string
	// Truncated reports that something was cut to fit MaxBytes.
	Truncated bool
	// DroppedMessages counts whole messages removed from the front to fit.
	//
	// It exists because dropping a message leaves a document that is valid,
	// complete-looking and simply shorter, so unlike a cut string it carries no
	// in-band sign at all. Nothing else reports it.
	DroppedMessages int
	// FromIndex is the conversation index the first exported message sits at. It
	// places a delta for a reader, and across a trace the indices chain, so a gap
	// between consecutive model calls is a span that never arrived.
	//
	// The builder resolves it rather than the caller because the builder clamps the
	// index it was handed.
	FromIndex int
}

// ContentBuilder renders one content attribute on demand.
//
// It is a func so that nothing is serialized when capture is off: the span invokes it
// only when it is going to record the result, so a run with capture off pays for
// building a closure and nothing else.
//
// It is called synchronously by the span method it was handed to, and is never
// stored. That is what makes closing over the run's live message slice safe, and the
// guarantee has two halves that both have to hold: a builder must never be kept to be
// run later (the conversation has moved on by then), and Content must never be
// changed to carry a reference into caller memory rather than a string (the batch
// processor reads exported attributes on its own goroutine).
type ContentBuilder func(ContentOptions) Content

// contentAttr pairs a content attribute's key with the builder that renders it.
//
// withIndex marks the one attribute per span whose starting position in the
// conversation is meaningful, so a reader can place a delta and, across a trace, see
// where a span went missing.
type contentAttr struct {
	key       attribute.Key
	build     ContentBuilder
	withIndex bool
}

// recordContent renders the content attributes for one span and records what had to be
// cut to fit them.
//
// It is the only place a ContentBuilder is ever invoked, and it returns before invoking
// any of them when capture is off, which is what makes the lazy builder worth having: a
// run with capture off pays for constructing closures and nothing else. Content is set
// from the span's single Finish rather than through setters of its own for the reason
// section 6.2.1 records: a tool call has eight ways to end and one place that ends its
// span, so a second place to attach data to it is a second place to keep in agreement.
func (s *Span) recordContent(attrs ...contentAttr) {
	if s == nil || s.span == nil || s.provider == nil || !s.provider.capture.enabled {
		return
	}

	opts := ContentOptions{Full: s.provider.capture.full, MaxBytes: s.provider.capture.maxBytes}

	var out []attribute.KeyValue
	var truncated []string
	dropped := 0

	for _, a := range attrs {
		if a.build == nil {
			continue
		}

		c := a.build(opts)
		if c.JSON == "" {
			continue
		}

		out = append(out, a.key.String(c.JSON))
		if c.Truncated {
			truncated = append(truncated, string(a.key))
		}
		dropped += c.DroppedMessages

		if a.withIndex {
			out = append(out, AttrContentFromIndex.Int(c.FromIndex))
		}
	}

	if len(out) == 0 {
		return
	}

	if len(truncated) > 0 {
		out = append(out, AttrContentTruncated.StringSlice(truncated))
	}
	if dropped > 0 {
		out = append(out, AttrContentDroppedMessages.Int(dropped))
	}

	s.span.SetAttributes(out...)
}
