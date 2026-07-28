//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package rag

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// WordsSort selects the order words are returned in. It resolves before the limit
// applies, on the same terms as EnumerateSort: a truncated list has to be the top
// of an ordered set rather than an arbitrary subset of one.
type WordsSort string

const (
	// SortWordsAuto orders by document count when counts were computed and
	// alphabetically when they were not, which is what each of the two output shapes
	// wants. A long list is scanned, so it reads in alphabetical order; a short one is
	// compared, so it reads by frequency.
	SortWordsAuto WordsSort = ""

	// SortWordsByDocs orders by the as-written document count, most first.
	SortWordsByDocs WordsSort = "docs"

	// SortWordsByWord orders alphabetically.
	SortWordsByWord WordsSort = "word"

	// SortWordsByStem groups the forms of one word together, which is the view that
	// answers "what does this corpus actually call this thing".
	SortWordsByStem WordsSort = "stem"
)

// WordsOptions bounds one vocabulary listing.
type WordsOptions struct {
	// Pattern is an optional regular expression narrowing the vocabulary. It is
	// matched unanchored and case-insensitively against every word.
	Pattern string

	// Limit caps the words returned after sorting. Zero returns every match.
	Limit int

	// MinDocs and MaxDocs bound the as-written document count, inclusive. Zero
	// disables each. MaxDocs is what removes the words that appear everywhere, since
	// FTS5 keeps no stopword list and those words carry the largest counts.
	MinDocs int
	MaxDocs int

	// Field scopes the listing to one indexed column, using the same names the
	// enumerate query compiler accepts: "body" or "heading". Empty counts both.
	//
	// The vocabulary table spans both columns and cannot say which one a word came
	// from, so scoping is applied to the counts and a word with none in the chosen
	// column drops out. That makes it exact without a schema change, at the price of
	// forcing the counts to be computed.
	Field string

	Sort WordsSort

	// CountThreshold is the matched-word count at or below which per-word document
	// counts are computed. Counting costs two queries per word, so it is worth paying
	// for a set small enough to compare and not for a vocabulary dump that is only
	// going to be scanned. A bound on MinDocs or MaxDocs forces counting regardless,
	// since those filter on a number that would otherwise not exist.
	CountThreshold int
}

// Word is one word of the index vocabulary.
type Word struct {
	// Word is the word as the documents spell it, lowercased and with Latin-1
	// diacritics folded, because that is how unicode61 stored it.
	Word string

	// Stem is what the porter tokenizer reduces the word to, and therefore what the
	// stemmed index files it under.
	Stem string

	// AsWritten counts documents holding this exact word. AnyForm counts documents
	// holding any word sharing its stem, which is the number knowledge match reports
	// for it. Both are zero when Counted is false on the result.
	AsWritten int
	AnyForm   int

	// Queryable reports whether knowledge match will accept this word. A word below
	// the minimum term length, or one of the reserved operator names, is in the index
	// but cannot be asked about, so a count is stated for it that match would refuse
	// to produce.
	Queryable bool
}

// WordsResult is the answer to one vocabulary listing.
type WordsResult struct {
	Words []Word

	// Vocabulary is every distinct word in the index, before the pattern. Matched is
	// what survived the pattern, the field scope and the count bounds, before the
	// limit. PatternMatched is what survived the pattern alone, which is what lets an
	// empty result say whether the word is absent or was filtered out.
	Vocabulary     int
	PatternMatched int
	Matched        int
	Returned       int
	Truncated      bool

	IndexedDocuments int

	// Counted reports whether per-word document counts were computed. It selects the
	// output shape: a counted result is a table, an uncounted one is a word list.
	Counted bool

	// Scoped reports whether the counts cover one indexed column rather than both,
	// which the reader has to be told or the numbers look like a different corpus.
	Scoped bool

	// Status reuses the enumerate states because this command's are a subset of them:
	// there is no query to be empty, so only the index states can arise.
	Status EnumerateStatus
}

// Words lists the vocabulary of the index with document frequencies.
//
// It exists for the question a zero from Enumerate raises but cannot answer: when
// no document contains a word, the next thing to know is what the documents call it
// instead. The vocabulary is read from the unstemmed index, so it returns the words
// as written rather than the stems the ranked index files them under.
//
// Document counts come from one MATCH query per word rather than from the vocabulary
// table, whose doc column counts chunks. They are computed for the whole matched set
// before sorting and limiting, never for the displayed page alone, because filtering
// or ordering on one number while displaying another produces a list whose own
// column contradicts the flags that built it.
func (s *Store) Words(ctx context.Context, opts WordsOptions) (*WordsResult, error) {
	res := &WordsResult{Words: []Word{}, Status: EnumOK}

	if s.db == nil {
		res.Status = EnumIndexNotBuilt
		return res, nil
	}

	pattern, err := compileWordPattern(opts.Pattern)
	if err != nil {
		return nil, err
	}

	column, err := wordsColumn(opts.Field)
	if err != nil {
		return nil, err
	}

	res.IndexedDocuments, err = scanCount(ctx, s.db, `SELECT count(*) FROM documents`)
	if err != nil {
		return nil, fmt.Errorf("counting documents: %w", err)
	}
	if res.IndexedDocuments == 0 {
		res.Status = EnumCorpusEmpty
		return res, nil
	}

	surfaces, err := s.vocabulary(ctx)
	if err != nil {
		return nil, err
	}
	res.Vocabulary = len(surfaces)

	var candidates []string
	for _, w := range surfaces {
		if pattern == nil || pattern.MatchString(w) {
			candidates = append(candidates, w)
		}
	}

	res.PatternMatched = len(candidates)

	bounded := opts.MinDocs > 0 || opts.MaxDocs > 0
	res.Scoped = column != ""
	res.Counted = bounded || res.Scoped || len(candidates) <= opts.CountThreshold

	words, err := s.describeWords(ctx, candidates, res.Counted, column)
	if err != nil {
		return nil, err
	}

	// A word absent from the chosen column is not in that part of the corpus, so it
	// leaves the listing rather than sitting there with a zero.
	if res.Scoped {
		words = boundWords(words, 1, 0)
	}
	if bounded {
		words = boundWords(words, opts.MinDocs, opts.MaxDocs)
	}

	res.Matched = len(words)
	sortWords(words, opts.Sort, res.Counted)

	if opts.Limit > 0 && len(words) > opts.Limit {
		words = words[:opts.Limit]
		res.Truncated = true
	}

	res.Words = words
	res.Returned = len(words)

	return res, nil
}

// compileWordPattern compiles the narrowing pattern case-insensitively, which is
// not a convenience: unicode61 folds case at index time, so the stored vocabulary
// holds no uppercase at all and a case-sensitive pattern could only ever return
// nothing for anything an operator would naturally type.
func compileWordPattern(pattern string) (*regexp.Regexp, error) {
	if pattern == "" {
		return nil, nil
	}

	re, err := regexp.Compile("(?i)" + pattern)
	if err != nil {
		return nil, fmt.Errorf("%q is not a valid regular expression: %w. The argument is a pattern, so escape any special characters to match them literally", pattern, err)
	}

	return re, nil
}

// wordsColumn maps the operator's field name onto the real column, reusing the
// enumerate compiler's names so body: and heading: mean the same thing in both
// commands.
func wordsColumn(field string) (string, error) {
	switch field {
	case "":
		return "", nil
	case enumFieldBody:
		return enumColumnBody, nil
	case enumFieldHeading:
		return enumColumnHeading, nil
	}

	return "", fmt.Errorf("unknown field %q; %s and %s are the only fields this command knows", field, enumFieldBody, enumFieldHeading)
}

// vocabulary reads every distinct word from the unstemmed index.
//
// The whole list is read rather than filtered in SQL because the pattern is a Go
// regular expression: SQLite has no REGEXP of its own, and registering one through
// the driver would install it process-wide, including on the agent's connection.
// Narrowing the scan by a literal prefix taken from the pattern is not available
// either, since a prefix of a match is not a prefix of the subject under unanchored
// matching, and a range scan built from one silently drops every word that contains
// the pattern without starting with it.
func (s *Store) vocabulary(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT term FROM chunks_vocab ORDER BY term`)
	if err != nil {
		return nil, fmt.Errorf("reading index vocabulary: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var term string
		if err := rows.Scan(&term); err != nil {
			return nil, fmt.Errorf("reading index vocabulary: %w", err)
		}
		out = append(out, term)
	}

	return out, rows.Err()
}

// describeWords fills in the stem, the queryability and, when counted, both
// document counts. The counts are two queries per word, which is why the caller
// decides whether they are worth paying for.
func (s *Store) describeWords(ctx context.Context, surfaces []string, counted bool, column string) ([]Word, error) {
	if len(surfaces) == 0 {
		return []Word{}, nil
	}

	// The stem is only shown beside the counts, so a vocabulary dump does not pay to
	// tokenize thousands of words for a column it will not print.
	var stems map[string]string
	if counted {
		stems = stemSurfaces(ctx, surfaces)
	}

	out := make([]Word, 0, len(surfaces))
	for _, surface := range surfaces {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		w := Word{Word: surface, Stem: stems[surface], Queryable: wordIsQueryable(surface)}
		if counted {
			match := enumTerm{Surface: surface, Column: column}.match()

			if err := s.countDocumentsMatching(ctx, ftsTableExact, match, &w.AsWritten); err != nil {
				return nil, err
			}
			if err := s.countDocumentsMatching(ctx, ftsTablePorter, match, &w.AnyForm); err != nil {
				return nil, err
			}
		}

		out = append(out, w)
	}

	return out, nil
}

// countDocumentsMatching counts the distinct documents with at least one chunk
// matching the expression in the named table. It counts in SQLite rather than
// materializing the id set the way documentsMatching does, because the caller wants
// only the size and a common word matches most of the corpus.
func (s *Store) countDocumentsMatching(ctx context.Context, table, match string, into *int) error {
	q := fmt.Sprintf(`SELECT count(DISTINCT c.document_id) FROM %s f JOIN chunks c ON c.id = f.rowid WHERE %[1]s MATCH ?`, table)

	n, err := scanCount(ctx, s.db, q, match)
	if err != nil {
		return fmt.Errorf("counting documents for a word: %w", err)
	}
	*into = n

	return nil
}

// wordIsQueryable reports whether knowledge match will accept the word. The
// vocabulary holds words that command refuses: anything below the minimum term
// length, and the reserved operator names, which are ordinary words in a document
// and near the top of any frequency listing.
func wordIsQueryable(word string) bool {
	if utf8.RuneCountInString(word) < MinTermRunes {
		return false
	}

	_, reserved := reservedOperators[strings.ToLower(word)]

	return !reserved
}

// boundWords applies the inclusive as-written document bounds.
func boundWords(words []Word, minDocs, maxDocs int) []Word {
	out := make([]Word, 0, len(words))
	for _, w := range words {
		if minDocs > 0 && w.AsWritten < minDocs {
			continue
		}
		if maxDocs > 0 && w.AsWritten > maxDocs {
			continue
		}
		out = append(out, w)
	}

	return out
}

// sortWords orders the list before the limit applies. The automatic order follows
// the output shape rather than a fixed preference: a counted result is read as a
// comparison and wants frequency, an uncounted one is scanned for a word and wants
// the alphabet.
func sortWords(words []Word, order WordsSort, counted bool) {
	if order == SortWordsAuto {
		order = SortWordsByWord
		if counted {
			order = SortWordsByDocs
		}
	}

	switch order {
	case SortWordsByDocs:
		sort.SliceStable(words, func(i, j int) bool {
			if words[i].AsWritten != words[j].AsWritten {
				return words[i].AsWritten > words[j].AsWritten
			}
			return words[i].Word < words[j].Word
		})

	case SortWordsByStem:
		sort.SliceStable(words, func(i, j int) bool {
			if words[i].Stem != words[j].Stem {
				return words[i].Stem < words[j].Stem
			}
			return words[i].Word < words[j].Word
		})

	default:
		sort.SliceStable(words, func(i, j int) bool { return words[i].Word < words[j].Word })
	}
}
