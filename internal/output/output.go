// Package output renders one giraffe find answer: section-headed plain
// text, sections omitted when empty (specifications/giraffe.md §7).
package output

import (
	"fmt"
	"iter"
	"path"
	"sort"
	"strings"

	"giraffe/internal/entry"
)

const (
	maxResults = 8
	byteBudget = 2000
	narrowByN  = 3 // how many groups the truncated section suggests narrowing by

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
)

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
	// Languages and ByExt are the two ## index breakdowns (§25 C-24):
	// what turns "0 symbols" into "go 0 | unknown 181099" for a
	// repository giraffe cannot extract from at all, plus the raw
	// extension counts underneath it — a fact requiring no judgement,
	// unlike the guess an extension-to-language table would encode.
	// Pre-rendered here, not left as raw counts, so Format stays a pure
	// renderer.
	Languages  string
	ByExt      string
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
		Warnings:             warnings,
		Notices:              notices,
		Exclusions:           exclusions,
	}
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
	if page < 1 {
		page = 1
	}
	totalPages := pageCount(len(matches))
	offset := (page - 1) * maxResults

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

	// §26 C-30: the byte budget is a safety limit, and a safety limit
	// with exceptions is not one. Everything printed ahead of ## results
	// counts against it — b.Len() at this point is exactly that: the
	// "## index" header, the index line, and (when present) the
	// languages: and by-ext: lines, none of which fit's own preambleLen
	// used to see. The "## results" header itself is also printed, but
	// only once shown turns out non-empty, so its length is added here
	// too, conditional on there being anything to show.
	preambleLen := b.Len()
	const resultsHeader = "\n## results\n"
	if len(matches) > 0 {
		preambleLen += len(resultsHeader)
	}
	shown, remainder := fit(matches, s.ShowProject, s.ShowLang, s.ExtVaries, preambleLen, offset)

	if len(shown) > 0 {
		b.WriteString(resultsHeader)
		for _, e := range shown {
			b.WriteString(resultLine(e, s.ShowProject, s.ShowLang, s.ExtVaries))
		}
	}

	if len(remainder) > 0 {
		b.WriteString("\n## truncated\n")
		b.WriteString(truncatedLine(remainder, page, totalPages))
		b.WriteByte('\n')
	}

	// §22: `## truncated` already tells a caller how to narrow the same
	// question. Nothing told it how to switch to a different one, and a
	// zero-result answer carried no direction at all. Only emitted when
	// there is something to show — TryNext is populated exclusively from
	// evidence in the index (query.Suggest), never a fixed list, so an
	// unfed empty search stays silent rather than inventing advice.
	if len(matches) == 0 && len(s.TryNext) > 0 {
		b.WriteString("\n## try next\n")
		for _, line := range s.TryNext {
			fmt.Fprintf(&b, "%s\n", line)
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
// exactly what this section exists to avoid.
func resultLine(e entry.Entry, showProject, showLang bool, extVaries map[string]bool) string {
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
	for _, p := range e.Position {
		fmt.Fprintf(&line, " %s:%s", p.Name, quote(p.Value))
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
	if e.Doc != "" {
		fmt.Fprintf(&line, "  %s\n", e.Doc)
	}
	return line.String()
}

func quote(s string) string {
	if strings.ContainsAny(s, " \t") {
		return "'" + s + "'"
	}
	return s
}

// pageCount reports how many --page-sized pages total matches spans, the
// nominal count FormatFrom shows in ## truncated's "page X of Y" (§7,
// directive 033): pages are sized by maxResults, the stable unit a page
// number converts to and from, even though fit's own byteBudget can
// occasionally show fewer than maxResults on one particular page — the
// caller's next --page N is still N, not something recomputed from how
// many bytes an earlier page happened to fit.
func pageCount(total int) int {
	if total == 0 {
		return 0
	}
	return (total + maxResults - 1) / maxResults
}

// fit selects a prefix of matches that stays within maxResults and, once
// at least one result is shown, within byteBudget total for the answer.
// preambleLen is every byte the caller has already printed or is about
// to print ahead of the first result line — §7's ## index line, the
// optional languages: and by ext: lines, and the ## results header
// itself — not indexLine's length alone (§26 C-30: "everything printed
// counts").
//
// offset skips the first offset entries of matches before the budget is
// applied — FormatFrom's page-to-offset conversion (directive 033;
// offset was directive 032's own public unit, --page is now the only
// public one). matches already carries the full ranking, so paging is a
// plain slice, not a second search. offset past the end of matches
// yields no results and no remainder, not an error — the caller asked to
// see further than there is anything to see.
func fit(matches []entry.Entry, showProject, showLang bool, extVaries map[string]bool, preambleLen, offset int) (shown, remainder []entry.Entry) {
	if offset >= len(matches) {
		return nil, nil
	}
	matches = matches[offset:]
	total := preambleLen
	for i, e := range matches {
		if i >= maxResults {
			return matches[:i], matches[i:]
		}
		line := resultLine(e, showProject, showLang, extVaries)
		if i > 0 && total+len(line) > byteBudget {
			return matches[:i], matches[i:]
		}
		total += len(line)
	}
	return matches, nil
}

// truncatedLine reports the current page's position, the remaining
// count, and a distribution to narrow by (§7: "a bare count does not
// tell the caller what to do next"; directive 033: the count alone also
// never said *where* the shown page sits, or that the remainder has an
// order that can be walked — "page X of Y" is that, in the same unit
// --page takes). Narrows by path directory rather than language
// position, since --query only supports filtering by path and project —
// unchanged from before this directive (033's own constraint: the
// distribution axis is not what this measures).
func truncatedLine(remainder []entry.Entry, page, totalPages int) string {
	counts := make(map[string]int)
	for _, e := range remainder {
		counts[path.Dir(e.Path)]++
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
	fmt.Fprintf(&b, "page %d of %d, %d more. narrow by path:", page, totalPages, len(remainder))
	for _, d := range dirs {
		fmt.Fprintf(&b, " %s (%d)", d, counts[d])
	}
	return b.String()
}
