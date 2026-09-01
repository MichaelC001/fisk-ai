//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package rag

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The enumerate syntax is small and closed on purpose. Every form here composes in
// Go over document sets rather than inside one MATCH expression, because FTS5
// booleans evaluate within a single row, which is one chunk: a document with
// "retention" in one chunk and "policy" in the next satisfies neither
// '"retention" AND "policy"' nor any other single-row expression, and would be
// reported absent by a command whose whole claim is completeness.
//
//	foo bar        both terms, same document
//	"foo bar"      adjacent stems, same chunk
//	-foo           exclude
//	heading:foo    the section breadcrumb only
//	body:foo       the body only
//
// What is absent is absent deliberately. OR, AND, NOT and NEAR are rejected by
// name rather than passed through, because FTS5 would accept them as operators and
// silently answer a different question than the one typed. So is '*': prefix
// matching against a stemmed index is not monotonic, so it returns fewer results
// for a longer prefix, which no completeness claim survives.
const (
	// enumFieldBody and enumFieldHeading are the field prefixes a query may scope a
	// term with, mapped to the FTS5 columns they name. An unknown field is a compile
	// error rather than a passthrough, which would surface as "no such column"
	// quoting the user's own text back at them.
	enumFieldBody    = "body"
	enumFieldHeading = "heading"

	// enumColumnBody and enumColumnHeading are the real column names. heading: is
	// spelled shorter than the column it compiles to, so the mapping is not identity
	// and cannot be skipped.
	enumColumnBody    = "body"
	enumColumnHeading = "heading_path"
)

// reservedOperators are rejected by name. Case-insensitively: FTS5 only treats
// them as operators in upper case, but a lower-case "and" typed by a user means
// the same thing to them and would silently become a search term.
var reservedOperators = map[string]string{
	"and": "terms are already required together, so write them side by side: deprecated api",
	"or":  "alternatives are not supported; run one query per term and compare",
	"not": "use a leading minus to exclude: -deprecated",
	"near": "proximity is not supported; a quoted phrase requires the words adjacent " +
		`in one section: "retention policy"`,
}

// enumTerm is one compiled term: the text handed to FTS5 and the column it is
// scoped to, if any. Phrase records whether the surface came from a quoted phrase,
// which changes only how it is described back to the reader.
type enumTerm struct {
	Surface string // as typed, without the field prefix or leading minus
	Column  string // "" for either column, else a real column name
	Phrase  bool
	Negated bool
}

// match renders the term as an FTS5 MATCH fragment. Every token is double-quoted
// with internal quotes doubled, so the only unquoted characters this package ever
// emits into MATCH are its own operators. A column filter is the one exception and
// is never user text: it is one of two constants chosen by the compiler.
func (t enumTerm) match() string {
	quoted := `"` + strings.ReplaceAll(t.Surface, `"`, `""`) + `"`
	if t.Column == "" {
		return quoted
	}

	return t.Column + ":" + quoted
}

// String renders the term the way the compiled line shows it to a reader.
func (t enumTerm) String() string {
	var b strings.Builder
	if t.Negated {
		b.WriteString("-")
	}
	switch t.Column {
	case enumColumnBody:
		b.WriteString(enumFieldBody + ":")
	case enumColumnHeading:
		b.WriteString(enumFieldHeading + ":")
	}
	b.WriteString(`"` + t.Surface + `"`)

	return b.String()
}

// enumQuery is a parsed enumerate query. Positive terms intersect, negative terms
// subtract, and Dropped names what was thrown away before either happened, since a
// term silently discarded would make a complete answer a lie about a different
// question.
type enumQuery struct {
	Positive []enumTerm
	Negative []enumTerm
	Dropped  []string
}

// Compiled renders the expression that was actually run, for the compiled line and
// the tool's JSON. It shows composition as AND because that is what intersecting
// document sets means to a reader, while being explicit that it is not one MATCH.
func (q *enumQuery) Compiled() string {
	parts := make([]string, 0, len(q.Positive)+len(q.Negative))
	for _, t := range q.Positive {
		parts = append(parts, t.String())
	}
	for _, t := range q.Negative {
		parts = append(parts, t.String())
	}

	return strings.Join(parts, " AND ")
}

// compileEnumerateQuery parses the enumerate syntax into terms to intersect and
// subtract. It reports a usable error for every rejected form rather than letting
// FTS5 answer a different question or quote the user's text back inside its own
// parser's wording.
func compileEnumerateQuery(query string) (*enumQuery, error) {
	tokens, err := splitEnumerateQuery(query)
	if err != nil {
		return nil, err
	}

	out := &enumQuery{}
	for _, tok := range tokens {
		term, dropped, err := compileEnumerateToken(tok)
		if err != nil {
			return nil, err
		}
		switch {
		case dropped != "":
			out.Dropped = append(out.Dropped, dropped)
		case term.Negated:
			out.Negative = append(out.Negative, term)
		default:
			out.Positive = append(out.Positive, term)
		}

		if len(out.Positive)+len(out.Negative) >= maxFTSTerms {
			break
		}
	}

	// FTS5 has no unary negation: 'NOT "x"' is a syntax error and '-"x"' resolves as
	// a column reference, so there is nothing to subtract from. The set to subtract
	// from would be the whole corpus, which is a different command.
	if len(out.Positive) == 0 && len(out.Negative) > 0 {
		return nil, fmt.Errorf("a query of only exclusions has nothing to exclude from; add a term to match, as in: api -deprecated")
	}

	return out, nil
}

// compileEnumerateToken turns one whitespace-delimited token into a term. It
// returns a non-empty dropped surface instead when the term is too short to query,
// which the caller reports rather than discarding.
func compileEnumerateToken(tok enumToken) (enumTerm, string, error) {
	term := enumTerm{Surface: tok.text, Phrase: tok.quoted}

	if !tok.quoted {
		if err := rejectUnsupported(tok.text); err != nil {
			return term, "", err
		}

		rest := tok.text
		if strings.HasPrefix(rest, "-") {
			term.Negated = true
			rest = rest[1:]
			if rest == "" {
				return term, "", fmt.Errorf("a bare '-' excludes nothing; write it against a term, as in: -deprecated")
			}
		}

		field, value, ok := strings.Cut(rest, ":")
		if ok {
			column, err := enumColumn(field)
			if err != nil {
				return term, "", err
			}
			if value == "" {
				return term, "", fmt.Errorf("%q names a field but no term; write it as %s:<word>", rest, field)
			}
			term.Column = column
			rest = value
		}

		term.Surface = rest
	}

	if err := rejectUnsupported(term.Surface); err != nil {
		return term, "", err
	}

	// The same two-rune floor the ranked search uses. Reported rather than dropped
	// silently: under a completeness contract, a term that was never queried has to
	// be named or the answer is complete about a question nobody asked.
	if !tok.quoted && utf8.RuneCountInString(term.Surface) < MinTermRunes {
		return term, term.Surface, nil
	}

	return term, "", nil
}

// enumColumn maps a field prefix to the column it names.
func enumColumn(field string) (string, error) {
	switch strings.ToLower(field) {
	case enumFieldBody:
		return enumColumnBody, nil
	case enumFieldHeading:
		return enumColumnHeading, nil
	}

	return "", fmt.Errorf("unknown field %q; %s: and %s: are the only fields this command knows", field, enumFieldBody, enumFieldHeading)
}

// rejectUnsupported refuses the forms FTS5 would otherwise accept and answer
// differently from what was meant.
func rejectUnsupported(text string) error {
	if text == "" {
		return nil
	}

	// Measured: "OR" survives the two-rune floor, so an unguarded compiler turns
	// 'foo OR bar' into 'foo AND or AND bar' and intersects with every document
	// containing the word "or". That is a wrong answer rather than an empty one,
	// from a command whose contract is completeness.
	if fix, ok := reservedOperators[strings.ToLower(text)]; ok {
		return fmt.Errorf("%q is not an operator here; %s", text, fix)
	}

	// A bare '*' and a leading '*foo' produce FTS5's own "unknown special query"
	// wording, which quotes the user's text back in a framing that means nothing to
	// them, and 'foo*bar' does not error at all: it parses as 'foo*' AND 'bar' and
	// returns a wrong non-empty set.
	if strings.Contains(text, "*") {
		return fmt.Errorf("%q uses '*', which this command does not support; matching is by word stem already, so deprecated finds deprecate and deprecation", text)
	}

	return nil
}

// enumToken is one lexed token: its text, and whether it was quoted, which decides
// whether it is a phrase and whether the field and exclusion prefixes apply.
type enumToken struct {
	text   string
	quoted bool
}

// splitEnumerateQuery lexes the query into whitespace-delimited tokens, keeping a
// double-quoted run together as one token. Inside a quoted run, a doubled quote is
// one literal quote, which is FTS5's own escaping rule and the reason the emission
// side has to double it back: a phrase can legitimately contain the character that
// delimits it. An unterminated quote is an error rather than an implied close,
// since the two readings return different sets and the reader cannot tell which
// they got.
func splitEnumerateQuery(query string) ([]enumToken, error) {
	var (
		out     []enumToken
		current strings.Builder
		inQuote bool
	)

	flush := func(quoted bool) {
		text := current.String()
		current.Reset()
		if !quoted {
			text = strings.TrimSpace(text)
		}
		if text != "" {
			out = append(out, enumToken{text: text, quoted: quoted})
		}
	}

	runes := []rune(query)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case r == '"' && inQuote:
			if i+1 < len(runes) && runes[i+1] == '"' {
				current.WriteRune('"')
				i++
				continue
			}
			flush(true)
			inQuote = false

		case r == '"':
			flush(false)
			inQuote = true

		case unicode.IsSpace(r) && !inQuote:
			flush(false)

		default:
			current.WriteRune(r)
		}
	}

	if inQuote {
		return nil, fmt.Errorf(`unbalanced quote in query; a phrase needs a closing quote, as in: "retention policy"`)
	}
	flush(false)

	return out, nil
}
