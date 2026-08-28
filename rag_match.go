//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/choria-io/fisk"
	"github.com/choria-io/ui/columns"
	"github.com/choria-io/ui/table"

	"github.com/choria-io/fisk-ai/internal/rag"
	"github.com/choria-io/fisk-ai/internal/util"
)

const (
	// matchScreenLimit is the default document budget: enough to read without
	// scrolling, not enough to flood a terminal. The total is always reported, so
	// the budget never hides the size of the answer.
	matchScreenLimit = 20

	// matchMaxPathRunes bounds a rendered path. Paths come from the corpus, so they
	// are sanitized and truncated like any other text that reaches a terminal.
	matchMaxPathRunes = 120
)

var (
	knowledgeMatchQuery      string
	knowledgeMatchLimit      int
	knowledgeMatchAll        bool
	knowledgeMatchSort       string
	knowledgeMatchMinMatches int
	knowledgeMatchPathsOnly  bool
	knowledgeMatchExplain    bool
	knowledgeMatchCount      bool
	knowledgeMatchExitCode   bool
)

// registerRAGMatchCommand adds the match verb to the knowledge command. The
// aliases are the two other words people reach for when they mean this question.
func registerRAGMatchCommand(k *fisk.CmdClause) {
	match := k.Command("match", "Lists every indexed document that contains the given words, as a complete set rather than a ranking").
		Alias("enumerate").Alias("which").Action(knowledgeMatchAction)

	match.Arg("query", "The words to match; terms side by side must all appear, \"quoted words\" must be adjacent, -word excludes, body: and heading: scope to one part").
		Required().StringVar(&knowledgeMatchQuery)

	match.Flag("limit", "Maximum documents to list; the total is reported either way").Default(fmt.Sprintf("%d", matchScreenLimit)).IntVar(&knowledgeMatchLimit)
	match.Flag("all", "List every matching document, with no budget").UnNegatableBoolVar(&knowledgeMatchAll)
	match.Flag("sort", "Order the set before the limit applies: matches or path").Default(string(rag.SortByMatches)).EnumVar(&knowledgeMatchSort, string(rag.SortByMatches), string(rag.SortByPath))
	match.Flag("min-matches", "Only list documents with at least this many matching body sections").IntVar(&knowledgeMatchMinMatches)
	match.Flag("paths-only", "Print bare paths, one per line, with no banner or headers, for piping").UnNegatableBoolVar(&knowledgeMatchPathsOnly)
	match.Flag("explain", "Show the compiled expression and what the index holds for each term").UnNegatableBoolVar(&knowledgeMatchExplain)
	match.Flag("count", "Print the number of matching documents and nothing else").UnNegatableBoolVar(&knowledgeMatchCount)
	match.Flag("exit-code", "Exit 1 when nothing matched, like grep; without it, zero matches is a successful answer").UnNegatableBoolVar(&knowledgeMatchExitCode)
}

// validateMatchFlags rejects the combinations that contradict each other rather
// than silently letting one win, since a flag the user typed and did not get is
// worse than an error naming the conflict.
func validateMatchFlags(limitSet bool) error {
	if knowledgeMatchAll && limitSet {
		return fmt.Errorf("--all and --limit contradict each other; --all is the complete set and --limit is a budget")
	}
	if knowledgeMatchCount && (knowledgeMatchPathsOnly || knowledgeMatchExplain) {
		return fmt.Errorf("--count prints only a number, so it cannot be combined with --paths-only or --explain")
	}
	if knowledgeMatchPathsOnly && knowledgeMatchExplain {
		return fmt.Errorf("--paths-only prints only paths, so it cannot be combined with --explain")
	}
	if knowledgeMatchMinMatches < 0 {
		return fmt.Errorf("--min-matches cannot be negative")
	}

	return nil
}

// knowledgeMatchAction wraps the run so the exit decision happens after the
// rendered document has been flushed: os.Exit skips deferred writes, and grep
// semantics must not cost the output they are reporting on.
func knowledgeMatchAction(pc *fisk.ParseContext) error {
	matched, err := runKnowledgeMatch(pc)
	if err != nil {
		return err
	}

	// Only when asked for: a complete empty answer is a successful answer, so the
	// default exit is 0 even at zero matches.
	if knowledgeMatchExitCode && matched == 0 {
		os.Exit(1)
	}

	return nil
}

// runKnowledgeMatch renders one match run and reports how many documents matched.
func runKnowledgeMatch(pc *fisk.ParseContext) (int, error) {
	ctx, cancel := interruptContext()
	defer cancel()

	if err := validateMatchFlags(flagWasSet(pc, "limit")); err != nil {
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

	opts := rag.EnumerateOptions{
		Limit:          knowledgeMatchLimit,
		Sort:           rag.EnumerateSort(knowledgeMatchSort),
		MinBodyMatches: knowledgeMatchMinMatches,
	}
	if knowledgeMatchAll {
		opts.Limit = 0
	}

	res, err := store.Enumerate(ctx, knowledgeMatchQuery, opts)
	if err != nil {
		return 0, err
	}

	c := columns.New()
	defer c.WriteTo(os.Stdout)

	return res.Matched, renderMatch(c, res, len(cfg.RAGCitationRules()) > 0)
}

// flagWasSet reports whether the user gave a flag, as opposed to it holding its
// default. A budget the user did not ask for must not contradict --all.
func flagWasSet(pc *fisk.ParseContext, name string) bool {
	for _, el := range pc.Elements {
		if f, ok := el.Clause.(*fisk.FlagClause); ok && f.Model().Name == name {
			return true
		}
	}

	return false
}

// renderMatch adds the whole result to c. citations says whether the operator
// configured any citation rules, which decides whether the table carries an address
// column.
//
// The two machine-readable modes bypass it and write bare lines, as knowledge show
// does for a chunk: a document renderer decorates, and decoration is exactly what a
// pipe cannot have.
func renderMatch(c *columns.Document, res *rag.EnumerateResult, citations bool) error {
	machine := knowledgeMatchCount || knowledgeMatchPathsOnly

	switch res.Status {
	case rag.EnumIndexNotBuilt:
		if machine {
			return fmt.Errorf("the knowledge index has not been built yet; run: fisk knowledge index")
		}
		c.Print("the knowledge index has not been built yet; run: fisk knowledge index")

		return nil

	case rag.EnumCorpusEmpty:
		if machine {
			return fmt.Errorf("the knowledge index has 0 documents; nothing can be matched; run: fisk knowledge index")
		}
		c.Print("the knowledge index has 0 documents; nothing can be matched")
		c.Print("run: fisk knowledge index")

		return nil

	case rag.EnumQueryEmpty:
		// Nothing was queried, so this says nothing about the index. It is an error
		// rather than an empty answer for exactly that reason.
		return fmt.Errorf("no searchable terms in query %q\n  terms shorter than %d characters are dropped before matching\n  nothing was queried, so this says nothing about the index",
			util.SanitizeForTerminal(knowledgeMatchQuery, matchMaxPathRunes), rag.MinTermRunes)
	}

	if knowledgeMatchCount {
		fmt.Println(res.Matched)
		return nil
	}

	if knowledgeMatchPathsOnly {
		// No banner, no headers, no notes: anything else corrupts a pipe, and the
		// tier line is printed by every other verb.
		for _, d := range res.Docs {
			fmt.Println(d.Path)
		}
		return nil
	}

	c.Print(rag.EnumerateTierLine)
	c.Blank()
	c.Item("Query", util.SanitizeForTerminal(knowledgeMatchQuery, matchMaxPathRunes))
	c.Item("Compiled", res.Compiled)
	c.Item("Matched", fmt.Sprintf("%d of %d indexed documents", res.Matched, res.IndexedDocuments))
	c.Blank()

	if len(res.Docs) == 0 {
		renderEmptyMatch(c, res)

		return nil
	}

	c.Embed(matchTable(res.Docs, citations))
	renderMatchNotes(c, res)

	return nil
}

// terminalText prepares one corpus-derived value for a table cell: sanitized, and
// cut to the width a knowledge table renders a path at.
func terminalText(s string) string {
	return util.TruncateLine(util.SanitizeForTerminal(s, matchMaxPathRunes), matchMaxPathRunes)
}

// matchTable renders the matched documents. The citation carries the path as its
// own prefix, so listing both doubles the width of the widest column to say the
// same thing twice; the citation wins because it is the token knowledge show
// accepts. Citations and addresses come from the corpus, so they are sanitized and
// truncated like any other text on its way to a terminal.
//
// citations says whether the operator configured any citation rules. Without them
// there is no address any document could have, and a column of blanks in every
// listing is worse than no column. With them the column is blank for a document no
// rule matched, which is the ordinary state of a partly published corpus.
//
// The address is MatchedDoc.Address, which addresses the document at its first
// matching chunk, and is what knowledge_enumerate hands the model. A rule using
// ${ordinal} therefore renders here with the ordinal and in knowledge sources
// without it; see rag.CitationMapper.RenderDocument.
func matchTable(docs []rag.MatchedDoc, citations bool) *table.Table {
	tbl := table.NewTableWriter("")

	if !citations {
		tbl.AddHeaders("Citation", "Body", "Heading")
		for _, d := range docs {
			tbl.AddRow(terminalText(d.Citation), d.BodyMatches, d.HeadingMatches)
		}

		return tbl
	}

	tbl.AddHeaders("Citation", "Body", "Heading", "Address")
	for _, d := range docs {
		address := ""
		if d.AddressMapped {
			address = terminalText(d.Address)
		}

		tbl.AddRow(terminalText(d.Citation), d.BodyMatches, d.HeadingMatches, address)
	}

	return tbl
}

// renderMatchNotes adds what the reader needs after the table: why a stemmed count
// is larger than the literal one, which terms were never queried, and that the
// list was cut short.
func renderMatchNotes(c *columns.Document, res *rag.EnumerateResult) {
	if knowledgeMatchExplain {
		c.Blank()
		renderTermFrequency(c, res)
	}

	var notes []string

	// Only when the two counts differ, so it does not fire on every query and
	// become noise the reader stops seeing.
	for _, t := range res.Terms {
		if t.Dropped || t.Literal >= t.Docs {
			continue
		}
		notes = append(notes, fmt.Sprintf("note: %q matched by word stem; %d %s it as written",
			t.Surface, t.Literal, plural(t.Literal, "document contains", "documents contain")))
	}

	for _, t := range res.Terms {
		if t.Dropped {
			notes = append(notes, fmt.Sprintf("note: %q was not queried; terms shorter than %d characters are dropped before matching",
				util.SanitizeForTerminal(t.Surface, matchMaxPathRunes), rag.MinTermRunes))
		}
	}

	if res.Truncated {
		notes = append(notes, fmt.Sprintf("showing %d of %d; use --all or --limit for the rest", res.Returned, res.Matched))
	}

	if len(notes) == 0 {
		return
	}

	c.Blank()
	for _, note := range notes {
		c.Print(note)
	}
}

// renderEmptyMatch adds the empty result. The two cases below are both complete
// answers and call for opposite next actions, which is why a single "0 results"
// line cannot serve them: one says the word is absent from every document, the
// other says the words are each present but never together.
func renderEmptyMatch(c *columns.Document, res *rag.EnumerateResult) {
	var absent []string
	for _, t := range res.Terms {
		if !t.Dropped && t.Docs == 0 {
			absent = append(absent, fmt.Sprintf("%q", t.Surface))
		}
	}

	if len(absent) > 0 {
		c.Print(fmt.Sprintf("No indexed document contains %s, in any of its forms. This is the complete answer, not a ranking cutoff.", strings.Join(absent, " or ")))
	} else {
		c.Print("No indexed document contains all of these. This is the complete answer, not a ranking cutoff.")
	}
	c.Blank()

	// Unconditional on an empty result, not only under --explain: the frequencies
	// are what stop a zero being read as absence when the author simply typed a form
	// the documents do not use.
	renderTermFrequency(c, res)
	c.Blank()

	if len(absent) == 0 {
		c.Print("Each term is in the index but they never appear in the same document. Drop a term to see either set on its own.")

		return
	}

	// The word is not in the index, so ranking sections by it is the weaker of the
	// two next moves. What the reader wants now is what the documents call it
	// instead, which is a vocabulary question rather than a retrieval one.
	c.Print(fmt.Sprintf("Nothing to narrow. To see what the documents do call it, run: %s", wordsSuggestion(firstAbsentSurface(res))))
	c.Print(fmt.Sprintf("Or 'fisk knowledge search %s' to rank sections, which may use other words.",
		util.SanitizeForTerminal(knowledgeMatchQuery, matchMaxPathRunes)))
}

// firstAbsentSurface names the term the vocabulary suggestion should be built from.
func firstAbsentSurface(res *rag.EnumerateResult) string {
	for _, t := range res.Terms {
		if !t.Dropped && t.Docs == 0 {
			return t.Surface
		}
	}

	return knowledgeMatchQuery
}

// renderTermFrequency shows what the index holds for each term: the stem it is
// matched by, how many documents hold any form of it, and how many hold it as
// written.
//
// The columns are named and ordered the way knowledge words names them. Both
// numbers are document counts, so heading either of them "Documents" made the two
// commands appear to report different totals for the same word, which is exactly
// what these counts exist to prevent.
func renderTermFrequency(c *columns.Document, res *rag.EnumerateResult) {
	if len(res.Terms) == 0 {
		return
	}

	tbl := table.NewTableWriter("")
	tbl.AddHeaders("Word", "Stem", "As written", "Any form")
	for _, t := range res.Terms {
		if t.Dropped {
			tbl.AddRow(util.SanitizeForTerminal(t.Surface, matchMaxPathRunes), "not queried", "-", "-")
			continue
		}
		tbl.AddRow(util.SanitizeForTerminal(t.Surface, matchMaxPathRunes), t.Stem, t.Literal, t.Docs)
	}

	c.Section("Term frequency in the index", func(c *columns.Document) {
		c.Embed(tbl)
	})
}

// matchSuggestion is the pointer knowledge search prints when it ranks nothing, so
// a user staring at an empty ranked result learns that a different question exists.
// It is the cheapest discovery surface the feature has.
func matchSuggestion(query string) string {
	return fmt.Sprintf("Nothing ranked above the cutoff, which is not the same as nothing being there. To find out whether any document contains these words, run: fisk knowledge match %q",
		util.SanitizeForTerminal(query, matchMaxPathRunes))
}
