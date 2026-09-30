// Package output renders one giraffe find answer: section-headed plain
// text, sections omitted when empty (specifications/giraffe.md §7).
package output

import (
	"fmt"
	"iter"
	"path"
	"sort"
	"strconv"
	"strings"

	"giraffe/internal/entry"
)

const (
	// PageSize is how many results a page holds at most (§7's "8
	// results"); a --page is that many entries into the ranking, and the
	// window a caller enriches before printing (main attaches callers
	// to exactly the page's entries, directive 050) is this wide.
	PageSize   = 8
	byteBudget = 2000
	narrowByN  = 3 // how many groups the truncated section suggests narrowing by

	// minShareResults and minShareBudget are the least one word of a
	// several-word answer gets (directive 062, h3).
	minShareResults = 3
	minShareBudget  = 1000

	// declCountThreshold gates declarations: on a file result (directive
	// 009 item 3): a caller deciding whether to open the file or narrow
	// it first with --query wants to know when it holds unusually many,
	// and only then (giraffe.md §25 C-25). It fires on a real corpus —
	// 51 of gekka's 508 files at e6fd6aa (directive 043's count).
	//
	// size: has no threshold any more (043). 009 set one at 100,000
	// bytes against llvm-project's generated .td/.inc files; on gekka it
	// fired on two files of 508 and had appeared in no transcript since,
	// while the decision it exists for — cat the file, or sed the range
	// a result line names — is one every file result faces. So every
	// file result carries it: " size:6837" is ten bytes or so, on file
	// entries only, against a 2,000-byte page.
	declCountThreshold = 40

	// maxCallers bounds how many call sites a result's callers: line
	// and a ## source header name (directive 050 §4): bounded because a
	// list of eighty is not read, not to save bytes — the remainder is
	// a count. Five: the sites query.orderCallers puts first are the
	// ones whose qualifier names the declaration's own type, and five
	// leaves room for a name with a few real callers to show all of
	// them while keeping the line to one line.
	maxCallers = 5
)

// Source is one declaration's text for ## source: the entry and the
// lines of its range.
type Source struct {
	Entry entry.Entry
	Lines []string
}

// FileText is a whole file a page carries under ## file (directive
// 073): its repo-relative path and its lines.
type FileText struct {
	Path  string
	Lines []string
}

// sourceBudget bounds ## source the way byteBudget bounds a page.
const sourceBudget = 2000

// sourceSection renders ## source: each source's name, path and range,
// paste-ready like a result line, then its lines verbatim, until
// sourceBudget is spent. A source longer than the room left prints its
// first lines into half of that room, then the lines of the rest that
// carry the other search words — the most words first, each with its
// line number the way grep -n prints one — into the other half, and
// ends with the count of lines left and the sed that prints them from
// where the head stopped; nothing after it prints. Directive 072: a
// declaration of two hundred lines asked for by name showed its first
// forty, and the line the caller was after — the one that carried the
// other words of the call — was past them, so the run opened the file
// for it. The words are the call's own, matched as text, the way
// everything else on the page is.
func sourceSection(sources []Source, words []string) string {
	var b strings.Builder
	for _, src := range sources {
		e := src.Entry
		fmt.Fprintf(&b, "name:%s path:%s line:%s\n", quote(e.Name), quote(e.Path), e.LineRange())
		if c := callersLine(e.Callers, e.CallersElsewhere); c != "" { // where the name is called is part of the answer to a name typed exactly (directive 050)
			b.WriteString(c)
		}
		room := sourceBudget - b.Len()
		if textLen(src.Lines) <= room {
			for _, l := range src.Lines {
				b.WriteString(l)
				b.WriteByte('\n')
			}
			continue
		}
		headEnd := b.Len() + room/2
		i := 0
		for ; i < len(src.Lines) && b.Len()+len(src.Lines[i])+1 <= headEnd; i++ {
			b.WriteString(src.Lines[i])
			b.WriteByte('\n')
		}
		for _, wl := range wordLines(src.Lines[i:], e.Line+i, otherWords(words, e.Name)) {
			line := fmt.Sprintf("%d: %s\n", wl.number, wl.text)
			if b.Len()+len(line) > sourceBudget {
				break
			}
			b.WriteString(line)
		}
		fmt.Fprintf(&b, "... %d more lines: sed -n '%d,%dp' %s\n", len(src.Lines)-i, e.Line+i, e.EndLine, quote(e.Path))
		return b.String()
	}
	return b.String()
}

// textLen is the bytes lines take printed one a line.
func textLen(lines []string) int {
	n := 0
	for _, l := range lines {
		n += len(l) + 1
	}
	return n
}

// otherWords is words without the one that is name itself, lowered:
// the name is on every source's header already, and a recursive call
// aside is on no line of its body.
func otherWords(words []string, name string) []string {
	var out []string
	for _, w := range words {
		if !strings.EqualFold(w, name) {
			out = append(out, strings.ToLower(w))
		}
	}
	return out
}

// wordLine is one line of a source's remainder that carries at least
// one of the search words, numbered as in the file.
type wordLine struct {
	number int
	text   string
	words  int // how many distinct words it carries
}

// wordLines picks, from lines (the first of them numbered first), every
// line carrying one of lowerWords, the ones carrying the most distinct
// words first and, among equals, in file order. Whitespace around a
// line is trimmed: it is shown out of its indentation anyway.
func wordLines(lines []string, first int, lowerWords []string) []wordLine {
	var out []wordLine
	for i, l := range lines {
		ll := strings.ToLower(l)
		n := 0
		for _, w := range lowerWords {
			if strings.Contains(ll, w) {
				n++
			}
		}
		if n > 0 {
			out = append(out, wordLine{number: first + i, text: strings.TrimSpace(l), words: n})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].words != out[j].words {
			return out[i].words > out[j].words
		}
		return out[i].number < out[j].number
	})
	return out
}

// Summary is the evidence carried on the always-present ## index line: it
// is what the caller pastes into a report to show the command actually
// ran, so it is whole-index totals, not counts scoped to one query — the
// same numbers specifications/giraffe.md §11 measures. Dispatched is the
// one field that is specific to this invocation (how much work this run
// actually did) — named for exactly what it counts (directive 018 §4):
// files sent through lang.Extract this run, not files actually read.
// Since directive 017 §1 the two are different numbers (a file the
// generic module handles is dispatched without ever being opened), and
// "Reparsed" claimed the second when it always meant the first.
type Summary struct {
	Commit   string
	Symbols  int
	Packages int
	// UndocumentedExported and UndocumentedPackage report the M11
	// quality measure (code-manners) separately by scope (§22 C-11): one
	// mixed count reads as a regression the moment unexported
	// declarations — documented far less consistently — join the index,
	// when the exported figure (what M11 actually governs) has not
	// moved at all.
	UndocumentedExported int
	UndocumentedPackage  int
	Dispatched           int
	ShowProject          bool // true when the index spans more than one project
	// ShowLang mirrors ShowProject (§24 C-20, §25 C-25): lang costs a
	// line's worth of bytes per result the same way project does, so it
	// is only printed when more than one *known* value is actually
	// present — a single-language repository already knows what it is
	// written in (and ## index already says so via Languages below).
	// Unknown (Lang == "") is not a value for this count: a repository
	// that is all Go, plus assorted files nothing extracts from, still
	// has exactly one known language.
	ShowLang bool
	// ExtVaries reports, per known lang, whether more than one distinct
	// extension was observed for it (§25 C-25: "print ext only when it
	// is not recoverable from lang"). Go registers one extension, so
	// ExtVaries["go"] is false and a result line never prints ext:go;
	// an operator who assigns both .h and .inc to "c" makes
	// ExtVaries["c"] true, and ext becomes real information again. This
	// is independent of ShowLang — a single-language C++ repository
	// still benefits from ext distinguishing .h from .cpp even though
	// lang itself never needs printing.
	ExtVaries map[string]bool
	// Call is the find call this answer is for, as typed (directive
	// 071): every command the answer prints to be typed back is
	// composed from it, in the one grammar.
	Call Call
	// Sources are the declarations whose name is exactly one of the
	// search words, with their text (directive 065, g23; 044's ##
	// source): printed under ## source after the answers, within
	// sourceBudget.
	Sources []Source
	// File is the answer's top result when that result is a small file
	// (directive 073), whole: a file is the one result whose text the
	// page otherwise only names. Page 1 only; nil otherwise.
	File *FileText
	// Languages and ByExt are the two ## index breakdowns (§25 C-24):
	// what turns "0 symbols" into "go 0 | unknown 181099" for a
	// repository giraffe cannot extract from at all, plus the raw
	// extension counts underneath it — a fact requiring no judgement,
	// unlike the guess an extension-to-language table would encode.
	// Pre-rendered here, not left as raw counts, so Format stays a pure
	// renderer.
	Languages string
	ByExt     string
	// Layout is the repository's top-level directories, most entries
	// first, each with the doc line of its shallowest code file that has
	// one (directive 073): what the repository is made of, printed under
	// ## layout on a page where a word was found nowhere.
	Layout     []string
	Warnings   []string // parse failures; rendered "unparsed: ..."
	Notices    []string // other non-fatal cautions (e.g. an unignored local cache); rendered as-is
	Exclusions []string
	// TryNext is a short, evidence-backed direction offered only when a
	// find call returned zero results (§22's fan-out; C-9's third
	// question is browsing, but an empty vocabulary search has to say
	// what to try instead). Never fabricated: populated only from
	// something actually observed in the index (query.Suggest).
	TryNext []string
}

// Summarize computes a Summary from the full current entry set and the
// stats of the Refresh that produced it. knownLanguages names every
// language this build could possibly report (a registered extractor's
// own name, or a name an operator assigned via index.LoadExtensions —
// its own file since directive 023 item 2, .giraffe/exclude before it —
// from every root) — used only to give the ## index breakdown visibility
// into a known language with zero matches (§25 C-24: "go 0, unknown
// 181099" for llvm-project is accurate precisely because Go is known
// and genuinely absent). A plain list of opaque names, the same
// treatment Kind and Scope values already get (§21 C-12) — this
// package still never imports the lang package itself.
func Summarize(entries iter.Seq[entry.Entry], commit string, dispatched int, warnings, notices, exclusions, knownLanguages []string) Summary {
	projects := make(map[string]bool)
	positions := make(map[string]bool)
	languages := make(map[string]bool) // known langs only — see ShowLang
	extsByLang := make(map[string]map[string]bool)
	var undocExported, undocPackage, symbols int
	langCounts := make(map[string]int, len(knownLanguages)+1)
	for _, l := range knownLanguages {
		langCounts[l] = 0 // seeded so a known language with no matches still reads as "0", not absent
	}
	extCounts := make(map[string]int)
	dirCounts := make(map[string]int)
	dirDoc := make(map[string]string)
	dirDocDepth := make(map[string]int)
	// One pass over the whole entry set computes every whole-index total
	// §7/§11 wants on the ## index line (§30 C-46: entries stream lazily
	// from the store here — this still visits every entry, since these
	// are whole-index totals regardless of any query, but nothing beyond
	// the entry currently being summed is ever resident at once, unlike
	// the materialized slice this used to range over three times — once
	// here, once in languageBreakdown, once in extBreakdown).
	for e := range entries {
		symbols++
		projects[e.Project] = true
		langLabel := e.Lang
		if langLabel == "" {
			langLabel = unknownLang
		}
		langCounts[langLabel]++
		extLabel := e.Ext
		if extLabel == "" {
			extLabel = noExt
		}
		extCounts[extLabel]++
		if i := strings.IndexByte(e.Path, '/'); i > 0 {
			top := e.Path[:i]
			dirCounts[top]++
			if e.Kind == entry.KindFile && !e.Test && e.Doc != "" {
				depth := strings.Count(e.Path, "/")
				if d, ok := dirDocDepth[top]; !ok || depth < d || depth == d && e.Path < dirDoc[top+"\x00path"] {
					dirDoc[top], dirDocDepth[top], dirDoc[top+"\x00path"] = e.Doc, depth, e.Path
				}
			}
		}
		if e.Lang != "" {
			languages[e.Lang] = true
			if extsByLang[e.Lang] == nil {
				extsByLang[e.Lang] = make(map[string]bool)
			}
			extsByLang[e.Lang][e.Ext] = true
		}
		// A file entry (§22 item 4) is not a declaration M11 governs;
		// counting its always-empty Doc would inflate "undocumented
		// exported" with something that was never meant to carry
		// documentation, undermining the accuracy §22 C-11 just fixed.
		if e.Doc == "" && e.Kind != entry.KindFile {
			if e.Exported {
				undocExported++
			} else {
				undocPackage++
			}
		}
		// Packages counts distinct language positions, not distinct
		// Position pairs: a method or field (§28 C-38) carries a second
		// pair — its receiver or containing type — after the file's
		// shared module/crate/namespace position, which is always
		// first. Counting every pair here would let "Packages" grow
		// with the number of distinct types, not packages.
		if len(e.Position) > 0 {
			p := e.Position[0]
			positions[p.Name+"="+p.Value] = true
		}
	}
	extVaries := make(map[string]bool, len(extsByLang))
	for l, exts := range extsByLang {
		extVaries[l] = len(exts) > 1
	}
	var languagesLine, byExtLine string
	if symbols > 0 {
		languagesLine = breakdownLine(langCounts)
		byExtLine = breakdownLine(extCounts)
	}
	return Summary{
		Commit:               commit,
		Symbols:              symbols,
		Packages:             len(positions),
		UndocumentedExported: undocExported,
		UndocumentedPackage:  undocPackage,
		Dispatched:           dispatched,
		ShowProject:          len(projects) > 1,
		ShowLang:             len(languages) > 1,
		ExtVaries:            extVaries,
		Languages:            languagesLine,
		ByExt:                byExtLine,
		Layout:               layoutLines(dirCounts, dirDoc),
		Warnings:             warnings,
		Notices:              notices,
		Exclusions:           exclusions,
	}
}

// maxLayout caps how many directories ## layout names.
const maxLayout = 16

// layoutLines renders ## layout: each top-level directory with its
// entry count and, when it has one, its doc line, most entries first.
func layoutLines(counts map[string]int, docs map[string]string) []string {
	dirs := make([]string, 0, len(counts))
	for d := range counts {
		dirs = append(dirs, d)
	}
	sort.Slice(dirs, func(i, j int) bool {
		if counts[dirs[i]] != counts[dirs[j]] {
			return counts[dirs[i]] > counts[dirs[j]]
		}
		return dirs[i] < dirs[j]
	})
	if len(dirs) > maxLayout {
		dirs = dirs[:maxLayout]
	}
	lines := make([]string, len(dirs))
	for i, d := range dirs {
		lines[i] = fmt.Sprintf("%s/ %d", d, counts[d])
		if doc := docs[d]; doc != "" {
			lines[i] += " — " + doc
		}
	}
	return lines
}

// maxBreakdownShown caps how many buckets ## index names outright in
// either breakdown line (§24 C-20: "keep the line readable — it is
// pasted into every report"), the same response-budget instinct §7
// already applies to a result line, extended here to a summary line
// for a repository with many languages or many extensions at once
// (§25: "006's 'cap at 6 with a +N tail' is a reasonable precedent").
const maxBreakdownShown = 6

// unknownLang labels, in the ## index breakdown only, the bucket of
// entries with no known language (Lang == ""). Display text, never an
// attribute value: querying lang:unknown matches nothing, since no
// entry's Lang is ever literally "unknown" (§25 C-24 — absence is
// encoded by "", not a placeholder string).
const unknownLang = "unknown"

// noExt labels, in the ## index by-ext breakdown only, the bucket of
// files with no extension at all (Ext == "") — the known-extensionless
// files §24 C-19 named and internal/lang/exclude now recognises
// (Makefile, LICENSE, ...). Display text, not an attribute value, the
// same way unknownLang is not.
const noExt = "(none)"

// unknownLang and noExt labels are applied, and the "go 0, unknown
// 181099" style breakdown lines built, inline in Summarize's single
// pass over entries now (§30 C-46: avoiding two further full scans of
// the same iterator) rather than in their own functions ranging over a
// materialized slice a second and third time — see Summarize.

// breakdownLine renders a counts map as "key N | key N | ... | +M more"
// (§25: pipe-separated, matching the directive's own worked example),
// sorted by count descending then key, capped at maxBreakdownShown.
// Shared by languageBreakdown and extBreakdown (code-manners D5): both
// are "sort a distribution, cap it, join it" with nothing
// language-specific in the rendering itself.
func breakdownLine(counts map[string]int) string {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if counts[keys[i]] != counts[keys[j]] {
			return counts[keys[i]] > counts[keys[j]]
		}
		return keys[i] < keys[j]
	})
	shown, more := keys, 0
	if len(shown) > maxBreakdownShown {
		more = len(shown) - maxBreakdownShown
		shown = shown[:maxBreakdownShown]
	}
	parts := make([]string, len(shown))
	for i, k := range shown {
		parts[i] = fmt.Sprintf("%s %d", k, counts[k])
	}
	line := strings.Join(parts, " | ")
	if more > 0 {
		line += fmt.Sprintf(" | +%d more", more)
	}
	return line
}

// Format renders the full answer for one find call, page 1 — the top of
// the ranking. Equivalent to FormatFrom with page 1.
func Format(s Summary, matches []entry.Entry) string {
	return FormatFrom(s, matches, 1)
}

// FormatFrom renders the full answer for one find call, at page (--page,
// directive 033; 1-indexed, page 1 is the top of the ranking). A page
// beyond the last is not an error — it renders like a query with nothing
// left to show, the same as `page` being out of range for any other
// paginated listing.
//
// §29 C-42/§33's own history: 032 shipped --offset, a count of entries to
// skip — a unit that matched nothing giraffe ever displays, so a caller
// who had just been shown "353 more" had to invent the arithmetic to turn
// that into a number to type. 033 replaces it outright (032's own
// directive named it disposable: "it shipped one directive ago and
// nothing depends on it" — confirmed here: grep for --offset outside
// output.go/main.go/main_test.go before this change found nothing).
// `--page` is the unit `## truncated` now displays (see truncatedLine),
// so the number a caller reads is the number they type — no multiply,
// no second flag to learn. page and pageSize convert to fit's own
// count-based offset right here, once, rather than teaching fit a second
// unit — fit already had exactly the right primitive (skip N entries),
// it was only ever reachable through the wrong-unit flag.
func FormatFrom(s Summary, matches []entry.Entry, page int) string {
	return FormatWords(s, []WordAnswer{{Matches: matches, TryNext: s.TryNext}}, page)
}

// WordAnswer is one search word's share of an answer (directive 062):
// its ranked matches and, when there are none, its direction. Word is
// "" for the one unnamed answer of a browse or a single-answer caller.
type WordAnswer struct {
	Word    string
	Matches []entry.Entry
	TryNext []string
	// Words are the search words the answer is for, and Counts, when
	// several words were ranked together (directive 065), how many
	// entries each matched on its own. A result prints the doc line
	// that carries most of Words.
	Words  []string
	Counts []int
	// Text is, for a word no name, doc comment or path contains, where
	// it occurs in the text of the index's files (directive 073): the
	// search the none: answer used to leave to grep. TextFiles counts
	// the files, TextSearched how many were read; Text holds the first
	// sites, file:line: the line.
	Text         []string
	TextFiles    int
	TextSearched int
}

// FormatWords renders one find call's answer for every word it was
// given, at page (directive 062): the ## index block once, then each
// word's results, truncation and direction under headers naming the
// word when there is more than one, then ## warnings once. Before
// this, a caller with three words ran giraffe three times and read the
// index block and the warnings three times. A declaration an earlier
// word already showed on this page is not printed again. A named word
// that matched nothing says so, and what it was matched against,
// rather than leaving an answer with no results section to be read as
// one.
func FormatWords(s Summary, answers []WordAnswer, page int) string {
	if page < 1 {
		page = 1
	}

	var b strings.Builder

	indexLine := fmt.Sprintf("commit %s | symbols %d | packages %d | undocumented %d exported, %d package | dispatched %d",
		s.Commit, s.Symbols, s.Packages, s.UndocumentedExported, s.UndocumentedPackage, s.Dispatched)
	b.WriteString("## index\n")
	b.WriteString(indexLine)
	b.WriteByte('\n')
	if s.Languages != "" {
		fmt.Fprintf(&b, "languages: %s\n", s.Languages)
	}
	if s.ByExt != "" {
		fmt.Fprintf(&b, "by ext:    %s\n", s.ByExt)
	}
	// ## layout follows the index block, ahead of the answers
	// (directive 073): a reader who cuts the page short (head -100) still
	// has what the repository is made of when a word was found nowhere.
	if len(s.Layout) > 0 && page == 1 && anyNowhere(answers) {
		b.WriteString("\n## layout\n")
		for _, l := range s.Layout {
			fmt.Fprintf(&b, "%s\n", l)
		}
	}

	// §26 C-30: the byte budget is a safety limit, and a safety limit
	// with exceptions is not one. Everything printed ahead of ## results
	// counts against it — b.Len() at this point is exactly that: the
	// "## index" header, the index line, and (when present) the
	// languages: and by-ext: lines, none of which fit's own preambleLen
	// used to see. With several words each word's block gets the same
	// budget, the shared preamble counted against every one of them.
	preamble := b.Len()
	shownBefore := make(map[string]bool)
	printed := make(map[string]bool) // commands already on this page
	for _, a := range answers {
		named := len(answers) > 1
		header := func(section string) string {
			if named {
				return fmt.Sprintf("\n## %s: %s\n", section, quote(a.Word))
			}
			return fmt.Sprintf("\n## %s\n", section)
		}
		var fresh []entry.Entry
		for _, e := range a.Matches {
			if !shownBefore[entryKey(e)] {
				fresh = append(fresh, e)
			}
		}
		resultsHeader := header("results")
		preambleLen := preamble
		if len(fresh) > 0 {
			preambleLen += len(resultsHeader)
		}
		// The entries several words carry together get a page of their
		// own; each word's own answer shares the rest (directive 065).
		share := len(answers)
		if len(a.Counts) > 0 {
			share = 1
		} else if len(answers) > 1 && len(answers[0].Counts) > 0 {
			share = len(answers) - 1
		}
		shown, remainder := fitShare(fresh, a.Words, s.ShowProject, s.ShowLang, s.ExtVaries, preambleLen, page, share)

		switch {
		case len(shown) > 0:
			b.WriteString(resultsHeader)
			if len(a.Counts) > 0 {
				b.WriteString(countsLine(a.Words, a.Counts))
			}
			for _, e := range shown {
				shownBefore[entryKey(e)] = true
				b.WriteString(resultLine(e, a.Words, s.ShowProject, s.ShowLang, s.ExtVaries))
			}
			if a.TextSearched > 0 {
				b.WriteString(textLines(a))
			}
		case a.Word != "" && len(a.Matches) == 0 && len(a.Counts) > 0:
			b.WriteString(resultsHeader)
			b.WriteString(countsLine(a.Words, a.Counts))
			fmt.Fprintf(&b, "none: no name, doc comment or path among the index's %d entries contains two of these words (text inside bodies is not indexed)\n", s.Symbols)
		case a.Word != "" && len(a.Matches) == 0 && a.TextSearched > 0:
			b.WriteString(resultsHeader)
			fmt.Fprintf(&b, "none: no name, doc comment or path among the index's %d entries contains %s\n", s.Symbols, quote(a.Word))
			b.WriteString(textLines(a))
		case a.Word != "" && len(a.Matches) == 0:
			b.WriteString(resultsHeader)
			fmt.Fprintf(&b, "none: no name, doc comment or path among the index's %d entries contains %s (text inside bodies is not indexed)\n", s.Symbols, quote(a.Word))
		case a.Word != "" && len(fresh) == 0:
			b.WriteString(resultsHeader)
			b.WriteString("every match is shown above\n")
		}

		if len(remainder) > 0 {
			b.WriteString(header("truncated"))
			if len(answers) > 1 && len(a.Words) == 1 && a.Word == a.Words[0] {
				// A single word's block among several: its next page is
				// the call's, and narrowing is the several-word block's
				// to offer (directive 073).
				b.WriteString(nextPageLine(remainder, page, s.Call, printed))
			} else {
				b.WriteString(truncatedLines(remainder, page, s.Call, printed))
			}
		}

		// §22: `## truncated` already tells a caller how to narrow the
		// same question. Nothing told it how to switch to a different
		// one, and a zero-result answer carried no direction at all.
		// Only emitted when there is something to show — TryNext is
		// populated exclusively from evidence in the index
		// (query.Suggest), never a fixed list.
		if len(a.Matches) == 0 && len(a.TryNext) > 0 {
			b.WriteString(header("try next"))
			for _, line := range a.TryNext {
				fmt.Fprintf(&b, "%s\n", line)
			}
		}
	}

	if len(s.Sources) > 0 && page == 1 {
		b.WriteString("\n## source\n")
		b.WriteString(sourceSection(s.Sources, s.Call.Words))
	}

	if s.File != nil && page == 1 {
		fmt.Fprintf(&b, "\n## file: %s\n", s.File.Path)
		for i, l := range s.File.Lines {
			fmt.Fprintf(&b, "%4d  %s\n", i+1, l)
		}
	}

	if len(s.Warnings) > 0 || len(s.Notices) > 0 || len(s.Exclusions) > 0 {
		b.WriteString("\n## warnings\n")
		for _, w := range s.Warnings {
			fmt.Fprintf(&b, "unparsed: %s\n", w)
		}
		for _, n := range s.Notices {
			fmt.Fprintf(&b, "%s\n", n)
		}
		if len(s.Exclusions) > 0 {
			fmt.Fprintf(&b, "active exclusions: %s\n", strings.Join(s.Exclusions, ", "))
		}
	}

	return b.String()
}

// anyNowhere reports whether a word of the call was found nowhere: in
// no name, doc comment or path, and, where its text was searched, in no
// file's text either.
func anyNowhere(answers []WordAnswer) bool {
	for _, a := range answers {
		if a.Word != "" && len(a.Counts) == 0 && len(a.Matches) == 0 && a.TextFiles == 0 {
			return true
		}
	}
	return false
}

// textLines renders where a none: word occurs in the files' text: that
// it occurs in none of them, or the files it is in and its first
// sites.
func textLines(a WordAnswer) string {
	if a.TextFiles == 0 && len(a.Matches) == 0 {
		return fmt.Sprintf("text: nor is it in the text of any of the %d source files indexed\n", a.TextSearched)
	}
	if a.TextFiles == 0 {
		return fmt.Sprintf("text: in the text of none of the %d source files indexed\n", a.TextSearched)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "text: in the text of %d of the %d source files indexed:\n", a.TextFiles, a.TextSearched)
	for _, t := range a.Text {
		fmt.Fprintf(&b, "    %s\n", t)
	}
	if more := a.TextFiles - len(a.Text); more > 0 {
		fmt.Fprintf(&b, "    +%d more files\n", more)
	}
	return b.String()
}

// nextPageLine is truncatedLines' first line alone: the page, the count
// left, and the next page's command, once per page like every command.
func nextPageLine(remainder []entry.Entry, page int, call Call, printed map[string]bool) string {
	c := call.Command(call.Query, page+1)
	if printed[c] {
		c = "as above"
	}
	printed[c] = true
	return fmt.Sprintf("page %d, %d more: %s\n", page, len(remainder), c)
}

// entryKey identifies an entry across the words of one answer.
func entryKey(e entry.Entry) string {
	return e.Project + "\x00" + e.Path + "\x00" + e.Kind + "\x00" + e.Name + "\x00" + strconv.Itoa(e.Line)
}

// resultLine renders one result. line prints whenever the entry has one
// (a generic file entry addresses no specific line, §31.4 — absence is
// the correct encoding, not a placeholder), as the declaration's whole
// range — line:19-62, or line:19 for a one-liner (directive 042;
// entry.Entry.LineRange is the one definition of that form, shared
// with --query's matching). project, ext and lang each
// print only when they discriminate (§25 C-25): project only across more
// than one project (unchanged from §24); ext only when it is not
// recoverable from lang — either lang is unknown, so ext is all there
// is, or lang is known but was observed with more than one extension
// (extVaries); lang only when more than one known language is actually
// present (showLang), the same "costs a line, so only when it varies"
// treatment project already gets — printing lang unconditionally would
// make "lang:go" redundant noise on every line of an all-Go repository,
// exactly what this section exists to avoid. A language position prints
// only when it does not repeat the path's own directory (directive
// 047), by the same rule.
func resultLine(e entry.Entry, words []string, showProject, showLang bool, extVaries map[string]bool) string {
	var line strings.Builder
	fmt.Fprintf(&line, "name:%s kind:%s path:%s", quote(e.Name), quote(e.Kind), quote(e.Path))
	if r := e.LineRange(); r != "" {
		fmt.Fprintf(&line, " line:%s", r)
	}
	if showProject && e.Project != "" {
		fmt.Fprintf(&line, " project:%s", quote(e.Project))
	}
	if e.Ext != "" && (e.Lang == "" || extVaries[e.Lang]) {
		fmt.Fprintf(&line, " ext:%s", quote(e.Ext))
	}
	if showLang && e.Lang != "" {
		fmt.Fprintf(&line, " lang:%s", quote(e.Lang))
	}
	// Exported is the overwhelming default and is never printed, the same
	// way an absent language position is not filled with a placeholder
	// (§6) — the reader assumes exported unless told otherwise, driven by
	// Exported (§21 C-12), not by comparing Scope's displayed word
	// against a fixed value. Only the informative case (this declaration
	// cannot be reused outside its own module) costs a line's worth of
	// bytes, printed in whatever word the language module chose.
	if !e.Exported {
		fmt.Fprintf(&line, " scope:%s", quote(e.Scope))
	}
	// Test is the same "only the informative case costs a line" rule as
	// scope above: production is the overwhelming default, so test:true
	// prints and test:false never does (directive 065, g42) — the fact
	// a caller needs to tell a test's declarations apart from production
	// code's, now that both are indexed.
	if e.Test {
		line.WriteString(" test:true")
	}
	// Size and DeclCount are facts about the file, not the declaration
	// (entry.Entry's own doc comment), so only a Kind==KindFile result
	// carries them: size: always, since a file result is the one kind
	// with no line: range and the caller's next move is to read it
	// whole or not (directive 043); declarations: when the file holds
	// enough of them to be worth narrowing with --query rather than
	// opening blind (directive 009 item 3; giraffe.md §25 C-25).
	if e.Kind == entry.KindFile {
		fmt.Fprintf(&line, " size:%d", e.Size)
		if e.DeclCount > declCountThreshold {
			fmt.Fprintf(&line, " declarations:%d", e.DeclCount)
		}
	}
	for _, pos := range e.Position {
		// The same "only when it discriminates" test the attributes above
		// pass, applied to the language positions for the first time
		// (directive 047 §2 measured one aided run: module: was 1,269 of
		// the 24,050 bytes giraffe printed, on every declaration line,
		// and not once typed into a later --query). Go's module is
		// path.Dir of the entry's own path, so it repeats, verbatim, a
		// part of the line the reader has already been given. A position
		// that says something the line does not already say still
		// prints; --query module:... is unaffected, since the index
		// keeps what the extractor emitted either way.
		if pos.Value == path.Dir(e.Path) {
			continue
		}
		fmt.Fprintf(&line, " %s:%s", pos.Name, quote(pos.Value))
	}
	line.WriteByte('\n')
	// The declaration's own line (§31.4) prints first — it is what lets
	// a caller discard a candidate without opening the file at all
	// ("struct X {" vs "v := X.New" answer the same name search
	// differently, and neither kind nor Doc distinguishes them) — then
	// Doc, which says what the declaration is for rather than what it
	// looks like. Both print when both are present; whether that binds
	// the response budget in practice is measured, not assumed (see the
	// report: the source line is typically shorter than Doc, so the
	// common case costs nothing extra).
	if e.LineText != "" {
		fmt.Fprintf(&line, "  %s\n", e.LineText)
	}
	if d := docLine(e, words); d != "" {
		fmt.Fprintf(&line, "  %s\n", d)
	}
	// Where the name is called (directive 050), after what it is and
	// what it is for: the sites the caller would otherwise grep for —
	// 049 counted that grep in 81 of 119 runs — as places to open,
	// bounded, the rest a count. The label is the attribute --query
	// takes (callers:), and each site is the value it matches. Directive
	// 051 §1 added each site's own line (Text) and, since test files are
	// indexed too, the sites this now lists include the tests: what the
	// grep in 14 of round 9's 20 runs went to get.
	if m := membersLine(e.Members, e.Kind == entry.KindFile); m != "" {
		line.WriteString(m)
	}
	if c := callersLine(e.Callers, e.CallersElsewhere); c != "" {
		line.WriteString(c)
	}
	return line.String()
}

// maxMembers bounds how many of a declaration's members its members:
// line names; the rest are a count.
const maxMembers = 20

// maxMemberSites bounds how many outside call sites a member carries.
const maxMemberSites = 2

// membersLine renders a declaration's members (directive 073) on one
// line, name and range each, in source order: what the reader would
// open the file for to see the shape of a type before reading any of
// it, with the range to read one member alone. "" when it has none.
//
// A file's members are its outline, labelled so.
func membersLine(members []entry.Entry, file bool) string {
	if len(members) == 0 {
		return ""
	}
	shown := members
	if len(shown) > maxMembers {
		shown = shown[:maxMembers]
	}
	parts := make([]string, len(shown))
	for i, m := range shown {
		parts[i] = m.Name + " " + m.LineRange()
		// A member's call sites outside its type's directory, up to
		// maxMemberSites (directive 073).
		if len(m.Callers) > 0 && !file {
			var sites []string
			for j, c := range m.Callers {
				if j == maxMemberSites {
					sites = append(sites, fmt.Sprintf("+%d", len(m.Callers)-j))
					break
				}
				sites = append(sites, c.Place())
			}
			parts[i] += " ← " + strings.Join(sites, ", ")
		}
	}
	label := "members"
	if file {
		label = "outline"
	}
	line := "  " + label + ": " + strings.Join(parts, " | ")
	if more := len(members) - len(shown); more > 0 {
		line += fmt.Sprintf(" | +%d more", more)
	}
	return line + "\n"
}

// callersLine renders callers: as a small block, one site a line —
// path:line: the call's own text, at most maxCallers of them, likeliest
// first (query.orderCallers), then a count of the rest; "" when there
// are none, so the section is omitted like any other absent attribute.
// One site a line, not one line of path:line pairs (050's original
// shape), because directive 051 §1 added what 050 did not: the call's
// own text, the same fact a grep of the name returns and a bare site
// does not — a line long enough to hold that for five sites is not one
// line any reader would rather have compressed.
//
// Directive 072: calls are the sites attributed to the declaration
// (query.attributeCallers); elsewhere is how many sites of the name were
// not, printed as one count — under the list, or alone when no site is
// this declaration's own — so a name declared in many places says how
// many sites it has without spending five lines on other declarations'.
//
// Directive 072 (change G), 073: a site where the name is selected but
// not called — a field read or set, its address taken, a constant or
// variable referred to — is a use, and prints under uses: the same way,
// after the calls; a field or constant has only uses, a function only
// calls, and each block is capped on its own.
func callersLine(calls []entry.Call, elsewhere int) string {
	if len(calls) == 0 && elsewhere == 0 {
		return ""
	}
	if len(calls) == 0 {
		return fmt.Sprintf("  callers: none of its own; %d of the name elsewhere\n", elsewhere)
	}
	var b strings.Builder
	b.WriteString(sitesBlock("callers", calls, false))
	b.WriteString(sitesBlock("uses", calls, true))
	if elsewhere > 0 {
		fmt.Fprintf(&b, "    +%d of the name elsewhere\n", elsewhere)
	}
	return b.String()
}

// sitesBlock renders the sites of calls whose Use is use under header,
// one a line, at most maxCallers of them then a count of the rest; ""
// when there are none. A site in a test file is counted, not listed,
// when the code has sites too (directive 073): the code's sites are
// where the name is used, and a test's line repeats one of them.
func sitesBlock(header string, calls []entry.Call, use bool) string {
	var code, tests []entry.Call
	for _, c := range calls {
		if c.Use != use {
			continue
		}
		if c.Test {
			tests = append(tests, c)
		} else {
			code = append(code, c)
		}
	}
	shown, counted := code, len(tests)
	if len(code) == 0 {
		shown, counted = tests, 0
	}
	if len(shown) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "  %s:\n", header)
	for i, c := range shown {
		if i == maxCallers {
			fmt.Fprintf(&b, "    +%d more\n", len(shown)-i)
			break
		}
		if c.In != "" {
			fmt.Fprintf(&b, "    %s in %s: %s\n", c.Place(), c.In, c.Text)
			continue
		}
		fmt.Fprintf(&b, "    %s: %s\n", c.Place(), c.Text)
	}
	if counted > 0 {
		fmt.Fprintf(&b, "    +%d in tests\n", counted)
	}
	return b.String()
}

// docLine is the line of e's documentation a result prints (directive
// 065): the one that carries the most of words, the first line when
// none carries any.
func docLine(e entry.Entry, words []string) string {
	text := e.DocText
	if text == "" {
		return e.Doc
	}
	best, bestN := e.Doc, 0
	for _, l := range strings.Split(text, "\n") {
		ll, n := strings.ToLower(l), 0
		for _, w := range words {
			if w != "" && strings.Contains(ll, strings.ToLower(w)) {
				n++
			}
		}
		if n > bestN {
			best, bestN = strings.TrimSpace(l), n
		}
	}
	return best
}

// countsLine says how many entries each of several ranked words matched
// on its own (directive 065) — "none" for a word that matched nothing.
func countsLine(words []string, counts []int) string {
	parts := make([]string, len(words))
	for i, w := range words {
		if counts[i] == 0 {
			parts[i] = quote(w) + " none"
			continue
		}
		parts[i] = fmt.Sprintf("%s %d", quote(w), counts[i])
	}
	return "entries per word: " + strings.Join(parts, " | ") + "\n"
}

func quote(s string) string {
	if strings.ContainsAny(s, " \t") {
		return "'" + s + "'"
	}
	return s
}

// fit selects the entries of page: a prefix of what the pages before
// it left, within PageSize and, once at least one result is shown,
// within byteBudget total for the answer. preambleLen is every byte the
// caller has already printed or is about to print ahead of the first
// result line — §7's ## index line, the optional languages: and by ext:
// lines, and the ## results header itself — not indexLine's length
// alone (§26 C-30: "everything printed counts").
//
// A page beyond the last yields no results and no remainder, not an
// error — the caller asked to see further than there is anything to
// see.
func fit(matches []entry.Entry, showProject, showLang bool, extVaries map[string]bool, preambleLen, page int) (shown, remainder []entry.Entry) {
	return fitShare(matches, nil, showProject, showLang, extVaries, preambleLen, page, 1)
}

// fitShare is fit for one of share words sharing one page (directive
// 062, h3): each gets its share of the results and of the byte budget,
// never fewer than minShareResults results or minShareBudget bytes, so a
// six-word call costs about what a one-word call does.
//
// Page N holds what comes after page N-1's last result (directive 072).
// Until now page N began at entry (N-1)*PageSize whatever the pages
// before it had fitted, and the byte budget fits fewer than PageSize
// results whenever their lines are long: on kubernetes `giraffe find
// prefix` showed two results on page 1, one on page 2, and the five
// entries between them — a kubectl field named Prefix among them — on
// no page at all. Every result is now on exactly one page; the page a
// result is on is found by fitting the pages before it, the same
// fitting that printed them.
func fitShare(matches []entry.Entry, words []string, showProject, showLang bool, extVaries map[string]bool, preambleLen, page, share int) (shown, remainder []entry.Entry) {
	limit, budget := max(PageSize/share, minShareResults), max(byteBudget/share, minShareBudget)
	start := 0
	for p := 1; ; p++ {
		if start >= len(matches) {
			return nil, nil
		}
		n := fitCount(matches[start:], words, showProject, showLang, extVaries, preambleLen, limit, budget)
		if p == page {
			return matches[start : start+n], matches[start+n:]
		}
		start += n
	}
}

// fitCount is how many of matches, from the first, one page holds: at
// most limit, and within budget once one is shown — so at least one
// while there is one, and a walk over pages always moves.
func fitCount(matches []entry.Entry, words []string, showProject, showLang bool, extVaries map[string]bool, preambleLen, limit, budget int) int {
	total := preambleLen
	for i, e := range matches {
		if i >= limit {
			return i
		}
		line := resultLine(e, words, showProject, showLang, extVaries)
		if i > 0 && total+len(line) > budget {
			return i
		}
		total += len(line)
	}
	return len(matches)
}

// Call is the find call an answer is for, as typed (directive 071):
// --query's value, and the words after find. Every command an answer
// prints for the caller to type back — the next page, a narrowing — is
// composed from it in the one grammar, options before find and the
// words after, so it can be pasted as it stands.
type Call struct {
	Query string
	Words []string
}

// Command renders this call with query in place of its own --query and
// page in place of its own --page.
func (c Call) Command(query string, page int) string {
	parts := []string{"giraffe"}
	if query != "" {
		parts = append(parts, "--query", ShellWord(query))
	}
	if page > 1 {
		parts = append(parts, "--page", strconv.Itoa(page))
	}
	parts = append(parts, "find")
	for _, w := range c.Words {
		parts = append(parts, ShellWord(w))
	}
	return strings.Join(parts, " ")
}

// narrowed is this call's --query with its path: values replaced by
// dir — a directory every remaining result already lies in, so the
// narrowed query holds what the wider one held there and nothing more.
func (c Call) narrowed(dir string) string {
	var kept []string
	for _, tok := range queryTokens(c.Query) {
		if !strings.HasPrefix(tok, "path:") {
			kept = append(kept, tok)
		}
	}
	kept = append(kept, "path:"+dir)
	return strings.Join(kept, " ")
}

// queryTokens splits a --query value the way query.Parse does: on
// spaces, a value in single quotes kept whole.
func queryTokens(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\'':
			inQuote = !inQuote
			cur.WriteByte(c)
		case c == ' ' && !inQuote:
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// ShellWord quotes s for a POSIX shell when it needs it, so a printed
// command pastes as it stands.
func ShellWord(s string) string {
	if s == "" {
		return "''"
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_-./:=+@%,", r)) {
			return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	return s
}

// truncatedLines reports the current page's position and the remaining
// count, then the commands to type next, each pasteable as it stands
// (§7: "a bare count does not tell the caller what to do next";
// directive 033: "page X" in the unit --page takes, and the count left,
// exact — the pages left are not, since a page holds what its budget
// fits (directive 072, fitShare); directive 071:
// every command giraffe prints is in its grammar): the next page, and
// the same call narrowed to each of the directories holding most of
// the remainder — path: is what --query narrows by (033's own
// constraint: the distribution axis is not what this measures).
// Files at the root have no directory to narrow to, so "." is not
// offered.
//
// A command already printed on the page (every word's answer is the one
// call, so its next page and its narrowings are the same commands) is
// not printed again: the line keeps its count and says the command is
// above.
func truncatedLines(remainder []entry.Entry, page int, call Call, printed map[string]bool) string {
	counts := make(map[string]int)
	for _, e := range remainder {
		if d := path.Dir(e.Path); d != "." {
			counts[d]++
		}
	}
	dirs := make([]string, 0, len(counts))
	for d := range counts {
		dirs = append(dirs, d)
	}
	sort.Slice(dirs, func(i, j int) bool {
		if counts[dirs[i]] != counts[dirs[j]] {
			return counts[dirs[i]] > counts[dirs[j]]
		}
		return dirs[i] < dirs[j]
	})
	if len(dirs) > narrowByN {
		dirs = dirs[:narrowByN]
	}

	var b strings.Builder
	command := func(c string) string {
		if printed[c] {
			return "as above"
		}
		printed[c] = true
		return c
	}
	fmt.Fprintf(&b, "page %d, %d more: %s\n", page, len(remainder), command(call.Command(call.Query, page+1)))
	for _, d := range dirs {
		fmt.Fprintf(&b, "%d in %s: %s\n", counts[d], d, command(call.Command(call.narrowed(d), 1)))
	}
	return b.String()
}
