package query

import (
	"strings"
	"testing"

	"giraffe/internal/entry"
)

func TestParse_ValidAndInvalid(t *testing.T) {
	valid := map[string]bool{"path": true, "name": true, "kind": true, "project": true, "module": true}

	f, err := Parse("path:internal/retry", valid)
	if err != nil {
		t.Fatal(err)
	}
	if f["path"][0] != "internal/retry" {
		t.Fatalf("got %v", f)
	}

	if _, err := Parse("bogus:x", valid); err == nil {
		t.Fatal("expected an error for an unknown attribute")
	}
	if _, err := Parse("malformed", valid); err == nil {
		t.Fatal("expected an error for a malformed attribute")
	}
}

func TestParse_QuotedValueWithSpaces(t *testing.T) {
	valid := map[string]bool{"path": true, "project": true}
	f, err := Parse("path:'internal/my dir' project:giraffe", valid)
	if err != nil {
		t.Fatal(err)
	}
	if f["path"][0] != "internal/my dir" {
		t.Fatalf("path = %q", f["path"])
	}
	if f["project"][0] != "giraffe" {
		t.Fatalf("project = %q", f["project"])
	}
}

func TestParse_RepeatedAttributeIsOR_DifferentIsAND(t *testing.T) {
	valid := map[string]bool{"path": true, "project": true}
	f, err := Parse("path:a path:b project:giraffe", valid)
	if err != nil {
		t.Fatal(err)
	}
	if len(f["path"]) != 2 || len(f["project"]) != 1 {
		t.Fatalf("got %v", f)
	}
}

func TestFilter_Match(t *testing.T) {
	f := Filter{"path": {"internal/retry"}}
	inside := entry.Entry{Path: "internal/retry/retry.go"}
	exact := entry.Entry{Path: "internal/retry"}
	outside := entry.Entry{Path: "internal/other/x.go"}
	if !f.Match(inside) {
		t.Error("expected a file under internal/retry/ to match")
	}
	if !f.Match(exact) {
		t.Error("expected an exact path match")
	}
	if f.Match(outside) {
		t.Error("expected a file outside internal/retry/ not to match")
	}
}

// TestFilter_Match_PathIsSubstringNotPrefix (037): the motivating case —
// 033's aided run wrote path:kubectl meaning the kubectl project, and
// 036's own run wrote path:logs meaning the logs command; neither
// kubectl/ nor logs/ sits at a repository root, so the old
// exact-or-directory-prefix rule matched nothing for either. A value
// appearing anywhere in the path — not just at its start — now matches.
func TestFilter_Match_PathIsSubstringNotPrefix(t *testing.T) {
	f := Filter{"path": {"kubectl"}}
	middle := entry.Entry{Path: "staging/src/k8s.io/kubectl/pkg/cmd/logs/logs.go"}
	if !f.Match(middle) {
		t.Error("expected kubectl to match as a substring of a path where it is not the leading segment")
	}
	noMatch := entry.Entry{Path: "internal/other/x.go"}
	if f.Match(noMatch) {
		t.Error("expected a path containing no occurrence of kubectl not to match")
	}
	// A value carrying "/" still behaves like a directory prefix in
	// practice: it is a substring that happens to include the separator.
	prefixShaped := Filter{"path": {"staging/src/k8s.io/kubectl"}}
	if !prefixShaped.Match(middle) {
		t.Error("expected a slash-bearing value to still match the directory it names")
	}
}

// TestParse_KindAndScopeAreFilterable (§22 C-10): before this directive
// these were valid, displayed attributes rejected by --query as
// "not supported" — safe, but broke §6's paste-ready promise. 001's
// path/project-only restriction was a minimal-version convenience, not
// a design decision.
func TestParse_KindAndScopeAreFilterable(t *testing.T) {
	valid := map[string]bool{"kind": true, "scope": true}
	f, err := Parse("kind:func scope:package", valid)
	if err != nil {
		t.Fatalf("expected kind and scope to be filterable, got: %v", err)
	}
	if f["kind"][0] != "func" || f["scope"][0] != "package" {
		t.Fatalf("got %v", f)
	}
}

// TestParse_LangIsFilterable (§24 C-20): lang joins kind and scope as a
// common, filterable attribute.
func TestParse_LangIsFilterable(t *testing.T) {
	valid := map[string]bool{"lang": true}
	f, err := Parse("lang:go", valid)
	if err != nil {
		t.Fatalf("expected lang to be filterable, got: %v", err)
	}
	if f["lang"][0] != "go" {
		t.Fatalf("got %v", f)
	}
}

func TestFilter_MatchesLang(t *testing.T) {
	f := Filter{"lang": {"go"}}
	match := entry.Entry{Lang: "go"}
	other := entry.Entry{Lang: "c"}
	unknown := entry.Entry{Lang: ""} // §25 C-24: absent, not a placeholder value
	if !f.Match(match) {
		t.Error("expected lang:go to match a go entry")
	}
	if f.Match(other) {
		t.Error("expected lang:go to exclude a differently-languaged entry")
	}
	if f.Match(unknown) {
		t.Error("expected lang:go to exclude an entry with no known language")
	}
}

// TestParse_ExtIsFilterable and TestFilter_MatchesExt (§25 C-24): ext is
// a common, filterable attribute independent of lang — present for
// every file, known or not.
func TestParse_ExtIsFilterable(t *testing.T) {
	valid := map[string]bool{"ext": true}
	f, err := Parse("ext:h", valid)
	if err != nil {
		t.Fatalf("expected ext to be filterable, got: %v", err)
	}
	if f["ext"][0] != "h" {
		t.Fatalf("got %v", f)
	}
}

func TestFilter_MatchesExt(t *testing.T) {
	f := Filter{"ext": {"h"}}
	match := entry.Entry{Ext: "h", Lang: ""}
	other := entry.Entry{Ext: "cpp", Lang: ""}
	if !f.Match(match) {
		t.Error("expected ext:h to match an .h entry even with lang unknown")
	}
	if f.Match(other) {
		t.Error("expected ext:h to exclude a .cpp entry")
	}
}

func TestFilter_MatchesKindAndScope(t *testing.T) {
	f := Filter{"kind": {"type"}, "scope": {"package"}}
	match := entry.Entry{Kind: "type", Scope: "package"}
	wrongKind := entry.Entry{Kind: "func", Scope: "package"}
	wrongScope := entry.Entry{Kind: "type", Scope: "exported"}
	if !f.Match(match) {
		t.Error("expected kind:type scope:package to match")
	}
	if f.Match(wrongKind) {
		t.Error("expected a func to be excluded by kind:type")
	}
	if f.Match(wrongScope) {
		t.Error("expected an exported entry to be excluded by scope:package")
	}
}

// TestValidAttributes_IncludesSizeDeclarationsAndLine (directive 017
// §4): these three are common Entry fields, valid unconditionally like
// path/name/kind — not derived per repository like a language
// position — so an empty entry set still reports them valid.
func TestValidAttributes_IncludesSizeDeclarationsAndLine(t *testing.T) {
	valid := ValidAttributes(Slice(nil))
	for _, name := range []string{"size", "declarations", "line"} {
		if !valid[name] {
			t.Errorf("ValidAttributes() missing %q", name)
		}
	}
}

// TestParse_SizeDeclarationsAndLineAreFilterable (directive 017 §4:
// "make every printed attribute filterable" — size and declarations
// have printed unfilterable since 009, line since 016).
func TestParse_SizeDeclarationsAndLineAreFilterable(t *testing.T) {
	valid := map[string]bool{"size": true, "declarations": true, "line": true}
	f, err := Parse("size:150000 declarations:42 line:17", valid)
	if err != nil {
		t.Fatalf("expected size, declarations and line to be filterable, got: %v", err)
	}
	if f["size"][0] != "150000" || f["declarations"][0] != "42" || f["line"][0] != "17" {
		t.Fatalf("got %v", f)
	}
}

func TestFilter_MatchesSizeDeclarationsAndLine(t *testing.T) {
	fileEntry := entry.Entry{Kind: entry.KindFile, Size: 150000, DeclCount: 42}
	declEntry := entry.Entry{Kind: "func", Line: 17}

	if !(Filter{"size": {"150000"}}).Match(fileEntry) {
		t.Error("expected size:150000 to match")
	}
	if (Filter{"size": {"1"}}).Match(fileEntry) {
		t.Error("expected size:1 to exclude a 150000-byte file")
	}
	if !(Filter{"declarations": {"42"}}).Match(fileEntry) {
		t.Error("expected declarations:42 to match")
	}
	if !(Filter{"line": {"17"}}).Match(declEntry) {
		t.Error("expected line:17 to match")
	}
	if (Filter{"line": {"17"}}).Match(fileEntry) {
		t.Error("expected line:17 to exclude a file entry (Line is zero-valued there)")
	}
}

// TestFilter_LineMatchesTheRangeAsPrinted (directive 042): a result
// line prints line:19-62; pasting that into --query matches (§7's
// paste-ready rule), and so does the bare start line. A different
// range does not, and a file entry, which prints no line, never does.
func TestFilter_LineMatchesTheRangeAsPrinted(t *testing.T) {
	ranged := entry.Entry{Kind: "type", Line: 19, EndLine: 62}
	fileEntry := entry.Entry{Kind: entry.KindFile}
	for _, v := range []string{"19-62", "19"} {
		if !(Filter{"line": {v}}).Match(ranged) {
			t.Errorf("expected line:%s to match an entry at 19-62", v)
		}
		if (Filter{"line": {v}}).Match(fileEntry) {
			t.Errorf("expected line:%s to exclude a file entry", v)
		}
	}
	for _, v := range []string{"62", "19-63", "20"} {
		if (Filter{"line": {v}}).Match(ranged) {
			t.Errorf("expected line:%s not to match an entry at 19-62", v)
		}
	}
}

// TestParse_PositionAttributeIsFilterable (directive 015 item 2): module,
// receiver, type — any language position, present or future — are
// filterable the moment ValidAttributes reports them, with no second
// list to update. Reproduces the exact gap the audit found live: a
// `--query 'module:...'` erroring on an attribute printed on every
// result line (found via the now-withdrawn `show`, §31.3, but the gap
// was in Parse/--query itself).
func TestParse_PositionAttributeIsFilterable(t *testing.T) {
	valid := map[string]bool{"module": true, "receiver": true, "type": true}
	f, err := Parse("module:internal/limits", valid)
	if err != nil {
		t.Fatalf("expected a language position to be filterable, got: %v", err)
	}
	if f["module"][0] != "internal/limits" {
		t.Fatalf("got %v", f)
	}
	if _, err := Parse("receiver:Store", valid); err != nil {
		t.Errorf("expected receiver to be filterable, got: %v", err)
	}
	if _, err := Parse("type:Store", valid); err != nil {
		t.Errorf("expected type to be filterable, got: %v", err)
	}
}

func TestFilter_MatchesPositionAttribute(t *testing.T) {
	f := Filter{"module": {"internal/limits"}}
	match := entry.Entry{Position: []entry.Position{{Name: "module", Value: "internal/limits"}}}
	other := entry.Entry{Position: []entry.Position{{Name: "module", Value: "internal/query"}}}
	noPosition := entry.Entry{}
	if !f.Match(match) {
		t.Error("expected module:internal/limits to match an entry with that position")
	}
	if f.Match(other) {
		t.Error("expected module:internal/limits to exclude a different module")
	}
	if f.Match(noPosition) {
		t.Error("expected module:internal/limits to exclude an entry with no language position")
	}

	// A second position pair (receiver, type) matches on its own name,
	// independent of the first (module) — golang.go always emits module
	// first, then receiver or type for a method or field.
	member := Filter{"receiver": {"Store"}}
	m := entry.Entry{Position: []entry.Position{{Name: "module", Value: "internal/index"}, {Name: "receiver", Value: "Store"}}}
	if !member.Match(m) {
		t.Error("expected receiver:Store to match a method entry's second position pair")
	}
}

// TestParse_NameIsFilterable (directive 018 §3): name closes the last
// gap in §6's "printed attributes are exactly the filterable ones"
// invariant — filterable now, as one more --query attribute, distinct
// from find's own positional substring match against the same field.
func TestParse_NameIsFilterable(t *testing.T) {
	valid := map[string]bool{"name": true}
	f, err := Parse("name:Retry", valid)
	if err != nil {
		t.Fatalf("expected name to be filterable, got: %v", err)
	}
	if f["name"][0] != "Retry" {
		t.Fatalf("got %v", f)
	}
}

// TestFilter_MatchesNameExactly: an exact match, not a substring one —
// distinct from find's own positional word (see tier), which is
// case-insensitive and matches a substring.
func TestFilter_MatchesNameExactly(t *testing.T) {
	f := Filter{"name": {"Retry"}}
	exact := entry.Entry{Name: "Retry"}
	substring := entry.Entry{Name: "RetryLoop"}
	caseFolded := entry.Entry{Name: "retry"}
	if !f.Match(exact) {
		t.Error("expected an exact name match")
	}
	if f.Match(substring) {
		t.Error("expected name:Retry not to match RetryLoop (exact, not substring)")
	}
	if f.Match(caseFolded) {
		t.Error("expected name:Retry not to match retry (exact, not case-folded)")
	}
}

func TestBrowse_FiltersAndOrdersByPathThenName(t *testing.T) {
	entries := []entry.Entry{
		{Name: "B", Path: "b.go", Project: "giraffe", Exported: true},
		{Name: "Z", Path: "a.go", Project: "giraffe", Exported: true},
		{Name: "A", Path: "a.go", Project: "giraffe", Exported: true},
		{Name: "Elsewhere", Path: "elsewhere.go", Project: "other", Exported: true},
	}
	got := Browse(Slice(entries), Filter{"project": {"giraffe"}})
	if len(got) != 3 {
		t.Fatalf("expected 3 entries in project giraffe, got %d: %v", len(got), got)
	}
	want := []string{"a.go/A", "a.go/Z", "b.go/B"}
	for i, w := range want {
		got := got[i].Path + "/" + got[i].Name
		if got != w {
			t.Errorf("position %d = %s, want %s", i, got, w)
		}
	}
}

func TestBrowse_HidesUnexportedOutsideItsOwnRoot(t *testing.T) {
	entries := []entry.Entry{
		{Name: "helper", Path: "internal/x/x.go", Project: "giraffe", Exported: false},
	}
	if got := Browse(Slice(entries), Filter{"path": {"internal/x"}}); len(got) != 1 {
		t.Fatalf("expected the package-scope entry visible from its own root, got %d", len(got))
	}
	if got := Browse(Foreign(Slice(entries)), Filter{"path": {"internal/x"}}); len(got) != 0 {
		t.Fatalf("expected the package-scope entry hidden from another root, got %d", len(got))
	}
}

// TestDiagnoseFilter_ListsEveryAttributeAValueActuallyBelongsTo
// (directive 035, revising 034): a value genuinely real under two other
// attributes at once (an exact Name and a Project) — 034 reported only
// the alphabetically-first ("name"), silently hiding the more useful
// "project" answer that was equally true. 035 requires both,
// alphabetical, no ranking between them. The fixture's Path
// (cmd/x/x.go) deliberately contains no occurrence of "kubectl" at all —
// 037 made path: a substring match, and the original fixture's
// cmd/kubectl/kubectl.go would now match path:kubectl directly (see
// TestFilter_Match_PathIsSubstringNotPrefix), which is a different test.
func TestDiagnoseFilter_ListsEveryAttributeAValueActuallyBelongsTo(t *testing.T) {
	entries := []entry.Entry{
		{Name: "kubectl", Kind: "file", Path: "cmd/x/x.go", Project: "gekka", Exported: true},
		{Name: "Get", Kind: "func", Path: "cmd/y/y.go", Project: "kubectl", Exported: true},
	}
	got := DiagnoseFilter(Slice(entries), Filter{"path": {"kubectl"}})
	if len(got) != 1 {
		t.Fatalf("expected one diagnosis, got %v", got)
	}
	want := `"path:kubectl" is wrong - it is in [name, project]`
	if got[0] != want {
		t.Errorf("diagnosis = %q, want %q", got[0], want)
	}
}

// TestDiagnoseFilter_PathDiagnosisStillFiresWhenNoPathContainsTheValue
// (037): the cross-attribute diagnosis for path is not dead — it is
// unreachable only for a value that some real path happens to contain.
// A value absent from every path, name, kind, and project still gets
// the plain "no such path" report.
func TestDiagnoseFilter_PathDiagnosisStillFiresWhenNoPathContainsTheValue(t *testing.T) {
	entries := []entry.Entry{{Name: "Retry", Kind: "func", Path: "internal/retry/retry.go", Project: "gekka", Exported: true}}
	got := DiagnoseFilter(Slice(entries), Filter{"path": {"nosuchsubstring"}})
	if len(got) != 1 {
		t.Fatalf("expected one diagnosis, got %v", got)
	}
	if !strings.Contains(got[0], "no such path") || !strings.Contains(got[0], `"nosuchsubstring"`) {
		t.Errorf("diagnosis = %q, want a plain \"no such path\" report", got[0])
	}
}

// TestDiagnoseFilter_ReportsNoSuchValueWhenFoundNowhere: a value absent
// from the index under every attribute, not just the one it was given
// under — no cross-attribute hint to offer, and none invented.
func TestDiagnoseFilter_ReportsNoSuchValueWhenFoundNowhere(t *testing.T) {
	entries := []entry.Entry{{Name: "Retry", Kind: "func", Path: "internal/retry/retry.go", Project: "gekka", Exported: true}}
	got := DiagnoseFilter(Slice(entries), Filter{"project": {"nosuchproject"}})
	if len(got) != 1 {
		t.Fatalf("expected one diagnosis, got %v", got)
	}
	if !strings.Contains(got[0], "no such project") || !strings.Contains(got[0], `"nosuchproject"`) {
		t.Errorf("diagnosis = %q, want a plain \"no such project\" report with no invented alternative", got[0])
	}
}

// TestDiagnoseFilter_DistinguishesRealEmptyIntersectionFromABadValue: two
// constraints that each individually match something get a different
// message than a bad value does — the two need different next moves and
// must not read identically (directive 034 §2).
func TestDiagnoseFilter_DistinguishesRealEmptyIntersectionFromABadValue(t *testing.T) {
	entries := []entry.Entry{
		{Name: "Retry", Kind: "func", Path: "internal/retry/retry.go", Project: "retryproj", Exported: true},
		{Name: "kubectl", Kind: "file", Path: "cmd/kubectl/kubectl.go", Project: "gekka", Exported: true},
	}
	// project:retryproj is real (Retry); path:cmd/kubectl is real (the
	// kubectl file entry) — but no single entry satisfies both at once.
	got := DiagnoseFilter(Slice(entries), Filter{"project": {"retryproj"}, "path": {"cmd/kubectl"}})
	if len(got) != 1 {
		t.Fatalf("expected one line, got %v", got)
	}
	if strings.Contains(got[0], "no such") || strings.Contains(got[0], "is wrong") {
		t.Errorf("diagnosis = %q, expected the real-but-empty-intersection message, not a bad-value one", got[0])
	}
}

// TestDiagnoseFilter_MultipleBadConstraintsEachGetTheirOwnLine (directive
// 035 report item 2's "a case with two bad constraints"): path:kubectl
// (real, under name) and lang:cobol (real nowhere) in the same filter
// each produce their own line, attribute-sorted. Path is cmd/x/x.go, not
// cmd/kubectl/kubectl.go, so 037's substring rule does not make
// path:kubectl a direct match here — see
// TestDiagnoseFilter_ListsEveryAttributeAValueActuallyBelongsTo's comment.
func TestDiagnoseFilter_MultipleBadConstraintsEachGetTheirOwnLine(t *testing.T) {
	entries := []entry.Entry{
		{Name: "kubectl", Kind: "file", Path: "cmd/x/x.go", Project: "gekka", Exported: true, Lang: "go"},
	}
	got := DiagnoseFilter(Slice(entries), Filter{"path": {"kubectl"}, "lang": {"cobol"}})
	if len(got) != 2 {
		t.Fatalf("expected two lines, got %v", got)
	}
	if got[0] != `no such lang: "cobol"` {
		t.Errorf("line 0 = %q, want the lang diagnosis first (attribute-sorted: lang < path)", got[0])
	}
	if got[1] != `"path:kubectl" is wrong - it is in [name]` {
		t.Errorf("line 1 = %q, want the path diagnosis naming only name (project is not in this fixture)", got[1])
	}
}

// TestDiagnoseFilter_NilForEmptyFilter: nothing to diagnose when there
// was no filter at all — main.go only calls this when queryRaw was
// non-empty, but the function's own contract holds regardless.
func TestDiagnoseFilter_NilForEmptyFilter(t *testing.T) {
	if got := DiagnoseFilter(Slice(nil), Filter{}); got != nil {
		t.Fatalf("expected nil for an empty filter, got %v", got)
	}
}

func TestSuggest_ReportsMatchesHiddenInAnotherRoot(t *testing.T) {
	entries := []entry.Entry{
		{Name: "readLines", Project: "giraffe", Exported: false},
	}
	got := Suggest(Foreign(Slice(entries)), "readLines")
	if len(got) != 1 {
		t.Fatalf("expected one hint, got %v", got)
	}
	if !strings.Contains(got[0], "1 entries") || !strings.Contains(got[0], "giraffe") {
		t.Errorf("hint = %q, want it to name the count and which project actually has the match", got[0])
	}
}

func TestSuggest_OffersNearNamesWhenNothingMatchedAtAll(t *testing.T) {
	entries := []entry.Entry{
		{Name: "Retry", Project: "giraffe", Exported: true},
		{Name: "RetryPolicy", Project: "giraffe", Exported: true},
	}
	got := Suggest(Slice(entries), "Retri") // typo: not a substring of either name, so tier() misses both
	if len(got) == 0 {
		t.Fatalf("expected a near-name hint for a shared prefix")
	}
	if !strings.Contains(got[0], "Retry") {
		t.Errorf("hint = %q, want it to mention a near-miss name", got[0])
	}
}

// TestSuggest_TokenOverlapCatchesRestructuredCompoundName (§23 C-16):
// the reported miss — "runShow" and "showCommand" share no leading
// prefix ("runs" vs "show"), so the prefix heuristic alone offered
// nothing for a caller who guessed the wrong shape of the compound
// name. Splitting both on their camelCase boundary and matching the
// shared token "show" is the fix.
func TestSuggest_TokenOverlapCatchesRestructuredCompoundName(t *testing.T) {
	entries := []entry.Entry{{Name: "runShow", Project: "giraffe", Exported: true}}
	got := Suggest(Slice(entries), "showCommand")
	if len(got) == 0 {
		t.Fatalf("expected a token-overlap hint for showCommand/runShow (shared token %q)", "show")
	}
	if !strings.Contains(got[0], "runShow") {
		t.Errorf("hint = %q, want it to name runShow", got[0])
	}
}

// TestSuggest_PrefixStillCatchesWhatItAlreadyCaught: browseEntries ->
// Browse is the directive's own named regression check — the prefix
// heuristic already caught it (shared leading "brow"), and token
// splitting must not have replaced or broken that path.
func TestSuggest_PrefixStillCatchesWhatItAlreadyCaught(t *testing.T) {
	entries := []entry.Entry{{Name: "Browse", Project: "giraffe", Exported: true}}
	got := Suggest(Slice(entries), "browseEntries")
	if len(got) == 0 {
		t.Fatalf("expected a near-name hint for browseEntries/Browse (shared leading characters)")
	}
	if !strings.Contains(got[0], "Browse") {
		t.Errorf("hint = %q, want it to name Browse", got[0])
	}
}

// TestCouldShareToken_NecessaryButNotSufficient (§26 C-28/item 4): the
// pre-filter must never produce a false negative (that would silently
// drop a real hint) but may produce false positives — "recluster"
// contains "cluster" as a substring without "cluster" ever being one of
// its identifier tokens (identifierTokens("recluster") is one token,
// "recluster", with no case or separator boundary to split on).
func TestCouldShareToken_NecessaryButNotSufficient(t *testing.T) {
	wordTokens := []string{"cluster"}
	if !couldShareToken("recluster", wordTokens) {
		t.Error("expected couldShareToken to over-approximate: \"cluster\" is a substring of \"recluster\"")
	}
	if sharesToken("recluster", map[string]bool{"cluster": true}) {
		t.Error("expected sharesToken to correctly reject it: \"recluster\" has no token boundary splitting off \"cluster\"")
	}
	if couldShareToken("Backoff", wordTokens) {
		t.Error("expected couldShareToken to reject a name with no substring overlap at all")
	}
}

// TestSuggest_PreFilterDoesNotChangeResults runs the same near-token
// scenarios the existing Suggest tests already cover, confirming the
// couldShareToken pre-filter (added purely for cost, §26 C-28/item 4)
// changes nothing about which hints are offered.
func TestSuggest_PreFilterDoesNotChangeResults(t *testing.T) {
	entries := []entry.Entry{
		{Name: "runShow", Project: "giraffe", Exported: true},
		{Name: "recluster", Project: "giraffe", Exported: true}, // substring overlap, no real token match
	}
	got := Suggest(Slice(entries), "showCommand")
	if len(got) == 0 || !strings.Contains(got[0], "runShow") {
		t.Fatalf("expected the real token-overlap hint to survive the pre-filter, got %v", got)
	}
	// "showCluster" tokenizes to ["show", "cluster"]; "recluster"
	// contains "cluster" as a raw substring (satisfying the pre-filter)
	// but tokenizes to the single token "recluster" (no case boundary),
	// so it must not be offered as a near-token hint.
	got2 := Suggest(Slice(entries), "showCluster")
	for _, hint := range got2 {
		if strings.Contains(hint, "recluster") {
			t.Errorf("expected \"recluster\" to be excluded (substring-only overlap, no shared token), got %v", got2)
		}
	}
}

func TestSuggest_NilWhenNothingToOffer(t *testing.T) {
	entries := []entry.Entry{{Name: "Unrelated", Project: "giraffe", Exported: true}}
	if got := Suggest(Slice(entries), "zzz"); len(got) != 0 {
		t.Fatalf("expected no fabricated hint, got %v", got)
	}
}

func TestIdentifierTokens_SplitsCamelCaseSnakeCaseAndAcronyms(t *testing.T) {
	cases := map[string][]string{
		"runShow":       {"run", "show"},
		"showCommand":   {"show", "command"},
		"browseEntries": {"browse", "entries"},
		"read_lines":    {"read", "lines"},
		"HTTPServer":    {"http", "server"},
		"retry":         {"retry"},
	}
	for word, want := range cases {
		got := identifierTokens(word)
		if len(got) != len(want) {
			t.Fatalf("identifierTokens(%q) = %v, want %v", word, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("identifierTokens(%q) = %v, want %v", word, got, want)
			}
		}
	}
}

func TestSearch_RanksNameOverDocOverPath(t *testing.T) {
	entries := []entry.Entry{
		{Name: "Backoff", Doc: "uses retry internally", Path: "x.go"},
		{Name: "Retry", Doc: "retries with backoff", Path: "internal/retry/retry.go"},
		{Name: "Other", Doc: "unrelated", Path: "internal/retry/x.go"},
	}
	got := Search(Slice(entries), "retry", nil)
	if len(got) != 3 {
		t.Fatalf("expected all 3 to match (name, doc, path), got %d: %v", len(got), got)
	}
	if got[0].Name != "Retry" {
		t.Errorf("expected exact name match first, got %s", got[0].Name)
	}
	if got[1].Name != "Backoff" {
		t.Errorf("expected doc match second, got %s", got[1].Name)
	}
	if got[2].Name != "Other" {
		t.Errorf("expected path-only match last, got %s", got[2].Name)
	}
}

// TestSearch_DeclarationRanksAboveFileEntry (§24 C-20): a declaration
// answering the search outranks a file merely sharing the name, within
// the same match tier — a difference in structure, not in language.
func TestSearch_DeclarationRanksAboveFileEntry(t *testing.T) {
	entries := []entry.Entry{
		{Name: "retry", Kind: entry.KindFile, Path: "retry.go", Exported: true, Lang: "go"},
		{Name: "retry", Kind: "func", Path: "retry.go", Exported: true, Lang: "go"},
	}
	got := Search(Slice(entries), "retry", nil)
	if len(got) != 2 {
		t.Fatalf("expected both to match, got %d: %v", len(got), got)
	}
	if got[0].Kind != "func" {
		t.Errorf("expected the declaration ranked first, got %s first", got[0].Kind)
	}
}

// TestSearch_TopLevelRanksAboveMemberInSameTier (§28 C-38, §29 C-42): a
// type named Cluster must not rank below a same-named field or method,
// exported or not — the risk directive 010 named explicitly ("find
// Cluster returning Cluster plus seven of its methods is worse than
// today"). Since §29, this is a tier separation (tier() itself puts a
// member's exact match below a top-level exact match), not a tie-break
// within one shared tier — memberRank, tried in 010, ordered within a
// tier but could not decide which entries filled the response budget
// once a tier held more than it.
func TestSearch_TopLevelRanksAboveMemberInSameTier(t *testing.T) {
	entries := []entry.Entry{
		{Name: "Cluster", Kind: "field", Path: "config.go", Exported: true},
		{Name: "cluster", Kind: "field", Path: "watcher.go", Exported: false},
		{Name: "Cluster", Kind: "method", Path: "cluster.go", Exported: true},
		{Name: "Cluster", Kind: "type", Path: "cluster.go", Exported: true},
	}
	got := Search(Slice(entries), "Cluster", nil)
	if len(got) != 4 {
		t.Fatalf("expected all four to match, got %d: %v", len(got), got)
	}
	if got[0].Kind != "type" {
		t.Errorf("expected the top-level type ranked first, got %s first", got[0].Kind)
	}
}

// TestSearch_MemberExactMatchOutranksTopLevelSubstring (directive 065,
// g33, reversing §29 C-42's order for this pair): a field named exactly
// the word says more than a type whose name merely contains it -
// "actors" asked, the field actors, not ActorSystem, which contains it
// only by running Actor and System together. A top-level exact match
// still comes first (TestSearch_TopLevelRanksAboveMemberInSameTier).
func TestSearch_MemberExactMatchOutranksTopLevelSubstring(t *testing.T) {
	entries := []entry.Entry{
		{Name: "cluster", Kind: "field", Path: "deployment_config.go", Exported: true},
		{Name: "cluster", Kind: "field", Path: "watcher_actor.go", Exported: true},
		{Name: "ClusterBootstrap", Kind: "type", Path: "cluster/bootstrap.go", Exported: true},
	}
	got := Search(Slice(entries), "Cluster", nil)
	if len(got) != 3 {
		t.Fatalf("expected all three to match, got %d: %v", len(got), got)
	}
	if got[0].Kind != "field" || got[2].Name != "ClusterBootstrap" {
		t.Errorf("expected the exact-named fields before ClusterBootstrap, got %v", got)
	}
}

// TestSearch_MemberStillWinsWhenItIsTheOnlyMatch (§29 C-42): demoting
// members below top-level declarations must not make a member
// unreachable when it is genuinely the right — or only — answer. A
// method whose name matches nothing else in the index must still be
// returned as the top (and only) result.
func TestSearch_MemberStillWinsWhenItIsTheOnlyMatch(t *testing.T) {
	entries := []entry.Entry{
		{Name: "GetMailboxLengths", Kind: "method", Path: "mailbox.go", Exported: true},
		{Name: "Unrelated", Kind: "type", Path: "other.go", Exported: true},
	}
	got := Search(Slice(entries), "GetMailboxLengths", nil)
	if len(got) != 1 {
		t.Fatalf("expected exactly one match, got %d: %v", len(got), got)
	}
	if got[0].Name != "GetMailboxLengths" {
		t.Errorf("expected the method itself, got %s", got[0].Name)
	}
}

func TestSearch_NoMatchIsExcluded(t *testing.T) {
	entries := []entry.Entry{{Name: "Unrelated", Doc: "nothing here", Path: "x.go"}}
	if got := Search(Slice(entries), "retry", nil); len(got) != 0 {
		t.Fatalf("expected no matches, got %v", got)
	}
}

func TestSearch_ExportedRanksAboveUnexportedInSameTier(t *testing.T) {
	entries := []entry.Entry{
		{Name: "retry", Scope: "package", Exported: false, Project: "giraffe", Path: "x.go"},
		{Name: "Retry", Scope: "exported", Exported: true, Project: "giraffe", Path: "y.go"},
	}
	got := Search(Slice(entries), "retry", nil)
	if len(got) != 2 {
		t.Fatalf("expected both to match by name (case-insensitive), got %d: %v", len(got), got)
	}
	if !got[0].Exported {
		t.Errorf("expected the exported entry first, got %s (exported %v)", got[0].Name, got[0].Exported)
	}
}

func TestSearch_PackageScopeHiddenOutsideItsOwnRoot(t *testing.T) {
	entries := []entry.Entry{
		{Name: "readLines", Scope: "package", Exported: false, Project: "giraffe", Path: "internal/index/refresh.go"},
	}
	if got := Search(Slice(entries), "readLines", nil); len(got) != 1 {
		t.Fatalf("expected the package-scope entry visible from its own root, got %d: %v", len(got), got)
	}
	if got := Search(Foreign(Slice(entries)), "readLines", nil); len(got) != 0 {
		t.Fatalf("expected the package-scope entry hidden from another root, got %d: %v", len(got), got)
	}
	// Directive 072: another project in the same root is not another root.
	other := []entry.Entry{{Name: "addPrefixIfNeeded", Scope: "package", Exported: false, Project: "kubectl", Path: "staging/src/k8s.io/kubectl/pkg/cmd/logs/logs.go"}}
	if got := Search(Slice(other), "prefix", nil); len(got) != 1 {
		t.Fatalf("expected a sibling module's unexported declaration visible within the same root, got %d: %v", len(got), got)
	}
}

func TestSearch_ExportedVisibleFromEveryProject(t *testing.T) {
	entries := []entry.Entry{
		{Name: "Retry", Scope: "exported", Exported: true, Project: "giraffe", Path: "internal/retry/retry.go"},
	}
	if got := Search(Slice(entries), "Retry", nil); len(got) != 1 {
		t.Fatalf("expected the exported entry visible from a different project, got %d: %v", len(got), got)
	}
}

// TestBrowse_OrdersByLineWithinAFile (directive 043, from 041's
// branch): within one path, a browse follows the file's own order — the
// file entry (line 0) first, then declarations by line — not name
// order, which scattered a struct's fields away from the struct and
// printed ranges out of sequence.
func TestBrowse_OrdersByLineWithinAFile(t *testing.T) {
	entries := []entry.Entry{
		{Name: "Zed", Path: "a.go", Line: 10, Project: "giraffe", Exported: true},
		{Name: "a", Path: "a.go", Kind: entry.KindFile, Project: "giraffe", Exported: true},
		{Name: "Alpha", Path: "a.go", Line: 30, Project: "giraffe", Exported: true},
		{Name: "Mid", Path: "a.go", Line: 20, Project: "giraffe", Exported: true},
	}
	got := Browse(Slice(entries), Filter{"path": {"a.go"}})
	want := []string{"a", "Zed", "Mid", "Alpha"}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Name != w {
			t.Errorf("position %d = %s, want %s", i, got[i].Name, w)
		}
	}
}

// TestSearch_ShorterNameFirstWithinATier (directive 042; built and
// measured in 041): among substring matches of equal tier, the name
// the word covers more of comes first — NodeMetrics ahead of
// DefaultMetricsExporterConfig — rather than whichever sorts first
// alphabetically. Exact matches (tier 0) still lead regardless of
// length, and the alphabetical order remains the tie-break between
// names of one length.
func TestSearch_ShorterNameFirstWithinATier(t *testing.T) {
	entries := []entry.Entry{
		{Name: "DefaultMetricsExporterConfig", Kind: "func", Path: "a.go", Exported: true},
		{Name: "ClusterMetricsProvider", Kind: "type", Path: "b.go", Exported: true},
		{Name: "NodeMetrics", Kind: "type", Path: "c.go", Exported: true},
		{Name: "MetricsSnapshot", Kind: "type", Path: "d.go", Exported: true},
		{Name: "Metrics", Kind: "type", Path: "e.go", Exported: true},
		{Name: "MetricsGossip", Kind: "type", Path: "f.go", Exported: true},
	}
	got := Search(Slice(entries), "metrics", nil)
	want := []string{"Metrics", "NodeMetrics", "MetricsGossip", "MetricsSnapshot", "ClusterMetricsProvider", "DefaultMetricsExporterConfig"}
	if len(got) != len(want) {
		t.Fatalf("got %d results, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Name != w {
			t.Errorf("position %d = %s, want %s (full order: %v)", i, got[i].Name, w, names(got))
		}
	}
}

func names(entries []entry.Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Name
	}
	return out
}

// Directive 065: several words ranked together — an entry carrying two
// of them comes first, the rarer word weighs more, an entry carrying
// one is left to that word's own answer, and a word matching nothing
// counts 0.
func TestRanked_TwoOrMoreWordsRarerFirst(t *testing.T) {
	src := Slice([]entry.Entry{
		{Name: "Queue", Path: "a/queue.go", Exported: true, DocText: "Queue holds messages.\nA full queue drops the newest."},
		{Name: "Drop", Path: "a/drop.go", Exported: true, Doc: "Drop discards a message."},
		{Name: "Message", Path: "a/msg.go", Exported: true},
		{Name: "MessageBus", Path: "b/bus.go", Exported: true, Doc: "MessageBus carries a message."},
		{Name: "Other", Path: "c/other.go", Exported: true},
	})
	got, counts := Ranked(src, []string{"message", "drop", "smtp"}, nil)
	var names []string
	for _, e := range got {
		names = append(names, e.Name)
	}
	if strings.Join(names, " ") != "Drop Queue" {
		t.Errorf("ranked = %v, want [Drop Queue]", names)
	}
	if counts[0] != 4 || counts[1] != 2 || counts[2] != 0 {
		t.Errorf("counts = %v, want [4 2 0]", counts)
	}
}

// Directive 065: a search matches the whole doc comment, not only its
// first line.
func TestSearch_MatchesTheWholeDocComment(t *testing.T) {
	src := Slice([]entry.Entry{{Name: "Tell", Path: "ref.go", Exported: true, Doc: "Tell sends msg.", DocText: "Tell sends msg.\nA full mailbox sends it to dead letters."}})
	if got := Search(src, "dead letters", nil); len(got) != 1 {
		t.Errorf("Search on a later doc line found %d entries, want 1", len(got))
	}
}

// Directive 065, g42: test: filters both ways, as a boolean fact a
// result prints only when true.
func TestFilter_MatchesTestBothWays(t *testing.T) {
	valid := ValidAttributes(Slice(nil))
	if !valid["test"] {
		t.Fatal("test is not a filterable attribute")
	}
	prod := entry.Entry{Name: "Send", Path: "a.go"}
	tst := entry.Entry{Name: "TestSend", Path: "a_test.go", Test: true}
	f, err := Parse("test:true", valid)
	if err != nil {
		t.Fatal(err)
	}
	if f.Match(prod) || !f.Match(tst) {
		t.Errorf("test:true matched prod=%v tst=%v", f.Match(prod), f.Match(tst))
	}
	f, _ = Parse("test:false", valid)
	if !f.Match(prod) || f.Match(tst) {
		t.Errorf("test:false matched prod=%v tst=%v", f.Match(prod), f.Match(tst))
	}
}

// TestFilter_MatchesCallersBySite (directive 050): callers: matches a
// site as the line prints it, or any substring of one — a file, to
// keep what that file calls.
func TestFilter_MatchesCallersBySite(t *testing.T) {
	e := entry.Entry{Name: "Snapshot", Kind: "method", Callers: []entry.Call{
		{Path: "cluster.go", Name: "Snapshot", Line: 4245, Via: "metrics"},
		{Path: "internal/core/monitoring.go", Name: "Snapshot", Line: 106, Via: "Metrics"},
	}}
	for v, want := range map[string]bool{
		"cluster.go:4245": true, "monitoring.go": true, "internal/core": true,
		"cluster.go:4246": false, "metrics_test.go": false,
	} {
		if got := (Filter{"callers": {v}}).Match(e); got != want {
			t.Errorf("callers:%s matched %v, want %v", v, got, want)
		}
	}
	if (Filter{"callers": {"cluster.go"}}).Match(entry.Entry{Name: "Snapshot"}) {
		t.Error("an entry with no callers matches no callers: value")
	}
	if !ValidAttributes(Slice(nil))["callers"] {
		t.Error("callers is a common attribute, valid in every index")
	}
}

// TestFilter_MatchesTest (directive 051 §1): test: matches either
// value, so a caller can narrow to a test file's own declarations and
// call sites (test:true) or exclude them (test:false) — unlike scope's
// print-only-the-informative-case convention, both boolean values are
// real, useful constraints here.
func TestFilter_MatchesTest(t *testing.T) {
	prod := entry.Entry{Name: "Snapshot", Kind: "method", Test: false}
	test := entry.Entry{Name: "TestSnapshot", Kind: "func", Test: true}
	for v, want := range map[string]struct{ prod, test bool }{
		"true":  {false, true},
		"false": {true, false},
	} {
		if got := (Filter{"test": {v}}).Match(prod); got != want.prod {
			t.Errorf("test:%s matched production entry %v, want %v", v, got, want.prod)
		}
		if got := (Filter{"test": {v}}).Match(test); got != want.test {
			t.Errorf("test:%s matched test entry %v, want %v", v, got, want.test)
		}
	}
	if !ValidAttributes(Slice(nil))["test"] {
		t.Error("test is a common attribute, valid in every index")
	}
}

// TestSearch_AttachesCallersOnlyWhenTheFilterAsks (directive 050): a
// callers: constraint is applied to the sites the source holds; without
// one, Search returns entries as decoded and leaves the page's callers
// to the caller (main.withCallers) — a store read each, not spent on
// entries never printed.
func TestSearch_AttachesCallersOnlyWhenTheFilterAsks(t *testing.T) {
	sites := []entry.Call{{Path: "a.go", Name: "Snapshot", Line: 1}}
	src := Slice([]entry.Entry{
		{Path: "b.go", Name: "Snapshot", Kind: "func", Project: "p", Exported: true, Callers: sites},
		{Path: "c.go", Name: "Snapshots", Kind: "func", Project: "p", Exported: true},
	})
	got := Search(src, "snapshot", Filter{"callers": {"a.go"}})
	if len(got) != 1 || got[0].Name != "Snapshot" || len(got[0].Callers) != 1 {
		t.Fatalf("Search with callers:a.go = %+v, want Snapshot alone, carrying its site", got)
	}
	got = Browse(src, Filter{"callers": {"a.go"}})
	if len(got) != 1 || got[0].Name != "Snapshot" {
		t.Fatalf("Browse with callers:a.go = %+v, want Snapshot alone", got)
	}
	if got := Search(src, "snapshots", nil); len(got) != 1 || got[0].Callers != nil {
		t.Errorf("Search without a callers constraint attached %+v", got)
	}
	if got := WithCallers(entry.Entry{Name: "Snapshot", Kind: "func"}, src); len(got.Callers) != 1 {
		t.Errorf("WithCallers = %+v, want the one site", got.Callers)
	}
	if got := WithCallers(entry.Entry{Name: "Snapshot", Kind: entry.KindFile}, src); got.Callers != nil {
		t.Errorf("a file entry is not called: %+v", got.Callers)
	}
}

// TestOrderCallers_QualifierNamingThePositionComesFirst (directive 050):
// among a name's sites, the ones whose qualifier names one of the
// entry's own position values — the whole value or its last word,
// case-insensitively — come first; the rest keep their stored order.
// A one-letter qualifier that happens to end the value does not count.
func TestOrderCallers_QualifierNamingThePositionComesFirst(t *testing.T) {
	e := entry.Entry{Name: "Snapshot", Kind: "method", Position: []entry.Position{
		{Name: "module", Value: "internal/core"}, {Name: "receiver", Value: "NodeMetrics"},
	}}
	stored := []entry.Call{
		{Path: "cluster.go", Line: 3788, Via: "g"},
		{Path: "cluster.go", Line: 4245, Via: "metrics"},
		{Path: "cluster/ddata/ormap.go", Line: 305, Via: "s"},
		{Path: "internal/core/monitoring.go", Line: 106, Via: "Metrics"},
		{Path: "x.go", Line: 1},
	}
	got := orderCallers(e, stored)
	wantLines := []int{4245, 106, 3788, 305, 1}
	for i, w := range wantLines {
		if got[i].Line != w {
			t.Fatalf("order = %+v, want lines %v", got, wantLines)
		}
	}
	if stored[0].Line != 3788 {
		t.Error("orderCallers must not reorder the caller's slice")
	}
	for _, c := range []struct {
		via, value string
		want       bool
	}{
		{"metrics", "NodeMetrics", true}, {"Metrics", "Metrics", true}, {"provider", "ClusterProvider", true},
		{"s", "NodeMetrics", false}, {"trics", "NodeMetrics", false}, {"", "NodeMetrics", false}, {"NodeMetricsX", "NodeMetrics", false},
	} {
		if got := namesValue(c.via, c.value); got != c.want {
			t.Errorf("namesValue(%q, %q) = %v, want %v", c.via, c.value, got, c.want)
		}
	}
}

// TestSearch_CodeBeforeTestsWithinATier (directive 065, g49): at the
// same tier, an entry from a test file ranks after every entry from
// the code, whatever their file or scope ranks would say.
func TestSearch_CodeBeforeTestsWithinATier(t *testing.T) {
	entries := []entry.Entry{
		{Name: "Actors", Kind: "method", Path: "internal/management/debug_test.go", Exported: true, Test: true},
		{Name: "actors", Kind: "field", Path: "cluster.go", Exported: false},
		{Name: "Actors", Kind: "method", Path: "cluster.go", Exported: true},
	}
	got := Search(Slice(entries), "actors", nil)
	if len(got) != 3 || got[2].Path != "internal/management/debug_test.go" {
		t.Fatalf("expected the test file's Actors last, got %v", got)
	}
}

// TestAttributeCallers (directive 072): when the index declares a name
// more than once, a result shows only the sites that are its own — a
// qualifier naming one of its position values, a qualifier equal to
// its directory's last element, or a site in its own directory — and
// counts the rest; when the name is declared once, every site is its.
func TestAttributeCallers(t *testing.T) {
	e := entry.Entry{Name: "Prefix", Kind: "field", Path: "cmd/logs/logs.go", Position: []entry.Position{
		{Name: "module", Value: "cmd/logs"}, {Name: "type", Value: "LogsOptions"},
	}}
	stored := []entry.Call{
		{Path: "client/rest/client_test.go", Line: 84, Via: "Get", Test: true},
		{Path: "client/rest/client_test.go", Line: 90, Via: "Get", Test: true},
		{Path: "cmd/logs/logs.go", Line: 210, Via: "o"},
		{Path: "cmd/run/run.go", Line: 12, Via: "logs"},
		{Path: "other/x.go", Line: 5, Via: "client"},
		{Path: "other/y.go", Line: 7, Via: "LogsOptions"},
	}
	own, elsewhere := attributeCallers(e, stored, 3)
	if elsewhere != 3 {
		t.Errorf("elsewhere = %d, want 3 (the two REST sites and client.Prefix)", elsewhere)
	}
	if len(own) != 3 || own[0].Line != 7 || own[1].Line != 210 || own[2].Line != 12 {
		t.Errorf("own = %+v, want y.go:7 (names the type) then logs.go:210 (own directory) then run.go:12 (package qualifier)", own)
	}
	own, elsewhere = attributeCallers(e, stored, 1)
	if len(own) != 6 || elsewhere != 0 {
		t.Errorf("declared once: own = %d elsewhere = %d, want every site and 0", len(own), elsewhere)
	}
	if own, elsewhere := attributeCallers(e, nil, 1); own != nil || elsewhere != 0 {
		t.Error("no sites, nothing")
	}
	// A root file's directory is ".", which no qualifier names.
	root := entry.Entry{Name: "Run", Kind: "func", Path: "main.go"}
	own, elsewhere = attributeCallers(root, []entry.Call{{Path: "pkg/a.go", Line: 1, Via: "."}, {Path: "b.go", Line: 2}}, 2)
	if len(own) != 1 || own[0].Line != 2 || elsewhere != 1 {
		t.Errorf("root: own = %+v elsewhere = %d, want only the root-directory site", own, elsewhere)
	}
}

// TestDeclarationsNamed counts the declarations sharing a name, files
// excluded, and takes a name too short for a trigram as shared.
func TestDeclarationsNamed(t *testing.T) {
	src := Slice([]entry.Entry{
		{Name: "Snapshot", Kind: "method", Path: "a.go"},
		{Name: "snapshot", Kind: "func", Path: "b.go"},
		{Name: "Snapshot", Kind: "method", Path: "a.go"}, // the same declaration seen twice
		{Name: "Snapshot", Kind: entry.KindFile, Path: "Snapshot"},
		{Name: "SnapshotAll", Kind: "func", Path: "c.go"},
	})
	if got := declarationsNamed(src, "Snapshot"); got != 2 {
		t.Errorf("declarationsNamed = %d, want 2", got)
	}
	if got := declarationsNamed(src, "Go"); got != 2 {
		t.Errorf("a two-letter name is taken as shared, got %d", got)
	}
	if got := declarationsNamed(src, "Nothing"); got != 0 {
		t.Errorf("unknown name = %d, want 0", got)
	}
}

// TestWithCallers_AttributesAmongSeveralDeclarations: through
// WithCallers, a name declared twice keeps only its own sites.
func TestWithCallers_AttributesAmongSeveralDeclarations(t *testing.T) {
	sites := []entry.Call{{Path: "pkg/a/a.go", Line: 1, Via: "x"}, {Path: "pkg/b/b.go", Line: 2, Via: "y"}}
	src := Slice([]entry.Entry{
		{Name: "Do", Kind: "func", Path: "pkg/a/a.go", Callers: sites},
		{Name: "Do", Kind: "func", Path: "pkg/b/b.go"},
	})
	got := WithCallers(entry.Entry{Name: "Do", Kind: "func", Path: "pkg/a/a.go"}, src)
	if len(got.Callers) != 1 || got.Callers[0].Line != 1 || got.CallersElsewhere != 1 {
		t.Errorf("got callers %+v elsewhere %d, want a.go:1 and 1 elsewhere", got.Callers, got.CallersElsewhere)
	}
}

// Directive 073: a type carries the declarations whose owning position
// names it, in its own directory, in source order; a member of another
// directory's type of the same name does not count.
func TestWithMembers(t *testing.T) {
	pkg := entry.Position{Name: "package", Value: "m"}
	src := Slice([]entry.Entry{
		{Path: "m/t.go", Name: "T", Kind: "type", Line: 3, EndLine: 9, Position: []entry.Position{pkg}, Exported: true},
		{Path: "m/t.go", Name: "Get", Kind: "method", Line: 20, EndLine: 24, Position: []entry.Position{pkg, {Name: "receiver", Value: "T"}}, Exported: true},
		{Path: "m/t.go", Name: "n", Kind: "field", Line: 4, Position: []entry.Position{pkg, {Name: "type", Value: "T"}}, Exported: true},
		{Path: "o/t.go", Name: "Other", Kind: "method", Line: 5, EndLine: 6, Position: []entry.Position{{Name: "package", Value: "o"}, {Name: "receiver", Value: "T"}}, Exported: true},
	})
	target := entry.Entry{Path: "m/t.go", Name: "T", Kind: "type", Line: 3, EndLine: 9, Position: []entry.Position{pkg}}
	WithMembers([]*entry.Entry{&target}, src)
	var got []string
	for _, m := range target.Members {
		got = append(got, m.Name)
	}
	if strings.Join(got, ",") != "n,Get" {
		t.Fatalf("members = %v, want [n Get]", got)
	}
}

// Directive 073: results from test files follow the code's, each side
// in its own order.
func TestCodeFirst(t *testing.T) {
	in := []entry.Entry{{Name: "T1", Test: true}, {Name: "A"}, {Name: "T2", Test: true}, {Name: "B"}}
	var got []string
	for _, e := range CodeFirst(in) {
		got = append(got, e.Name)
	}
	if strings.Join(got, ",") != "A,B,T1,T2" {
		t.Fatalf("CodeFirst = %v", got)
	}
}

// Directive 073: a file carries its outline — the declarations in it
// that are not members, in source order.
func TestWithMembers_FileOutline(t *testing.T) {
	pkg := entry.Position{Name: "package", Value: "m"}
	src := Slice([]entry.Entry{
		{Path: "m/t.go", Name: "t", Kind: entry.KindFile, Exported: true},
		{Path: "m/t.go", Name: "New", Kind: "func", Line: 12, EndLine: 14, Position: []entry.Position{pkg}, Exported: true},
		{Path: "m/t.go", Name: "T", Kind: "type", Line: 3, EndLine: 9, Position: []entry.Position{pkg}, Exported: true},
		{Path: "m/t.go", Name: "n", Kind: "field", Line: 4, Position: []entry.Position{pkg, {Name: "type", Value: "T"}}, Exported: true},
		{Path: "m/u.go", Name: "U", Kind: "type", Line: 1, EndLine: 2, Position: []entry.Position{pkg}, Exported: true},
	})
	file := entry.Entry{Path: "m/t.go", Name: "t", Kind: entry.KindFile}
	WithMembers([]*entry.Entry{&file}, src)
	var got []string
	for _, m := range file.Members {
		got = append(got, m.Name)
	}
	if strings.Join(got, ",") != "T,New" {
		t.Fatalf("outline = %v, want [T New]", got)
	}
}

// Directive 073: same-named declarations that tie on every rank come
// larger body first, before the path decides.
func TestSearch_LargerDeclarationFirst(t *testing.T) {
	pos := func(owner string) []entry.Position {
		return []entry.Position{{Name: "package", Value: "p"}, {Name: "receiver", Value: owner}}
	}
	src := Slice([]entry.Entry{
		{Path: "a/a.go", Name: "Tell", Kind: "method", Line: 10, EndLine: 12, Position: pos("A"), Exported: true},
		{Path: "b/b.go", Name: "Tell", Kind: "method", Line: 10, EndLine: 90, Position: pos("B"), Exported: true},
	})
	got := Search(src, "Tell", nil)
	if len(got) != 2 || got[0].Path != "b/b.go" {
		t.Fatalf("order: %v", got)
	}
}

// Directive 073: a use — a site that refers to the name without calling
// it — is listed only under a constant or a variable; a field of the
// same name gets the calls alone, and the use is not counted elsewhere.
func TestWithCallers_UsesOnlyUnderConstantsAndVariables(t *testing.T) {
	sites := []entry.Call{{Path: "pkg/a/a.go", Line: 1, Via: "a", Use: true}, {Path: "pkg/a/a.go", Line: 2, Via: "a"}}
	src := Slice([]entry.Entry{{Name: "Limit", Kind: "field", Path: "pkg/a/a.go", Callers: sites}})
	if got := WithCallers(entry.Entry{Name: "Limit", Kind: "field", Path: "pkg/a/a.go"}, src); len(got.Callers) != 1 || got.Callers[0].Use || got.CallersElsewhere != 0 {
		t.Errorf("field: got callers %+v elsewhere %d, want the call alone", got.Callers, got.CallersElsewhere)
	}
	for _, kind := range []string{"const", "var"} {
		if got := WithCallers(entry.Entry{Name: "Limit", Kind: kind, Path: "pkg/a/a.go"}, src); len(got.Callers) != 2 {
			t.Errorf("%s: got callers %+v, want the use and the call", kind, got.Callers)
		}
	}
}
