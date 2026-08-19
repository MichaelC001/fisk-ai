//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/choria-io/fisk"
	"github.com/choria-io/ui/columns"
	"github.com/choria-io/ui/table"

	"github.com/choria-io/fisk-ai/internal/rag"
	"github.com/choria-io/fisk-ai/internal/util"
)

const (
	// wordsScreenLimit caps a listing that was not asked to be longer. A vocabulary
	// runs to thousands of words, and nobody reads a five thousand line scroll.
	wordsScreenLimit = 1000

	// wordsTableLimit is the matched-word count at or below which the counted table
	// is rendered instead of the word list. It matches the screenful matchScreenLimit
	// is set to, for the same reason: past it the reader is scanning, not comparing.
	wordsTableLimit = matchScreenLimit

	// wordsColumns is how many words sit side by side in the list form. Three keeps a
	// long vocabulary to a third of the scroll while leaving room for the longest
	// word an elision allows.
	wordsColumns = 3

	// wordsMaxRunes bounds a displayed word. A token is any run of alphanumerics, so
	// one base64 blob or minified line in an indexed document is a single enormous
	// word that would otherwise destroy the column layout.
	wordsMaxRunes = 15

	// wordsSanitizeRunes is the hard bound applied before the display elision, so
	// sanitizing never truncates a word the elision is about to shorten anyway.
	wordsSanitizeRunes = 256
)

var (
	knowledgeWordsPattern    string
	knowledgeWordsLimit      int
	knowledgeWordsMinDocs    int
	knowledgeWordsMaxDocs    int
	knowledgeWordsField      string
	knowledgeWordsSort       string
	knowledgeWordsOnly       bool
	knowledgeWordsCount      bool
	knowledgeWordsExitCode   bool
	knowledgeWordsPatternSet bool
	knowledgeWordsLimitSet   bool
)

// registerKnowledgeWordsCommand adds the vocabulary verb. It is spelled words
// rather than terms because this package already uses "term" for a word in a query,
// in TermReport, in the minimum term length, and in the term frequency table that
// knowledge match prints. Both other spellings stay reachable as aliases.
func registerKnowledgeWordsCommand(k *fisk.CmdClause) {
	words := k.Command("words", "Lists the words the indexed documents actually use, with document counts").
		Alias("vocab").
		Alias("terms").
		Action(knowledgeWordsAction)

	words.Arg("pattern", "Only list words matching this regular expression; matches anywhere in a word unless anchored with ^ or $").
		IsSetByUser(&knowledgeWordsPatternSet).
		StringVar(&knowledgeWordsPattern)

	words.Flag("limit", "Maximum words to list").
		Default(fmt.Sprintf("%d", wordsScreenLimit)).
		IsSetByUser(&knowledgeWordsLimitSet).
		IntVar(&knowledgeWordsLimit)
	words.Flag("min-docs", "Only words appearing in at least this many documents").IntVar(&knowledgeWordsMinDocs)
	words.Flag("max-docs", "Only words appearing in at most this many documents, which is what removes the words that appear everywhere").IntVar(&knowledgeWordsMaxDocs)
	words.Flag("field", "Only count occurrences in one part of a section: body or heading").EnumVar(&knowledgeWordsField, "body", "heading")
	words.Flag("sort", "Order words by docs, word or stem").EnumVar(&knowledgeWordsSort, "docs", "word", "stem")
	words.Flag("words-only", "Print bare words, one per line, for piping").UnNegatableBoolVar(&knowledgeWordsOnly)
	words.Flag("count", "Print the number of matching words and nothing else").UnNegatableBoolVar(&knowledgeWordsCount)
	words.Flag("exit-code", "Exit 1 when no word matched").UnNegatableBoolVar(&knowledgeWordsExitCode)
}

func knowledgeWordsAction(pc *fisk.ParseContext) error {
	matched, err := runKnowledgeWords(pc)
	if err != nil {
		return err
	}

	if knowledgeWordsExitCode && matched == 0 {
		os.Exit(1)
	}

	return nil
}

func runKnowledgeWords(_ *fisk.ParseContext) (int, error) {
	ctx, cancel := interruptContext()
	defer cancel()

	if err := validateWordsFlags(); err != nil {
		return 0, err
	}

	cfg, err := knowledgeConfig()
	if err != nil {
		return 0, err
	}

	store, err := rag.Open(cfg, knowledgeStoreDir)
	if err != nil {
		return 0, err
	}
	defer store.Close()

	res, err := store.Words(ctx, rag.WordsOptions{
		Pattern:        knowledgeWordsPattern,
		Limit:          knowledgeWordsLimit,
		MinDocs:        knowledgeWordsMinDocs,
		MaxDocs:        knowledgeWordsMaxDocs,
		Field:          knowledgeWordsField,
		Sort:           rag.WordsSort(knowledgeWordsSort),
		CountThreshold: wordsTableLimit,
	})
	if err != nil {
		return 0, err
	}

	// A pipe must never be able to read an unbuilt index as an empty vocabulary, so
	// the machine modes fail rather than print nothing.
	if res.Status != rag.EnumOK {
		if knowledgeWordsOnly || knowledgeWordsCount {
			return 0, wordsStatusError(res.Status)
		}
	}

	if knowledgeWordsCount {
		fmt.Println(res.Matched)
		return res.Matched, nil
	}

	if knowledgeWordsOnly {
		for _, w := range res.Words {
			fmt.Println(w.Word)
		}
		return res.Matched, nil
	}

	c := columns.New()
	defer c.WriteTo(os.Stdout)

	return res.Matched, renderWords(c, res)
}

// validateWordsFlags refuses combinations that contradict each other, rather than
// letting one silently win, on the same terms as validateMatchFlags.
func validateWordsFlags() error {
	if knowledgeWordsLimit < 0 {
		return fmt.Errorf("--limit cannot be negative")
	}
	if knowledgeWordsMinDocs < 0 || knowledgeWordsMaxDocs < 0 {
		return fmt.Errorf("--min-docs and --max-docs cannot be negative")
	}
	if knowledgeWordsMinDocs > 0 && knowledgeWordsMaxDocs > 0 && knowledgeWordsMinDocs > knowledgeWordsMaxDocs {
		return fmt.Errorf("--min-docs %d is above --max-docs %d, so no word can satisfy both", knowledgeWordsMinDocs, knowledgeWordsMaxDocs)
	}
	if knowledgeWordsCount && knowledgeWordsOnly {
		return fmt.Errorf("--count and --words-only both own the output; pick one")
	}

	return nil
}

// wordsStatusError turns an index state into the error the machine modes fail with.
func wordsStatusError(status rag.EnumerateStatus) error {
	if status == rag.EnumIndexNotBuilt {
		return fmt.Errorf("the knowledge index has not been built; run: fisk knowledge index")
	}

	return fmt.Errorf("the knowledge index has 0 documents; run: fisk knowledge index")
}

// renderWords adds the whole listing to c.
func renderWords(c *columns.Document, res *rag.WordsResult) error {
	c.Heading(rag.EnumerateTierLine)

	if res.Status != rag.EnumOK {
		c.Print(wordsStatusError(res.Status).Error())
		return nil
	}

	c.ItemUnlessZero("Pattern", knowledgeWordsPattern)
	c.ItemUnlessZero("Field", knowledgeWordsField)
	c.Item("Vocabulary", fmt.Sprintf("%d distinct %s across %d indexed %s",
		res.Vocabulary, plural(res.Vocabulary, "word", "words"),
		res.IndexedDocuments, plural(res.IndexedDocuments, "document", "documents")))
	c.Item("Matched", fmt.Sprintf("%d %s", res.Matched, plural(res.Matched, "word", "words")))
	c.Blank()

	if res.Matched == 0 {
		renderNoWords(c, res)
		return nil
	}

	if res.Counted {
		c.Embed(wordsTable(res))
	} else {
		c.Embed(wordsList(res))
	}

	renderWordsNotes(c, res)

	return nil
}

// wordsTable is the counted form, shown when the set is small enough to compare.
// The column names and their order match the term frequency table knowledge match
// prints, so the same word reads identically in both rather than appearing to carry
// two different document counts.
func wordsTable(res *rag.WordsResult) *table.Table {
	tbl := table.NewTableWriter("")
	tbl.AddHeaders("Word", "Stem", "As written", "Any form")

	for _, w := range res.Words {
		anyForm := fmt.Sprintf("%d", w.AnyForm)
		if !w.Queryable {
			anyForm = fmt.Sprintf("%d (not queryable)", w.AnyForm)
		}
		tbl.AddRow(displayWord(w.Word), w.Stem, w.AsWritten, anyForm)
	}

	return tbl
}

// wordsList is the scanning form: bare words several to a line, because a long
// vocabulary is read looking for one word rather than compared row by row.
func wordsList(res *rag.WordsResult) *table.Table {
	tbl := table.NewTableWriter("")

	row := make([]any, 0, wordsColumns)
	for _, w := range res.Words {
		row = append(row, displayWord(w.Word))
		if len(row) == wordsColumns {
			tbl.AddRow(row...)
			row = row[:0]
		}
	}
	if len(row) > 0 {
		for len(row) < wordsColumns {
			row = append(row, "")
		}
		tbl.AddRow(row...)
	}

	return tbl
}

// renderNoWords explains an empty listing, distinguishing a word the documents do
// not use from one the flags removed. Without that split the command reproduces the
// confusion the whole feature exists to end.
func renderNoWords(c *columns.Document, res *rag.WordsResult) {
	// A word the pattern found and a filter then removed is not an absent word, and
	// the two must never render the same. Both branches name the flag that did it and
	// prove the words exist by counting them.
	if res.PatternMatched > 0 && res.Scoped {
		c.Printf("The pattern found %d %s, none of them in the %s of any section.",
			res.PatternMatched, plural(res.PatternMatched, "word", "words"), knowledgeWordsField)
		c.Print("Drop --field to see where they do appear.")
		return
	}

	if res.PatternMatched > 0 {
		c.Printf("The pattern found %d %s, and the document bounds excluded all of them.",
			res.PatternMatched, plural(res.PatternMatched, "word", "words"))
		c.Print("Drop --min-docs and --max-docs to see them.")
		return
	}

	if knowledgeWordsMinDocs > 0 || knowledgeWordsMaxDocs > 0 {
		c.Print("No word matched, and the document bounds took part, so this may be a filter rather than an absence.")
		c.Print("Drop --min-docs and --max-docs to see whether the word is there at all.")
		return
	}

	if knowledgeWordsPattern != "" {
		c.Printf("No word in the index matches %q. That is the whole vocabulary checked, not a sample.", knowledgeWordsPattern)
		c.Blank()
		c.Print("The vocabulary is lowercase with Latin-1 diacritics folded, so an accented spelling is stored without it.")
		c.Print("Try fewer letters, since the pattern matches anywhere in a word.")
		return
	}

	c.Print("The index holds no words.")
}

// renderWordsNotes says only what the numbers do not. Each note fires on a
// condition rather than on every run, so a reader does not learn to skip them.
func renderWordsNotes(c *columns.Document, res *rag.WordsResult) {
	var notes []string

	if res.Truncated {
		notes = append(notes, fmt.Sprintf("Showing %d of %d matching words. Raise --limit or narrow with a pattern.", res.Returned, res.Matched))
	}

	// Fired only on the bare form, which is the one whose author has not yet met the
	// pattern or the bounds. Detected from whether the flags were given, so an
	// explicit --limit equal to the default does not count as silence.
	if !knowledgeWordsPatternSet && !knowledgeWordsLimitSet && knowledgeWordsMinDocs == 0 && knowledgeWordsMaxDocs == 0 {
		notes = append(notes, "Narrow this with a pattern argument, or with --min-docs and --max-docs; --max-docs is what drops the words that appear in nearly every document.")
	}

	if res.Counted {
		notes = append(notes, "As written counts documents holding the word exactly; Any form counts documents holding any word with the same stem, which is what knowledge match reports.")
	}

	// Without this the counts read as a different corpus, since a scoped listing both
	// drops words and shrinks the numbers of the words it keeps.
	if res.Scoped {
		notes = append(notes, fmt.Sprintf("Both counts cover the %s only, and a word appearing nowhere in it is not listed. Unscoped, the vocabulary spans section bodies and heading breadcrumbs together.", knowledgeWordsField))
	} else if res.Counted {
		notes = append(notes, "The vocabulary spans section bodies and heading breadcrumbs together; use --field to count one of them.")
	}

	if wordsHasUnqueryable(res) {
		notes = append(notes, fmt.Sprintf("A word marked not queryable is in the index but knowledge match will not take it: it is under %d characters or is a reserved word.", rag.MinTermRunes))
	}

	if len(notes) == 0 {
		return
	}

	c.Blank()
	for _, n := range notes {
		c.Print(n)
	}
}

func wordsHasUnqueryable(res *rag.WordsResult) bool {
	if !res.Counted {
		return false
	}

	for _, w := range res.Words {
		if !w.Queryable {
			return true
		}
	}

	return false
}

// displayWord bounds a word for the screen. It sanitizes on the same unconditional
// rule the rest of the CLI follows, even though the tokenizer emits alphanumerics
// only and so cannot currently produce an escape sequence: the rule is what keeps a
// later tokenizer change from quietly reopening it.
func displayWord(word string) string {
	// Sanitized well above the display width, so the elision below is what bounds
	// the column and the two do not both try to shorten the same string.
	word = util.SanitizeForTerminal(word, wordsSanitizeRunes)
	if utf8.RuneCountInString(word) <= wordsMaxRunes {
		return word
	}

	// Head and tail rather than a plain cut, because the words this fires on are
	// machine noise whose ends are what distinguish one from the next.
	const ellipsis = "..."
	keep := wordsMaxRunes - len(ellipsis)
	runes := []rune(word)
	head := (keep + 1) / 2

	return string(runes[:head]) + ellipsis + string(runes[len(runes)-(keep-head):])
}

// wordsSuggestion offers the vocabulary listing to someone whose match returned
// nothing, which is the moment the question "what do the documents call it" is
// actually being asked. The prefix is short deliberately: the word they typed is
// not in the index, so its opening letters are the only part worth keeping.
func wordsSuggestion(surface string) string {
	runes := []rune(surface)
	if len(runes) > 3 {
		runes = runes[:3]
	}

	return fmt.Sprintf("fisk knowledge words %s", strings.ToLower(string(runes)))
}
