package output

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"giraffe/internal/entry"
)

func TestFormat_IndexLineAlwaysPresent_EmptySectionsOmitted(t *testing.T) {
	s := Summarize(slices.Values[[]entry.Entry](nil), "abc1234", 0, nil, nil, nil, nil)
	out := Format(s, nil)
	if !strings.Contains(out, "## index") {
		t.Fatalf("missing ## index in:\n%s", out)
	}
	if strings.Contains(out, "## results") {
		t.Errorf("expected ## results to be omitted when there are no matches:\n%s", out)
	}
	if strings.Contains(out, "## truncated") {
		t.Errorf("expected ## truncated to be omitted:\n%s", out)
	}
	if strings.Contains(out, "## warnings") {
		t.Errorf("expected ## warnings to be omitted:\n%s", out)
	}
}

func TestFormat_ResultsSection_PasteReadyAttributes(t *testing.T) {
	entries := []entry.Entry{
		{Path: "internal/retry/retry.go", Name: "Retry", Kind: "func", Doc: "retries with backoff", Exported: true,
			Position: []entry.Position{{Name: "module", Value: "internal/retry"}}},
	}
	s := Summarize(slices.Values(entries), "abc1234", 1, nil, nil, nil, nil)
	out := Format(s, entries)
	// module: is not on the line: Go's module is the path's own
	// directory, which the line already carries (directive 047).
	if !strings.Contains(out, "name:Retry kind:func path:internal/retry/retry.go\n") {
		t.Fatalf("result line not paste-ready:\n%s", out)
	}
	if !strings.Contains(out, "  retries with backoff") {
		t.Fatalf("doc line missing or not indented:\n%s", out)
	}
}

// TestFormat_PositionThatRepeatsThePathIsNotPrinted (directive 047 §3):
// Go's module position is path.Dir of the entry's own path, so printing
// it restates a part of the line the reader already has — 1,269 bytes of
// the 24,050 one measured aided run was given. A position that says
// something else still prints.
func TestFormat_PositionThatRepeatsThePathIsNotPrinted(t *testing.T) {
	entries := []entry.Entry{
		{Path: "internal/cluster/cluster.go", Name: "Snapshot", Kind: "method", Exported: true,
			Position: []entry.Position{{Name: "module", Value: "internal/cluster"}, {Name: "receiver", Value: "Cluster"}}},
		{Path: "cluster.go", Name: "Start", Kind: "func", Exported: true,
			Position: []entry.Position{{Name: "module", Value: "."}}},
		{Path: "internal/cluster/cluster.go", Name: "Stop", Kind: "func", Exported: true,
			Position: []entry.Position{{Name: "module", Value: "github.com/example/cluster"}}},
	}
	s := Summarize(slices.Values(entries), "abc1234", 3, nil, nil, nil, nil)
	out := Format(s, entries)
	if strings.Contains(out, "module:internal/cluster") {
		t.Errorf("module: repeats the path's directory and should not print:\n%s", out)
	}
	if strings.Contains(out, "module:.") {
		t.Errorf("a root file's module: is \".\", which repeats the path's directory:\n%s", out)
	}
	if !strings.Contains(out, "receiver:Cluster") {
		t.Errorf("receiver: says what the path does not, and should print:\n%s", out)
	}
	if !strings.Contains(out, "module:github.com/example/cluster") {
		t.Errorf("a module: that is not the path's directory should print:\n%s", out)
	}
}

// TestFormat_LineNumberAndLineTextBothPrint (§31.4): a result line
// carries line:N in the attribute line, and the declaration's own
// source line as a second indented line above Doc — both present when
// both are known.
func TestFormat_LineNumberAndLineTextBothPrint(t *testing.T) {
	entries := []entry.Entry{
		{Path: "internal/retry/retry.go", Name: "Retry", Kind: "func", Doc: "retries with backoff", Exported: true,
			Line: 14, LineText: "func Retry(ctx context.Context) error {"},
	}
	s := Summarize(slices.Values(entries), "abc1234", 1, nil, nil, nil, nil)
	out := Format(s, entries)
	if !strings.Contains(out, "name:Retry kind:func path:internal/retry/retry.go line:14") {
		t.Fatalf("expected line:14 in the attribute line:\n%s", out)
	}
	if !strings.Contains(out, "  func Retry(ctx context.Context) error {\n") {
		t.Fatalf("expected the declaration's own line, indented:\n%s", out)
	}
	if !strings.Contains(out, "  retries with backoff\n") {
		t.Fatalf("expected Doc to still print alongside the line text:\n%s", out)
	}
}

// TestFormat_LineIsTheDeclarationsRange (directive 042): a declaration
// spanning lines prints line:A-B — the two numbers `sed -n 'A,Bp'`
// takes — and a one-line declaration prints line:A alone, not A-A.
func TestFormat_LineIsTheDeclarationsRange(t *testing.T) {
	entries := []entry.Entry{
		{Path: "internal/core/metrics.go", Name: "NodeMetrics", Kind: "type", Exported: true, Line: 19, EndLine: 62},
		{Path: "internal/core/metrics.go", Name: "itoa", Kind: "func", Exported: false, Scope: "package", Line: 166, EndLine: 166},
	}
	s := Summarize(slices.Values(entries), "abc1234", 1, nil, nil, nil, nil)
	out := Format(s, entries)
	if !strings.Contains(out, "name:NodeMetrics kind:type path:internal/core/metrics.go line:19-62") {
		t.Fatalf("expected line:19-62 on the ranged declaration:\n%s", out)
	}
	if !strings.Contains(out, "name:itoa kind:func path:internal/core/metrics.go line:166 ") {
		t.Fatalf("expected line:166 alone on the one-line declaration:\n%s", out)
	}
}

// TestFormat_NoLineForAFileEntry (§31.4, §25 C-24): a generic file
// entry addresses no specific line — absence is the correct encoding,
// not a placeholder like line:0.
func TestFormat_NoLineForAFileEntry(t *testing.T) {
	entries := []entry.Entry{
		{Path: "README.md", Name: "README", Kind: entry.KindFile, Exported: true},
	}
	s := Summarize(slices.Values(entries), "abc1234", 0, nil, nil, nil, nil)
	out := Format(s, entries)
	if strings.Contains(out, "line:") {
		t.Fatalf("expected no line: attribute for a file entry:\n%s", out)
	}
}

func TestFormat_TruncatesAtEightResults(t *testing.T) {
	var entries []entry.Entry
	for i := 0; i < 12; i++ {
		entries = append(entries, entry.Entry{Path: "a/b.go", Name: "X", Kind: "func"})
	}
	s := Summarize(slices.Values(entries), "abc1234", 0, nil, nil, nil, nil)
	out := Format(s, entries)
	if got := strings.Count(out, "name:X"); got != PageSize {
		t.Fatalf("expected exactly %d result lines, got %d:\n%s", PageSize, got, out)
	}
	if !strings.Contains(out, "## truncated") {
		t.Fatalf("expected ## truncated for 12 matches:\n%s", out)
	}
	if !strings.Contains(out, "4 more: giraffe --page 2 find") {
		t.Fatalf("expected 4 more (12-8) in truncated section:\n%s", out)
	}
}

func TestFormat_QuotesValuesWithWhitespace(t *testing.T) {
	entries := []entry.Entry{{Path: "a b.go", Name: "X", Kind: "func"}}
	s := Summarize(slices.Values(entries), "abc1234", 0, nil, nil, nil, nil)
	out := Format(s, entries)
	if !strings.Contains(out, "path:'a b.go'") {
		t.Fatalf("expected a whitespace-containing value to be single-quoted:\n%s", out)
	}
}

func TestFormat_ScopePackageIsLabelled_ExportedIsImplicit(t *testing.T) {
	entries := []entry.Entry{
		{Path: "a.go", Name: "Exported", Kind: "func", Scope: "exported", Exported: true},
		{Path: "b.go", Name: "unexported", Kind: "func", Scope: "package", Exported: false},
	}
	s := Summarize(slices.Values(entries), "abc1234", 0, nil, nil, nil, nil)
	out := Format(s, entries)
	if !strings.Contains(out, "name:unexported kind:func path:b.go scope:package") {
		t.Fatalf("expected scope:package on the unexported entry's line:\n%s", out)
	}
	if strings.Contains(out, "name:Exported kind:func path:a.go scope:") {
		t.Fatalf("expected no scope: attribute on the exported entry's line (exported is implicit):\n%s", out)
	}
}

// TestFormat_TestIsLabelled_ProductionIsImplicit (directive 051 §1):
// the same "only the informative case costs a line" rule as scope
// above — test:true prints, test:false never does.
func TestFormat_TestIsLabelled_ProductionIsImplicit(t *testing.T) {
	entries := []entry.Entry{
		{Path: "a.go", Name: "Prod", Kind: "func", Exported: true},
		{Path: "a_test.go", Name: "TestProd", Kind: "func", Exported: true, Test: true},
	}
	s := Summarize(slices.Values(entries), "abc1234", 0, nil, nil, nil, nil)
	out := Format(s, entries)
	if !strings.Contains(out, "name:TestProd kind:func path:a_test.go test:true") {
		t.Fatalf("expected test:true on the test entry's line:\n%s", out)
	}
	if strings.Contains(out, "name:Prod kind:func path:a.go test:") {
		t.Fatalf("expected no test: attribute on the production entry's line:\n%s", out)
	}
}

func TestFormat_UndocumentedReportedByScope(t *testing.T) {
	entries := []entry.Entry{
		{Path: "a.go", Name: "A", Kind: "func", Exported: true, Doc: "documented"},
		{Path: "b.go", Name: "B", Kind: "func", Exported: true}, // undocumented, exported
		{Path: "c.go", Name: "c", Kind: "func", Exported: false},
		{Path: "d.go", Name: "d", Kind: "func", Exported: false},
	}
	s := Summarize(slices.Values(entries), "abc1234", 0, nil, nil, nil, nil)
	out := Format(s, nil)
	// §22 C-11: one mixed count reads as a regression that did not
	// happen once package-scope entries (documented far less often)
	// join the index. Report both, without adding separate labels.
	if !strings.Contains(out, "undocumented 1 exported, 2 package") {
		t.Fatalf("expected undocumented counts split by scope:\n%s", out)
	}
}

func TestFormat_TryNextOnlyRendersWhenEmptyAndPopulated(t *testing.T) {
	s := Summarize(slices.Values[[]entry.Entry](nil), "abc1234", 0, nil, nil, nil, nil)
	s.TryNext = []string{"names in the index starting the same way: Retry"}
	out := Format(s, nil)
	if !strings.Contains(out, "## try next\nnames in the index starting the same way: Retry\n") {
		t.Fatalf("expected a ## try next section for a zero-result answer:\n%s", out)
	}

	entries := []entry.Entry{{Path: "a.go", Name: "X", Kind: "func", Exported: true}}
	s2 := Summarize(slices.Values(entries), "abc1234", 0, nil, nil, nil, nil)
	s2.TryNext = []string{"should not appear"}
	out2 := Format(s2, entries)
	if strings.Contains(out2, "## try next") {
		t.Fatalf("expected no ## try next section when there are matches:\n%s", out2)
	}

	s3 := Summarize(slices.Values[[]entry.Entry](nil), "abc1234", 0, nil, nil, nil, nil)
	out3 := Format(s3, nil)
	if strings.Contains(out3, "## try next") {
		t.Fatalf("expected no ## try next section when nothing was found to offer:\n%s", out3)
	}
}

// TestFormat_FileEntriesDoNotCountAsUndocumented (§22 item 4): a file
// entry always carries Doc "" and Exported true, but it is not a
// declaration M11 governs — counting it would inflate "undocumented
// exported" with something that was never meant to carry documentation.
func TestFormat_FileEntriesDoNotCountAsUndocumented(t *testing.T) {
	entries := []entry.Entry{
		{Path: "a.go", Name: "a", Kind: entry.KindFile, Exported: true},
		{Path: "a.go", Name: "A", Kind: "func", Exported: true, Doc: "documented"},
	}
	s := Summarize(slices.Values(entries), "abc1234", 0, nil, nil, nil, nil)
	out := Format(s, nil)
	if !strings.Contains(out, "undocumented 0 exported, 0 package") {
		t.Fatalf("expected the file entry to be excluded from the undocumented count:\n%s", out)
	}
}

// TestFormat_LanguageBreakdownAndLangAttribute (§25 C-24): ## index
// reports a per-language breakdown with an "unknown" bucket for
// entries with no known language, and a result line only carries
// lang: when more than one *known* language is actually present — the
// same treatment project already gets.
func TestFormat_LanguageBreakdownAndLangAttribute(t *testing.T) {
	entries := []entry.Entry{
		{Path: "a.go", Name: "A", Kind: "func", Exported: true, Ext: "go", Lang: "go"},
		{Path: "b.c", Name: "b", Kind: "func", Exported: true, Ext: "c", Lang: "c"},
		{Path: "README.md", Name: "README", Kind: entry.KindFile, Exported: true, Ext: "md", Lang: ""},
	}
	s := Summarize(slices.Values(entries), "abc1234", 0, nil, nil, nil, nil)
	out := Format(s, entries)
	if !strings.Contains(out, "languages: c 1 | go 1 | unknown 1") {
		t.Fatalf("expected a per-language breakdown line with an unknown bucket:\n%s", out)
	}
	if !strings.Contains(out, "lang:go") || !strings.Contains(out, "lang:c") {
		t.Fatalf("expected lang: on result lines once more than one known language is present:\n%s", out)
	}
}

func TestFormat_LangOmittedForASingleKnownLanguageIndex(t *testing.T) {
	entries := []entry.Entry{
		{Path: "a.go", Name: "A", Kind: "func", Exported: true, Ext: "go", Lang: "go"},
		{Path: "README.md", Name: "README", Kind: entry.KindFile, Exported: true, Ext: "md", Lang: ""},
	}
	s := Summarize(slices.Values(entries), "abc1234", 0, nil, nil, nil, nil)
	out := Format(s, entries)
	if strings.Contains(out, "lang:go") {
		t.Fatalf("expected no lang: attribute when only one known language is present:\n%s", out)
	}
	if !strings.Contains(out, "languages: go 1 | unknown 1") {
		t.Fatalf("expected the index breakdown to still name the one known language and the unknown bucket:\n%s", out)
	}
}

// TestFormat_ExtOmittedWhenRecoverableFromLang_PrintedWhenItVaries
// (§25 C-25): ext is dropped when a language declares (in this index)
// exactly one extension — recoverable from lang alone — and printed
// when the same lang was observed with more than one, or when lang is
// unknown and ext is all there is.
func TestFormat_ExtOmittedWhenRecoverableFromLang_PrintedWhenItVaries(t *testing.T) {
	entries := []entry.Entry{
		{Path: "a.go", Name: "A", Kind: "func", Exported: true, Ext: "go", Lang: "go"},
		{Path: "b.h", Name: "B", Kind: "func", Exported: true, Ext: "h", Lang: "c"},
		{Path: "c.c", Name: "C", Kind: "func", Exported: true, Ext: "c", Lang: "c"},
		{Path: "README", Name: "README", Kind: entry.KindFile, Exported: true, Ext: "", Lang: ""},
	}
	s := Summarize(slices.Values(entries), "abc1234", 0, nil, nil, nil, nil)
	out := Format(s, entries)
	if strings.Contains(out, "ext:go") {
		t.Fatalf("expected ext:go to be omitted (go has one observed extension, recoverable from lang):\n%s", out)
	}
	if !strings.Contains(out, "ext:h") || !strings.Contains(out, "ext:c") {
		t.Fatalf("expected ext:h and ext:c (c was observed with two extensions):\n%s", out)
	}
	if !strings.Contains(out, "by ext:    (none) 1 | c 1 | go 1 | h 1") {
		t.Fatalf("expected a by-ext breakdown line including the extensionless file:\n%s", out)
	}
}

// TestFormat_ExtPrintsAloneWhenLangUnknown (§25 C-25): an unknown-lang
// file has nothing else to identify it by, so ext prints even in an
// index where every other file's extension happens to be unique to its
// (unknown) bucket too — the "lang unknown" case is unconditional, not
// gated on whether ext varies.
func TestFormat_ExtPrintsAloneWhenLangUnknown(t *testing.T) {
	entries := []entry.Entry{{Path: "notes.md", Name: "notes", Kind: entry.KindFile, Exported: true, Ext: "md", Lang: ""}}
	s := Summarize(slices.Values(entries), "abc1234", 0, nil, nil, nil, nil)
	out := Format(s, entries)
	if !strings.Contains(out, "ext:md") {
		t.Fatalf("expected ext:md to print when lang is unknown:\n%s", out)
	}
}

// TestFit_PreambleLenCountsTowardBudget (§26 C-30): fit's caller is
// responsible for passing every byte already printed ahead of the
// first result line, not indexLine's length alone. A larger preamble
// must leave room for fewer results before the byte backstop bites —
// the mechanism Format now relies on to count the ## index header, the
// languages: line, and the by ext: line, none of which used to count.
func TestFit_PreambleLenCountsTowardBudget(t *testing.T) {
	var matches []entry.Entry
	for i := 0; i < 20; i++ {
		matches = append(matches, entry.Entry{Path: strings.Repeat("a", 200), Name: "X", Kind: "func", Exported: true})
	}
	shownSmallPreamble, _ := fit(matches, false, false, nil, 10, 1)
	shownBigPreamble, _ := fit(matches, false, false, nil, 1200, 1)
	if len(shownBigPreamble) >= len(shownSmallPreamble) {
		t.Fatalf("expected a larger preamble to leave room for fewer results before the byte backstop: small=%d big=%d",
			len(shownSmallPreamble), len(shownBigPreamble))
	}
}

// TestFit_Page (directive 072, after 032's offset): page 2 begins at
// the entry after page 1's last, and the remainder shrinks by exactly
// what page 1 showed — paging is a walk over the existing ranking, not
// a second search with its own truncation count.
func TestFit_Page(t *testing.T) {
	var matches []entry.Entry
	for i := 0; i < 20; i++ {
		matches = append(matches, entry.Entry{Path: "a.go", Name: "X", Kind: "func", Exported: true, Line: i + 1})
	}
	shown1, remainder1 := fit(matches, false, false, nil, 10, 1)
	shown2, remainder2 := fit(matches, false, false, nil, 10, 2)
	if len(shown1) != PageSize || len(shown2) != PageSize {
		t.Fatalf("short lines: want full pages of %d, got %d and %d", PageSize, len(shown1), len(shown2))
	}
	if shown2[0].Line != matches[len(shown1)].Line {
		t.Fatalf("expected page 2 to start at the entry after page 1's last (%d), got %d", matches[len(shown1)].Line, shown2[0].Line)
	}
	if len(remainder2) != len(remainder1)-len(shown2) {
		t.Fatalf("expected the remainder to shrink by what page 2 shows: got %d, want %d", len(remainder2), len(remainder1)-len(shown2))
	}
}

// TestFit_PageBeyondEnd (directive 032): a page beyond the ranked set is
// not an error — it means there is nothing further to page to.
func TestFit_PageBeyondEnd(t *testing.T) {
	matches := []entry.Entry{{Path: "a.go", Name: "X", Kind: "func", Exported: true}}
	shown, remainder := fit(matches, false, false, nil, 10, 5)
	if shown != nil || remainder != nil {
		t.Fatalf("expected no shown and no remainder past the end of matches: shown=%v remainder=%v", shown, remainder)
	}
}

// TestFit_EveryResultOnExactlyOnePage (directive 072): when the byte
// budget fits fewer than PageSize results on a page, the next page
// starts where it stopped. Before, page N began at entry (N-1)*PageSize
// regardless, and the entries between the budget's cut and that point
// were on no page: on kubernetes `giraffe find prefix` showed two
// results on page 1, one on page 2, and five in between never.
func TestFit_EveryResultOnExactlyOnePage(t *testing.T) {
	var matches []entry.Entry
	for i := 0; i < 20; i++ {
		matches = append(matches, entry.Entry{Path: strings.Repeat("p", 600) + ".go", Name: fmt.Sprintf("Sym%d", i), Kind: "func", Exported: true, Line: i + 1})
	}
	seen := make(map[int]int)
	for page := 1; ; page++ {
		shown, _ := fit(matches, false, false, nil, 10, page)
		if len(shown) == 0 {
			break
		}
		if len(shown) >= PageSize {
			t.Fatalf("page %d: lines of 600 bytes should fit fewer than %d results in %d bytes, got %d", page, PageSize, byteBudget, len(shown))
		}
		for _, e := range shown {
			seen[e.Line]++
		}
		if page > 20 {
			t.Fatal("paging does not end")
		}
	}
	for _, e := range matches {
		if seen[e.Line] != 1 {
			t.Errorf("entry %d is on %d pages, want 1", e.Line, seen[e.Line])
		}
	}
}

// TestFormatFrom_Page (directive 033, replacing 032's --offset): page 1
// (Format's default) shows the first PageSize results and reports its
// own position in ## truncated; page 2 picks up exactly where page 1's
// budget stopped, in the same unit.
func TestFormatFrom_Page(t *testing.T) {
	var entries []entry.Entry
	for i := 0; i < 10; i++ {
		entries = append(entries, entry.Entry{
			Path: strings.Repeat("x", i+1) + ".go",
			Name: fmt.Sprintf("Sym%d", i),
			Kind: "func", Exported: true,
		})
	}
	s := Summarize(slices.Values(entries), "abc1234", 0, nil, nil, nil, nil)

	page1 := Format(s, entries)
	if !strings.Contains(page1, "name:Sym0") || strings.Contains(page1, "name:Sym8") {
		t.Fatalf("expected page 1 (Format's default) to hold the first 8 results only:\n%s", page1)
	}
	if !strings.Contains(page1, "page 1, 2 more: giraffe --page 2 find") {
		t.Fatalf("expected ## truncated to state page 1 and the count left:\n%s", page1)
	}

	page2 := FormatFrom(s, entries, 2)
	if strings.Contains(page2, "name:Sym0") {
		t.Fatalf("expected page 2 to skip page 1's results:\n%s", page2)
	}
	if !strings.Contains(page2, "name:Sym8") || !strings.Contains(page2, "name:Sym9") {
		t.Fatalf("expected page 2 to hold the remaining 2 results:\n%s", page2)
	}
	if strings.Contains(page2, "## truncated") {
		t.Fatalf("expected no ## truncated on the last page:\n%s", page2)
	}
}

// TestFormatFrom_PageBeyondEnd (directive 033): a page past the last one
// is not an error — empty results, no truncated section, same contract
// 032's offset-past-the-end had.
func TestFormatFrom_PageBeyondEnd(t *testing.T) {
	entries := []entry.Entry{{Path: "a.go", Name: "First", Kind: "func", Exported: true}}
	s := Summarize(slices.Values(entries), "abc1234", 0, nil, nil, nil, nil)
	out := FormatFrom(s, entries, 5)
	if strings.Contains(out, "## results") || strings.Contains(out, "## truncated") {
		t.Fatalf("expected an empty page past the end:\n%s", out)
	}
}

// TestFormat_PreambleIncludesIndexHeaderAndBreakdownLines (§26 C-30):
// the exact bug named in the report — indexLine's own length was
// counted, but the "## index\n" header text and the languages:/by ext:
// lines that print ahead of ## results were counted by nothing. Format
// must now pass fit a preamble length that actually reflects everything
// already written to the builder.
func TestFormat_PreambleIncludesIndexHeaderAndBreakdownLines(t *testing.T) {
	s := Summary{
		Commit:    "abc1234",
		Languages: "go 1 | unknown 1",
		ByExt:     "go 1 | md 1",
	}
	var matches []entry.Entry
	for i := 0; i < 20; i++ {
		matches = append(matches, entry.Entry{Path: strings.Repeat("a", 210), Name: "X", Kind: "func", Exported: true})
	}
	withBreakdown := Format(s, matches)

	s2 := s
	s2.Languages, s2.ByExt = "", ""
	withoutBreakdown := Format(s2, matches)

	if strings.Count(withBreakdown, "name:X") >= strings.Count(withoutBreakdown, "name:X") {
		t.Fatalf("expected the languages:/by ext: lines' bytes to reduce how many results fit near budget: with=%d without=%d",
			strings.Count(withBreakdown, "name:X"), strings.Count(withoutBreakdown, "name:X"))
	}
}

func TestFormat_WarningsAndExclusions(t *testing.T) {
	s := Summarize(slices.Values[[]entry.Entry](nil), "abc1234", 0, []string{"x.go: syntax error"}, nil, []string{"vendor/"}, nil)
	out := Format(s, nil)
	if !strings.Contains(out, "## warnings") {
		t.Fatalf("expected ## warnings section:\n%s", out)
	}
	if !strings.Contains(out, "unparsed: x.go: syntax error") {
		t.Fatalf("missing warning detail:\n%s", out)
	}
	if !strings.Contains(out, "active exclusions: vendor/") {
		t.Fatalf("missing active exclusions:\n%s", out)
	}
}

// TestFormat_KnownLanguageWithZeroMatchesStillNamed (§25 C-24): "go 0,
// unknown 181099" is the directive's own worked example for
// llvm-project — a registered language giraffe genuinely checked for
// and found nothing must still be named, distinct from a language it
// has no extractor for at all.
func TestFormat_KnownLanguageWithZeroMatchesStillNamed(t *testing.T) {
	entries := []entry.Entry{
		{Path: "README.md", Name: "README", Kind: entry.KindFile, Exported: true, Ext: "md", Lang: ""},
	}
	s := Summarize(slices.Values(entries), "abc1234", 0, nil, nil, nil, []string{"go"})
	out := Format(s, entries)
	if !strings.Contains(out, "languages: unknown 1 | go 0") {
		t.Fatalf("expected go to be named at 0 rather than omitted:\n%s", out)
	}
}

// TestResultLine_SizeAndDeclCount (directive 009 item 3; 043 for size
// on every file): a file result always prints size:, since the caller's
// next move with a file is to read it whole or not, and prints
// declarations: only when it holds enough of them to be worth narrowing
// with --query — §25 C-25's "only when it discriminates" applied to the
// one of the two that has a threshold that fires on a real corpus.
func TestResultLine_SizeAndDeclCount(t *testing.T) {
	tests := []struct {
		name string
		e    entry.Entry
		want []string
		none []string
	}{
		{
			name: "many declarations: both print",
			e:    entry.Entry{Path: "big.go", Name: "big", Kind: entry.KindFile, Exported: true, DeclCount: 678, Size: 5000},
			want: []string{"size:5000 declarations:678"},
		},
		{
			name: "large file, no extractor",
			e:    entry.Entry{Path: "big.td", Name: "big", Kind: entry.KindFile, Exported: true, DeclCount: 0, Size: 391229},
			want: []string{"size:391229"},
			none: []string{"declarations:"},
		},
		{
			name: "small file, few declarations: size prints, declarations does not",
			e:    entry.Entry{Path: "small.go", Name: "small", Kind: entry.KindFile, Exported: true, DeclCount: 3, Size: 1200},
			want: []string{"size:1200"},
			none: []string{"declarations:"},
		},
		{
			name: "a declaration entry (not a file) never carries either, even if the fields are set",
			e:    entry.Entry{Path: "big.go", Name: "Big", Kind: "func", Exported: true, DeclCount: 678, Size: 391229},
			none: []string{"size:", "declarations:"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := Summarize(slices.Values([]entry.Entry{tt.e}), "abc1234", 0, nil, nil, nil, nil)
			out := Format(s, []entry.Entry{tt.e})
			for _, w := range tt.want {
				if !strings.Contains(out, w) {
					t.Errorf("expected %q in:\n%s", w, out)
				}
			}
			for _, n := range tt.none {
				if strings.Contains(out, n) {
					t.Errorf("expected no %q in:\n%s", n, out)
				}
			}
		})
	}
}

// TestSummarize_PackagesCountsOnlyThePrimaryPosition (§28 C-38): a
// method or field carries a second Position pair (its receiver or
// containing type) after the file's shared module position. Packages
// must still count distinct modules, not grow with the number of
// distinct types that happen to have methods or fields.
func TestSummarize_PackagesCountsOnlyThePrimaryPosition(t *testing.T) {
	entries := []entry.Entry{
		{Name: "Cluster", Kind: "type", Exported: true,
			Position: []entry.Position{{Name: "module", Value: "internal/cluster"}}},
		{Name: "Snapshot", Kind: "method", Exported: true,
			Position: []entry.Position{{Name: "module", Value: "internal/cluster"}, {Name: "receiver", Value: "Cluster"}}},
		{Name: "actors", Kind: "field", Exported: false,
			Position: []entry.Position{{Name: "module", Value: "internal/cluster"}, {Name: "type", Value: "Cluster"}}},
	}
	s := Summarize(slices.Values(entries), "abc1234", 0, nil, nil, nil, nil)
	if s.Packages != 1 {
		t.Errorf("Packages = %d, want 1 (one module, despite two secondary position pairs)", s.Packages)
	}
}

func TestFormat_NoticesRenderWithoutTheUnparsedLabel(t *testing.T) {
	// A notice (e.g. "the local cache is not git-ignored") is not a parse
	// failure; labelling it "unparsed:" would be actively misleading.
	s := Summarize(slices.Values[[]entry.Entry](nil), "abc1234", 0, nil, []string{".giraffe/local is not ignored by git"}, nil, nil)
	out := Format(s, nil)
	if !strings.Contains(out, "\n.giraffe/local is not ignored by git\n") {
		t.Fatalf("expected the notice verbatim, without an unparsed: prefix:\n%s", out)
	}
	if strings.Contains(out, "unparsed: .giraffe/local") {
		t.Fatalf("a notice must not be labelled as a parse failure:\n%s", out)
	}
}

// TestFormatWords (directive 062): several words share one ## index and
// one ## warnings, each gets a block under its own header, a declaration
// already shown is not shown again, and a word that matched nothing says
// so and what it was matched against.
func TestFormatWords(t *testing.T) {
	x := entry.Entry{Path: "a/x.go", Name: "MailBox", Kind: "type", Line: 3, Exported: true}
	y := entry.Entry{Path: "a/y.go", Name: "Mailer", Kind: "func", Line: 9, Exported: true}
	s := Summary{Commit: "abc1234", Symbols: 42, Notices: []string{"a notice"}}
	out := FormatWords(s, []WordAnswer{
		{Word: "mail", Matches: []entry.Entry{x, y}},
		{Word: "box", Matches: []entry.Entry{x}},
		{Word: "smtp", TryNext: []string{"names in the index starting the same way: Smt"}},
	}, 1)
	for _, want := range []string{
		"## results: mail\nname:MailBox",
		"## results: box\nevery match is shown above\n",
		"## results: smtp\nnone: no name, doc comment or path among the index's 42 entries contains smtp",
		"## try next: smtp\nnames in the index",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("answer lacks %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "## index") != 1 || strings.Count(out, "a notice") != 1 || strings.Count(out, "name:MailBox") != 1 {
		t.Errorf("something printed more than once:\n%s", out)
	}
	one := FormatWords(s, []WordAnswer{{Word: "smtp"}}, 1)
	if !strings.Contains(one, "\n## results\nnone: ") {
		t.Errorf("a single word's empty answer does not say so:\n%s", one)
	}
}

// Directive 065: a result prints the doc line the search words are on,
// else the first.
func TestDocLine_TheLineTheWordsAreOn(t *testing.T) {
	e := entry.Entry{Doc: "Tell sends msg.", DocText: "Tell sends msg.\nA full mailbox sends it to dead letters.\nSee Ask."}
	if got := docLine(e, []string{"mailbox", "full"}); got != "A full mailbox sends it to dead letters." {
		t.Errorf("docLine = %q", got)
	}
	if got := docLine(e, []string{"smtp"}); got != "Tell sends msg." {
		t.Errorf("docLine with no word on any line = %q, want the first line", got)
	}
}

// Directive 065, g42: a result from a test file carries test:true, and
// one from production code says nothing.
func TestResultLine_TestOnlyWhenTrue(t *testing.T) {
	e := entry.Entry{Path: "a/x_test.go", Name: "TestX", Kind: "func", Line: 3, EndLine: 5, Exported: true, Test: true}
	if got := resultLine(e, nil, false, false, nil); !strings.Contains(got, " test:true") {
		t.Errorf("a test's result line lacks test:true: %q", got)
	}
	e.Test = false
	if got := resultLine(e, nil, false, false, nil); strings.Contains(got, "test:") {
		t.Errorf("a production result line names test: %q", got)
	}
}

// TestFormat_CallersLine (directive 050, extended by 051 §1 for each
// site's own text): a result with callers prints them under its doc
// line, one site a line as path:line: text, at most maxCallers with
// the rest counted; a result with none prints no such block; ## source
// carries the same block under its header.
func TestFormat_CallersLine(t *testing.T) {
	var sites []entry.Call
	for i := 1; i <= maxCallers+2; i++ {
		sites = append(sites, entry.Call{Path: "cluster.go", Name: "Snapshot", Line: i, Text: fmt.Sprintf("c.Snapshot(%d)", i)})
	}
	called := entry.Entry{Path: "m.go", Name: "Snapshot", Kind: "method", Exported: true, Doc: "Snapshot copies.", Callers: sites}
	quiet := entry.Entry{Path: "m.go", Name: "Quiet", Kind: "func", Exported: true}
	out := Format(Summary{Commit: "abc"}, []entry.Entry{called, quiet})
	want := "  Snapshot copies.\n  callers:\n    cluster.go:1: c.Snapshot(1)\n    cluster.go:2: c.Snapshot(2)\n" +
		"    cluster.go:3: c.Snapshot(3)\n    cluster.go:4: c.Snapshot(4)\n    cluster.go:5: c.Snapshot(5)\n    +2 more\n"
	if !strings.Contains(out, want) {
		t.Errorf("output lacks %q:\n%s", want, out)
	}
	if strings.Count(out, "callers:") != 1 {
		t.Errorf("a result with no callers printed a callers line:\n%s", out)
	}
	s := Summary{Commit: "abc", Sources: []Source{{Entry: called, Lines: []string{"func (m *M) Snapshot() {}"}}}}
	out = Format(s, []entry.Entry{called})
	if !strings.Contains(out, "## source\nname:Snapshot path:m.go line:\n  callers:\n    cluster.go:1: c.Snapshot(1)") {
		t.Errorf("## source lacks the callers block under its header:\n%s", out)
	}
	if callersLine(nil, 0) != "" {
		t.Error("no callers, no line")
	}
}

// TestFormat_UsesLine (directive 072): a field's sites are uses, printed
// under uses: after any callers:, capped on their own.
func TestFormat_UsesLine(t *testing.T) {
	var sites []entry.Call
	for i := 1; i <= maxCallers+1; i++ {
		sites = append(sites, entry.Call{Path: "logs.go", Name: "Prefix", Line: 200 + i, Text: fmt.Sprintf("o.Prefix = %d", i), In: "AddFlags", Use: true})
	}
	sites = append(sites, entry.Call{Path: "logs.go", Name: "Prefix", Line: 300, Text: "c.Prefix(x)", In: "Do"})
	got := callersLine(sites, 0)
	want := "  callers:\n    logs.go:300 in Do: c.Prefix(x)\n  uses:\n    logs.go:201 in AddFlags: o.Prefix = 1\n    logs.go:202 in AddFlags: o.Prefix = 2\n" +
		"    logs.go:203 in AddFlags: o.Prefix = 3\n    logs.go:204 in AddFlags: o.Prefix = 4\n    logs.go:205 in AddFlags: o.Prefix = 5\n    +1 more\n"
	if got != want {
		t.Errorf("callersLine = %q\nwant %q", got, want)
	}
	if only := callersLine(sites[:1], 0); only != "  uses:\n    logs.go:201 in AddFlags: o.Prefix = 1\n" {
		t.Errorf("uses alone: %q", only)
	}
}

// TestTruncated_PrintsPasteableCommands (directive 071): every command
// ## truncated prints is the call as typed in the one grammar — options
// before find, the words after, quoted where a shell needs it: the next
// page keeps the --query, a narrowing replaces its path: value and
// keeps the rest, and files at the root offer no narrowing.
func TestTruncated_PrintsPasteableCommands(t *testing.T) {
	var entries []entry.Entry
	for i := 0; i < 12; i++ {
		p := "internal/core/x.go"
		if i%3 == 0 {
			p = "root.go"
		}
		entries = append(entries, entry.Entry{Path: p, Name: fmt.Sprintf("Sym%d", i), Kind: "func", Exported: true})
	}
	s := Summarize(slices.Values(entries), "abc1234", 0, nil, nil, nil, nil)
	s.Call = Call{Query: "kind:func path:core", Words: []string{"usages", "mailbox overflow"}}
	out := Format(s, entries)
	for _, want := range []string{
		"\n## truncated\npage 1, 4 more: giraffe --query 'kind:func path:core' --page 2 find usages 'mailbox overflow'\n",
		" in internal/core: giraffe --query 'kind:func path:internal/core' find usages 'mailbox overflow'\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "path:.") {
		t.Errorf("root files offered as a narrowing:\n%s", out)
	}
	if got := (Call{}).Command("", 1); got != "giraffe find" {
		t.Errorf("browse with no query: %q", got)
	}
}

// TestFormat_SourceLongerThanItsRoomShowsTheLinesCarryingTheWords
// (directive 072): a declaration longer than ## source's room prints
// its head, then the numbered lines of the rest that carry the other
// words of the call, most words first, then the sed for the rest.
func TestFormat_SourceLongerThanItsRoomShowsTheLinesCarryingTheWords(t *testing.T) {
	lines := make([]string, 200)
	for i := range lines {
		lines[i] = fmt.Sprintf("\tstatement%03d := compute(%d)", i, i)
	}
	lines[0] = "func printPod(pod *Pod) {"
	lines[150] = "\trow.Cells = append(row.Cells, pod.Name, reason)"
	lines[120] = "\tlast := pod.Status.Reason"
	lines[180] = "\tcolumn := name(pod)"
	e := entry.Entry{Name: "printPod", Path: "p.go", Kind: "func", Line: 967, EndLine: 1166}
	s := Summary{Commit: "abc", Call: Call{Words: []string{"printPod", "column", "name"}}, Sources: []Source{{Entry: e, Lines: lines}}}
	got := FormatWords(s, []WordAnswer{{Word: "printPod", Words: []string{"printPod"}, Matches: []entry.Entry{e}}}, 1)
	src := got[strings.Index(got, "## source"):]
	if !strings.Contains(src, "func printPod(pod *Pod) {\n") {
		t.Errorf("the head is missing:\n%s", src)
	}
	i1147, i1117 := strings.Index(src, "1147: column := name(pod)"), strings.Index(src, "1117: row.Cells = append(row.Cells, pod.Name, reason)")
	if i1147 < 0 || i1117 < 0 || i1147 > i1117 {
		t.Errorf("want the line carrying two words (1147) before the one carrying one (1117):\n%s", src)
	}
	if strings.Contains(src, "1087:") {
		t.Errorf("a line carrying none of the words was shown:\n%s", src)
	}
	if !strings.Contains(src, "more lines: sed -n '") || len(src) > sourceBudget+200 {
		t.Errorf("want the sed for the rest within the budget (got %d bytes):\n%s", len(src), src)
	}
	// Short enough to fit: verbatim, unnumbered, as before.
	short := Summary{Commit: "abc", Call: Call{Words: []string{"x", "y"}}, Sources: []Source{{Entry: e, Lines: lines[:3]}}}
	got = FormatWords(short, nil, 1)
	if strings.Contains(got, "968:") || !strings.Contains(got, "\tstatement001 := compute(1)\n") {
		t.Errorf("a source that fits should print verbatim:\n%s", got)
	}
}

// TestFormat_CallersElsewhere (directive 072): sites of the name that
// are not this declaration's own print as one count, under the own
// sites or alone.
func TestFormat_CallersElsewhere(t *testing.T) {
	own := []entry.Call{{Path: "cmd/logs/logs.go", Line: 210, Text: "o.Prefix", In: "LogsOptions.AddFlags"}}
	got := callersLine(own, 45)
	want := "  callers:\n    cmd/logs/logs.go:210 in LogsOptions.AddFlags: o.Prefix\n    +45 of the name elsewhere\n"
	if got != want {
		t.Errorf("callersLine = %q, want %q", got, want)
	}
	if got := callersLine(nil, 8418); got != "  callers: none of its own; 8418 of the name elsewhere\n" {
		t.Errorf("no own sites: %q", got)
	}
	e := entry.Entry{Path: "cmd/logs/logs.go", Name: "Prefix", Kind: "field", Exported: true, CallersElsewhere: 50}
	out := FormatWords(Summary{}, []WordAnswer{{Word: "prefix", Words: []string{"prefix"}, Matches: []entry.Entry{e}}}, 1)
	if !strings.Contains(out, "  callers: none of its own; 50 of the name elsewhere\n") {
		t.Errorf("a result with only unattributed sites lacks the count line:\n%s", out)
	}
}

// Directive 073: every word's answer is the one call, so its next page
// and narrowings are the same commands; a page prints each once.
func TestFormatWords_CommandPrintedOnce(t *testing.T) {
	var many []entry.Entry
	for i := 0; i < 40; i++ {
		many = append(many, entry.Entry{Path: fmt.Sprintf("pkg/a/f%d.go", i), Name: fmt.Sprintf("N%d", i), Kind: "func", Line: 1})
	}
	s := Summary{Commit: "abc1234", Symbols: 40, Call: Call{Words: []string{"x", "y"}}}
	out := FormatWords(s, []WordAnswer{
		{Word: "x", Words: []string{"x"}, Matches: many},
		{Word: "y", Words: []string{"y"}, Matches: many[20:]},
	}, 1)
	if n := strings.Count(out, "giraffe --page 2 find x y"); n != 1 {
		t.Fatalf("next-page command printed %d times, want 1:\n%s", n, out)
	}
	if !strings.Contains(out, "more: as above") {
		t.Fatalf("second truncation should point at the command above:\n%s", out)
	}
}

// Directive 073: a none: word whose files' text was searched says what
// that search found — nowhere, or where — instead of leaving it to grep.
func TestFormatWords_NoneWithText(t *testing.T) {
	s := Summary{Commit: "abc1234", Symbols: 10, Call: Call{Words: []string{"smtp"}}}
	out := FormatWords(s, []WordAnswer{{Word: "smtp", Words: []string{"smtp"}, TextSearched: 7}}, 1)
	if !strings.Contains(out, "none: no name, doc comment or path among the index's 10 entries contains smtp\ntext: nor is it in the text of any of the 7 source files indexed\n") {
		t.Fatalf("none with an empty text search:\n%s", out)
	}
	out = FormatWords(s, []WordAnswer{{Word: "smtp", Words: []string{"smtp"}, TextSearched: 7, TextFiles: 2, Text: []string{"a.go:3: x := \"smtp\""}}}, 1)
	if !strings.Contains(out, "text: in the text of 2 of the 7 source files indexed:\n    a.go:3: x := \"smtp\"\n    +1 more files\n") {
		t.Fatalf("none with text sites:\n%s", out)
	}
}

// Directive 073: a page where a word was found nowhere says what the
// repository is made of — its top-level directories, each with its doc
// line — and a page where every word was found does not.
func TestFormatWords_LayoutOnNowhere(t *testing.T) {
	entries := []entry.Entry{
		{Path: "actor/a.go", Name: "a", Kind: entry.KindFile, Doc: "Package actor runs actors."},
		{Path: "actor/x.go", Name: "X", Kind: "func", Line: 1},
		{Path: "cluster/c.go", Name: "C", Kind: "func", Line: 1},
	}
	s := Summarize(slices.Values(entries), "abc1234", 0, nil, nil, nil, nil)
	s.Call = Call{Words: []string{"smtp"}}
	out := FormatWords(s, []WordAnswer{{Word: "smtp", Words: []string{"smtp"}, TextSearched: 3}}, 1)
	if !strings.Contains(out, "\n## layout\nactor/ 2 — Package actor runs actors.\ncluster/ 1\n") {
		t.Fatalf("no layout:\n%s", out)
	}
	if strings.Index(out, "## layout") > strings.Index(out, "## results") {
		t.Fatalf("layout after the answers:\n%s", out)
	}
	found := FormatWords(s, []WordAnswer{{Word: "x", Words: []string{"x"}, Matches: entries[1:2]}}, 1)
	if strings.Contains(found, "## layout") {
		t.Fatalf("layout on a page with every word found:\n%s", found)
	}
}

// Directive 073: a declaration the code calls lists the code's sites
// and counts the tests'; one only tests call lists the tests'.
func TestCallersLine_TestsCounted(t *testing.T) {
	code := entry.Call{Path: "a/a.go", Line: 3, In: "F", Text: "X()"}
	test := entry.Call{Path: "a/a_test.go", Line: 9, In: "TestF", Text: "X()", Test: true}
	if got := callersLine([]entry.Call{code, test, test}, 0); !strings.Contains(got, "a/a.go:3 in F: X()\n    +2 in tests\n") || strings.Contains(got, "a_test.go") {
		t.Fatalf("code and tests:\n%s", got)
	}
	if got := callersLine([]entry.Call{test}, 0); !strings.Contains(got, "a/a_test.go:9 in TestF") {
		t.Fatalf("tests only:\n%s", got)
	}
}
