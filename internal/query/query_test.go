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
	if _, err := Parse("name:Retry", valid); err == nil {
		t.Fatal("expected an error: name is valid but not filterable in this version")
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

// TestParse_NameStaysExcludedFromQuery: unchanged by directive 015 —
// find's positional word already substring-matches name, so --query
// itself continues to reject it (§23's "accepted as reported"). name
// was never given a second way in: directive 016 §2 withdrew show,
// which was the only other caller that needed it (ParseAddress), and
// browsing a place (§31.3) is entries at a path or project, never one
// singled out by name.
func TestParse_NameStaysExcludedFromQuery(t *testing.T) {
	valid := map[string]bool{"name": true}
	if _, err := Parse("name:Retry", valid); err == nil {
		t.Fatal("expected --query to still reject name")
	}
}

func TestBrowse_FiltersAndOrdersByPathThenName(t *testing.T) {
	entries := []entry.Entry{
		{Name: "B", Path: "b.go", Project: "giraffe", Exported: true},
		{Name: "Z", Path: "a.go", Project: "giraffe", Exported: true},
		{Name: "A", Path: "a.go", Project: "giraffe", Exported: true},
		{Name: "Elsewhere", Path: "elsewhere.go", Project: "other", Exported: true},
	}
	got := Browse(Slice(entries), Filter{"project": {"giraffe"}}, "giraffe")
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

func TestBrowse_HidesUnexportedOutsideItsOwnProject(t *testing.T) {
	entries := []entry.Entry{
		{Name: "helper", Path: "internal/x/x.go", Project: "giraffe", Exported: false},
	}
	if got := Browse(Slice(entries), Filter{"path": {"internal/x"}}, "giraffe"); len(got) != 1 {
		t.Fatalf("expected the package-scope entry visible from its own project, got %d", len(got))
	}
	if got := Browse(Slice(entries), Filter{"path": {"internal/x"}}, "gekka"); len(got) != 0 {
		t.Fatalf("expected the package-scope entry hidden from another project, got %d", len(got))
	}
}

func TestSuggest_ReportsMatchesHiddenByProjectVisibility(t *testing.T) {
	entries := []entry.Entry{
		{Name: "readLines", Project: "giraffe", Exported: false},
	}
	got := Suggest(Slice(entries), "readLines", "gekka")
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
	got := Suggest(Slice(entries), "Retri", "giraffe") // typo: not a substring of either name, so tier() misses both
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
	got := Suggest(Slice(entries), "showCommand", "giraffe")
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
	got := Suggest(Slice(entries), "browseEntries", "giraffe")
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
	got := Suggest(Slice(entries), "showCommand", "giraffe")
	if len(got) == 0 || !strings.Contains(got[0], "runShow") {
		t.Fatalf("expected the real token-overlap hint to survive the pre-filter, got %v", got)
	}
	// "showCluster" tokenizes to ["show", "cluster"]; "recluster"
	// contains "cluster" as a raw substring (satisfying the pre-filter)
	// but tokenizes to the single token "recluster" (no case boundary),
	// so it must not be offered as a near-token hint.
	got2 := Suggest(Slice(entries), "showCluster", "giraffe")
	for _, hint := range got2 {
		if strings.Contains(hint, "recluster") {
			t.Errorf("expected \"recluster\" to be excluded (substring-only overlap, no shared token), got %v", got2)
		}
	}
}

func TestSuggest_NilWhenNothingToOffer(t *testing.T) {
	entries := []entry.Entry{{Name: "Unrelated", Project: "giraffe", Exported: true}}
	if got := Suggest(Slice(entries), "zzz", "giraffe"); len(got) != 0 {
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
	got := Search(Slice(entries), "retry", nil, "")
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
	got := Search(Slice(entries), "retry", nil, "")
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
	got := Search(Slice(entries), "Cluster", nil, "")
	if len(got) != 4 {
		t.Fatalf("expected all four to match, got %d: %v", len(got), got)
	}
	if got[0].Kind != "type" {
		t.Errorf("expected the top-level type ranked first, got %s first", got[0].Kind)
	}
}

// TestSearch_TopLevelSubstringOutranksMemberExactMatch (§29 C-42): this
// is the case memberRank could not fix. Many unrelated types' fields
// are all named exactly "cluster" (tier: member exact match); a
// different top-level type merely contains "cluster" as a substring of
// its own name (tier: top-level substring match). The top-level
// substring match must still outrank the member exact matches, because
// it is a real declaration answering the same search, not a tie-break
// among entries already sharing a tier.
func TestSearch_TopLevelSubstringOutranksMemberExactMatch(t *testing.T) {
	entries := []entry.Entry{
		{Name: "cluster", Kind: "field", Path: "deployment_config.go", Exported: true},
		{Name: "cluster", Kind: "field", Path: "watcher_actor.go", Exported: true},
		{Name: "ClusterBootstrap", Kind: "type", Path: "cluster/bootstrap.go", Exported: true},
	}
	got := Search(Slice(entries), "Cluster", nil, "")
	if len(got) != 3 {
		t.Fatalf("expected all three to match, got %d: %v", len(got), got)
	}
	if got[0].Name != "ClusterBootstrap" {
		t.Errorf("expected ClusterBootstrap (top-level substring match) ranked first, got %s (%s) first", got[0].Name, got[0].Kind)
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
	got := Search(Slice(entries), "GetMailboxLengths", nil, "")
	if len(got) != 1 {
		t.Fatalf("expected exactly one match, got %d: %v", len(got), got)
	}
	if got[0].Name != "GetMailboxLengths" {
		t.Errorf("expected the method itself, got %s", got[0].Name)
	}
}

func TestSearch_NoMatchIsExcluded(t *testing.T) {
	entries := []entry.Entry{{Name: "Unrelated", Doc: "nothing here", Path: "x.go"}}
	if got := Search(Slice(entries), "retry", nil, ""); len(got) != 0 {
		t.Fatalf("expected no matches, got %v", got)
	}
}

func TestSearch_ExportedRanksAboveUnexportedInSameTier(t *testing.T) {
	entries := []entry.Entry{
		{Name: "retry", Scope: "package", Exported: false, Project: "giraffe", Path: "x.go"},
		{Name: "Retry", Scope: "exported", Exported: true, Project: "giraffe", Path: "y.go"},
	}
	got := Search(Slice(entries), "retry", nil, "giraffe")
	if len(got) != 2 {
		t.Fatalf("expected both to match by name (case-insensitive), got %d: %v", len(got), got)
	}
	if !got[0].Exported {
		t.Errorf("expected the exported entry first, got %s (exported %v)", got[0].Name, got[0].Exported)
	}
}

func TestSearch_PackageScopeHiddenOutsideItsOwnProject(t *testing.T) {
	entries := []entry.Entry{
		{Name: "readLines", Scope: "package", Exported: false, Project: "giraffe", Path: "internal/index/refresh.go"},
	}
	if got := Search(Slice(entries), "readLines", nil, "giraffe"); len(got) != 1 {
		t.Fatalf("expected the package-scope entry visible from its own project, got %d: %v", len(got), got)
	}
	if got := Search(Slice(entries), "readLines", nil, "gekka"); len(got) != 0 {
		t.Fatalf("expected the package-scope entry hidden from a different project, got %d: %v", len(got), got)
	}
}

func TestSearch_ExportedVisibleFromEveryProject(t *testing.T) {
	entries := []entry.Entry{
		{Name: "Retry", Scope: "exported", Exported: true, Project: "giraffe", Path: "internal/retry/retry.go"},
	}
	if got := Search(Slice(entries), "Retry", nil, "gekka"); len(got) != 1 {
		t.Fatalf("expected the exported entry visible from a different project, got %d: %v", len(got), got)
	}
}
