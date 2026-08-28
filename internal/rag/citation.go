//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package rag

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/choria-io/fisk-ai/config"
)

// CitationMapper rewrites the document path in a knowledge citation into the
// citation the operator's rules produce, using the ordered rules written under
// harness.knowledge.citations. A rule is a regular expression substitution over
// the stored path: most often it renders a URL a reader can open, but nothing
// stops it rendering a ticket key, an internal document id or a page title.
//
// A mapper holding no rules passes every citation through unrewritten, the raw
// <relpath>#<ordinal> token from Render and the document path from
// RenderDocument, so a caller never needs to know whether an operator configured
// any. Most corpora are published nowhere and are cited that way.
type CitationMapper struct {
	rules []config.RAGCitationRule
}

// NewCitationMapper returns a mapper over rules, which are tried in the order
// given. Pass what Config.RAGCitationRules returns: it hands back the rules
// config compiled and whose replacements it checked against their own pattern's
// groups.
//
// A nil mapper, a nil or empty slice, and a rule carrying no compiled pattern all
// pass a citation through: the mapper reports no match and the caller keeps the
// raw token.
func NewCitationMapper(rules []config.RAGCitationRule) *CitationMapper {
	return &CitationMapper{rules: rules}
}

// Render returns the mapped citation for one cited chunk and whether a rule
// matched.
//
// docPath is the document path as the indexer stored it, ordinal is the chunk's
// ordinal within that document, and headingPath is the chunk's breadcrumb in the
// "A > B > C" form Chunk carries. The first rule whose pattern matches docPath
// wins and its replacement is rendered; a path no rule matches is returned as
// Citation(docPath, ordinal) with false, since a corpus that is only partly
// published is the normal case.
//
// Callers need the second value rather than a comparison against
// Citation(docPath, ordinal): a rule may legitimately render a path unchanged.
//
// A mapped citation left ending in a bare "#", as ${heading} and ${anchor} leave
// one for a chunk with no heading, has that "#" trimmed. ${ordinal}
// always has a value and never reaches the trim. A replacement writing a
// literal between the "#" and an empty value, such as "#section-${ordinal}",
// renders "#section-" and is the operator's to get right.
//
// Every value substituted into a replacement is percent-encoded, a capture group
// and a reserved name alike, so a directory named a@evil.example renders as
// a%40evil.example. "/" is left alone, because a capture routinely spans
// directories. The literal text of the replacement is the operator's own and is
// written out as it stands.
func (m *CitationMapper) Render(docPath string, ordinal int, headingPath string) (string, bool) {
	citation, ok := m.render(docPath, citationReserved{ordinal: strconv.Itoa(ordinal), headingPath: headingPath})
	if !ok {
		return Citation(docPath, ordinal), false
	}

	return citation, true
}

// RenderDocument returns the mapped citation for a whole document and whether a
// rule matched, for a surface that lists documents rather than chunks: knowledge
// sources, or anything else built on Sources, which carries neither an ordinal nor
// a heading.
//
// Only the rule's own capture groups fill. ${ordinal}, ${heading} and ${anchor}
// each render empty, and a mapped citation left ending in a bare "#" has that "#"
// trimmed, so a rule written for chunks yields its document-level form here. A
// path no rule matches is returned as docPath itself, with false: no chunk is
// cited, so there is no citation token to fall back to.
//
// Every value substituted into a replacement is percent-encoded, as Render
// describes, with "/" left alone.
//
// This differs from Render, which cites one chunk and fills all three reserved
// names. A rule using ${ordinal} therefore renders differently on the two:
// knowledge match shows MatchedDoc.MappedCitation, which carries the first
// matching chunk's ordinal, where knowledge sources shows this. They agree on
// every rule that does not use ${ordinal}, and a rule that does is a poor one,
// since ordinals shift on every reindex.
func (m *CitationMapper) RenderDocument(docPath string) (string, bool) {
	citation, ok := m.render(docPath, citationReserved{})
	if !ok {
		return docPath, false
	}

	return citation, true
}

// citationReserved carries what the reserved names render from for one expansion.
// Render fills both fields; RenderDocument leaves them empty, which renders
// ${ordinal}, ${heading} and ${anchor} as nothing.
type citationReserved struct {
	ordinal     string
	headingPath string
}

// render expands the first rule whose pattern matches docPath and reports whether
// one did. It leaves the unmatched case to the caller, because the two public
// renderers answer it differently: a chunk citation for Render, the bare path for
// RenderDocument.
//
// A nil mapper holds no rules and matches nothing, so a caller with no rules
// configured can pass nil rather than build one.
func (m *CitationMapper) render(docPath string, reserved citationReserved) (string, bool) {
	if m == nil {
		return "", false
	}

	for _, rule := range m.rules {
		if rule.PatternCompiled == nil {
			continue
		}

		match := rule.PatternCompiled.FindStringSubmatchIndex(docPath)
		if match == nil {
			continue
		}

		citation := expandCitation(rule, docPath, match, reserved)

		return strings.TrimSuffix(citation, "#"), true
	}

	return "", false
}

// expandCitation renders one rule's replacement against a match on the document
// path. FindStringSubmatchIndex and this expander take the place of
// ReplaceAllString, which keeps the part of the path outside the match, so an
// unanchored rule on an absolute path yields /home/rip/https://docs.example.net/,
// substitutes every match rather than the first, and reports nothing about
// whether it matched at all.
//
// The walk is a single pass over the ranges RAGCitationRule.ReplaceRefs found:
// each reference resolves against the rule's own capture groups or the values
// supplied for the reserved names, and what was substituted is never rescanned. A
// second pass would let the corpus write the template, since a file named
// ${anchor}.md captured into the output would then have its own placeholder filled
// from the document's heading.
//
// The text between references is copied out as the operator wrote it, except for
// $$, which the scanner passed over and which is written here as one dollar. The
// second dollar of a pair cannot begin a reference: the scanner consumed both.
func expandCitation(rule config.RAGCitationRule, path string, match []int, reserved citationReserved) string {
	var out strings.Builder
	out.Grow(len(rule.Replace))

	pos := 0
	for _, ref := range rule.ReplaceRefs() {
		out.WriteString(strings.ReplaceAll(rule.Replace[pos:ref.Start], "$$", "$"))
		out.WriteString(escapeCitationValue(citationValue(rule.PatternCompiled, path, match, ref.Name, ref.Num, reserved)))
		pos = ref.End
	}

	out.WriteString(strings.ReplaceAll(rule.Replace[pos:], "$$", "$"))

	return out.String()
}

// citationValue resolves one reference to the text it stands for. A numbered or
// named capture group of the rule's own pattern wins over a reserved name, which
// is the order config validates in, and a group that took part in no match
// renders empty just as Expand renders it. A reserved name the caller could not
// supply, such as ${heading} for a chunk with no heading, also renders empty.
//
// The three reserved names are the ones config accepts a replacement for; the list
// config validates against is ragCitationReservedNames and the two must agree.
func citationValue(re *regexp.Regexp, path string, match []int, name string, num int, reserved citationReserved) string {
	if num >= 0 {
		if 2*num+1 < len(match) && match[2*num] >= 0 {
			return path[match[2*num]:match[2*num+1]]
		}

		return ""
	}

	group := false
	for i, sub := range re.SubexpNames() {
		if sub != name {
			continue
		}

		group = true
		if 2*i+1 < len(match) && match[2*i] >= 0 {
			return path[match[2*i]:match[2*i+1]]
		}
	}
	if group {
		return ""
	}

	switch name {
	case "ordinal":
		return reserved.ordinal
	case "heading":
		return citationHeading(reserved.headingPath)
	case "anchor":
		return citationAnchor(citationHeading(reserved.headingPath))
	}

	return ""
}

const citationHex = "0123456789ABCDEF"

// escapeCitationValue percent-encodes one substituted value for a URL path,
// leaving "/" alone because a capture routinely spans directories.
//
// Everything outside the unreserved set is encoded, which is stricter than
// url.PathEscape: that leaves "@" and ":" as they are, so a directory named
// a@evil.example substituted into https://$1.docs.example.net/ would render a URL
// whose host is evil.example. It also covers a heading holding a space, an "&" or
// a "?".
//
// The guarantee stops at "/". A capture spanning a directory boundary placed in
// the authority of a rule still moves the host: with ^(.*)/docs/.*\.md$ and
// https://$1.docs.example.net/, a stored path beginning evil.example/ renders a
// URL whose host is evil.example. A rule that puts a capture anywhere but the
// path is the operator's to get right.
func escapeCitationValue(value string) string {
	var out strings.Builder
	out.Grow(len(value))

	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			out.WriteByte(c)
		case c == '-', c == '_', c == '.', c == '~', c == '/':
			out.WriteByte(c)
		default:
			out.WriteByte('%')
			out.WriteByte(citationHex[c>>4])
			out.WriteByte(citationHex[c&0x0f])
		}
	}

	return out.String()
}

// citationHeading returns the deepest crumb of a breadcrumb, which is the heading
// the chunk sits under.
func citationHeading(headingPath string) string {
	i := strings.LastIndex(headingPath, crumbSeparator)
	if i < 0 {
		return headingPath
	}

	return headingPath[i+len(crumbSeparator):]
}

// citationAnchor slugs a heading the way github-slugger does, which is the
// fragment GitHub, Hugo and Docusaurus all generate for a heading: lowercase,
// delete anything that is not a letter, digit, underscore, space or hyphen, turn
// each space into a hyphen, and trim hyphens from both ends.
//
// It deletes rather than collapses because those slugs do: "Don't Panic" is
// dont-panic there and don-t-panic under a collapsing rule, and a fragment that
// names no heading fails silently in a browser.
func citationAnchor(heading string) string {
	var out strings.Builder
	out.Grow(len(heading))

	for _, r := range strings.ToLower(heading) {
		switch {
		case r == ' ':
			out.WriteRune('-')
		case r == '-', r == '_', unicode.IsLetter(r), unicode.IsDigit(r):
			out.WriteRune(r)
		}
	}

	return strings.Trim(out.String(), "-")
}
