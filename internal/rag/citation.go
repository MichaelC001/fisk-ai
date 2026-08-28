//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package rag

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/choria-io/fisk-ai/config"
)

// CitationMapper rewrites the document path in a knowledge citation into the
// address a reader can reach, using the ordered rules an operator writes under
// harness.knowledge.citations.
//
// A mapper holding no rules renders every citation as the raw
// <relpath>#<ordinal> token, so a caller never needs to know whether an operator
// configured any. That is the default: most corpora are not published.
type CitationMapper struct {
	rules []config.RAGCitationRule
}

// NewCitationMapper returns a mapper over rules, which are tried in the order
// given. Pass what Config.RAGCitationRules returns: it hands back rules whose
// pattern is compiled and whose replacement config has already checked against
// that pattern's groups, and nil when there is nothing configured.
//
// A nil or empty slice yields a mapper that passes every citation through, and so
// does a rule carrying no compiled pattern, which is skipped.
func NewCitationMapper(rules []config.RAGCitationRule) *CitationMapper {
	return &CitationMapper{rules: rules}
}

// Render returns the address for one cited chunk and whether a rule matched.
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
// An address left ending in a bare "#", which is what ${heading} or ${anchor}
// does for a chunk with no heading, has that "#" trimmed. ${ordinal} always has
// a value and never reaches the trim. A replacement writing a
// literal between the "#" and an empty value, such as "#section-${ordinal}",
// renders "#section-" and is the operator's to get right.
func (m *CitationMapper) Render(docPath string, ordinal int, headingPath string) (string, bool) {
	for _, rule := range m.rules {
		if rule.PatternCompiled == nil {
			continue
		}

		match := rule.PatternCompiled.FindStringSubmatchIndex(docPath)
		if match == nil {
			continue
		}

		address := expandCitation(rule.PatternCompiled, rule.Replace, docPath, match, ordinal, headingPath)

		return strings.TrimSuffix(address, "#"), true
	}

	return Citation(docPath, ordinal), false
}

// expandCitation renders one rule's replacement against a match on the document
// path. FindStringSubmatchIndex and this expander take the place of
// ReplaceAllString, which keeps the part of the path outside the match, so an
// unanchored rule on an absolute path yields /home/rip/https://docs.example.net/,
// substitutes every match rather than the first, and reports nothing about
// whether it matched at all.
//
// The walk is a single pass. Each reference resolves against the rule's own
// capture groups or the values supplied for the reserved names, and what was
// substituted is never rescanned. A second pass would let the corpus write the
// template, since a file named ${anchor}.md captured into the output would then
// have its own placeholder filled from the document's heading.
//
// The grammar is the one regexp.Regexp.Expand reads and config validates the
// replacement against, transcribed so that every reference config accepted
// resolves to the same group here: $$ is a literal dollar, a name is a run of
// Unicode letters, digits and underscores that may be braced, a $ that starts no
// name and an unterminated ${ stay literal, and an all-digit name with no leading
// zero and under the 1e8 cap is a numbered group while $01, $1x and a ten-digit
// run are named ones.
func expandCitation(re *regexp.Regexp, template string, path string, match []int, ordinal int, headingPath string) string {
	var out strings.Builder
	out.Grow(len(template))

	for len(template) > 0 {
		before, after, ok := strings.Cut(template, "$")
		if !ok {
			break
		}
		out.WriteString(before)
		template = after

		if template != "" && template[0] == '$' {
			out.WriteByte('$')
			template = template[1:]

			continue
		}

		name, num, rest, ok := citationExtract(template)
		if !ok {
			out.WriteByte('$')

			continue
		}
		template = rest

		out.WriteString(escapeCitationValue(citationValue(re, path, match, name, num, ordinal, headingPath)))
	}

	out.WriteString(template)

	return out.String()
}

// citationValue resolves one reference to the text it stands for. A numbered or
// named capture group of the rule's own pattern wins over a reserved name, which
// is the order config validates in, and a group that took part in no match
// renders empty just as Expand renders it. A reserved name the caller could not
// supply, such as ${heading} for a chunk with no heading, also renders empty.
func citationValue(re *regexp.Regexp, path string, match []int, name string, num int, ordinal int, headingPath string) string {
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
		return strconv.Itoa(ordinal)
	case "heading":
		return citationHeading(headingPath)
	case "anchor":
		return citationAnchor(citationHeading(headingPath))
	}

	return ""
}

// citationExtract reads a leading "name" or "{name}" from str, which the caller
// has already stripped the $ from. It is the same read as config's validation
// scanner and as the unexported extract in the regexp package, so a replacement
// that loaded resolves the references it was validated for.
func citationExtract(str string) (name string, num int, rest string, ok bool) {
	if str == "" {
		return
	}

	brace := false
	if str[0] == '{' {
		brace = true
		str = str[1:]
	}

	i := 0
	for i < len(str) {
		r, size := utf8.DecodeRuneInString(str[i:])
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' {
			break
		}
		i += size
	}
	if i == 0 {
		return
	}

	name = str[:i]
	if brace {
		if i >= len(str) || str[i] != '}' {
			return
		}
		i++
	}

	num = 0
	for j := 0; j < len(name); j++ {
		if name[j] < '0' || '9' < name[j] || num >= 1e8 {
			num = -1
			break
		}
		num = num*10 + int(name[j]) - '0'
	}
	if name[0] == '0' && len(name) > 1 {
		num = -1
	}

	rest = str[i:]
	ok = true

	return
}

// citationHex is the alphabet escapeCitationValue writes a percent escape with.
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
// Leaving "/" alone is what that guarantee stops at. A capture spanning a
// directory boundary placed in the authority of a rule still moves the host: with
// ^(.*)/docs/.*\.md$ and https://$1.docs.example.net/, a stored path beginning
// evil.example/ renders a URL whose host is evil.example. A rule that puts a
// capture anywhere but the path is the operator's to get right.
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
// the chunk sits under. A breadcrumb with one crumb is that crumb, and an empty
// one is empty.
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
// Deleting rather than collapsing is the whole point of matching them: "Don't
// Panic" is dont-panic there and don-t-panic under a collapsing rule, and a
// fragment that names no heading fails silently in a browser.
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
