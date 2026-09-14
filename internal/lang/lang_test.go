package lang

import (
	"testing"

	"giraffe/internal/entry"
	"giraffe/internal/scanner"
)

// TestExtract_DispatchesGoToTheGoModule confirms the registration order
// actually routes a .go file's content through the go module, not
// straight to the generic one (directive 016 §1) — its own entry, plus
// declarations.
func TestExtract_DispatchesGoToTheGoModule(t *testing.T) {
	f := scanner.WithContent("retry.go", []byte("package p\n\nfunc Retry() {}\n"))
	entries, declined := Extract(f, "giraffe", nil, nil)
	if declined != nil {
		t.Fatalf("unexpected declined error: %v", declined)
	}
	var sawFile, sawFunc bool
	for _, e := range entries {
		switch {
		case e.Kind == entry.KindFile:
			sawFile = true
			if e.Lang != "go" {
				t.Errorf("file entry Lang = %q, want %q", e.Lang, "go")
			}
		case e.Name == "Retry":
			sawFunc = true
			if e.Lang != "go" {
				t.Errorf("Retry entry Lang = %q, want %q", e.Lang, "go")
			}
		}
	}
	if !sawFile || !sawFunc {
		t.Errorf("expected both a file entry and a Retry func entry, got %v", entries)
	}
}

// TestExtract_FallsThroughToTheGenericModule (directive 016 §1): an
// extension no registered extraction module claims still produces
// exactly the file's own entry, via the generic module last in the
// order — nothing reaches the end of the chain unhandled.
func TestExtract_FallsThroughToTheGenericModule(t *testing.T) {
	f := scanner.WithContent("README.md", []byte("# hello\n"))
	entries, declined := Extract(f, "giraffe", nil, nil)
	if declined != nil {
		t.Fatalf("unexpected declined error: %v", declined)
	}
	if len(entries) != 1 || entries[0].Kind != entry.KindFile || entries[0].Name != "README" {
		t.Fatalf("entries = %v, want exactly one file entry named README", entries)
	}
	if entries[0].Lang != "" {
		t.Errorf("Lang = %q, want absent (no operator assignment)", entries[0].Lang)
	}
}

// TestExtract_GenericModuleUsesOperatorAssignment (§25 C-26): an
// extension no extractor recognises still gets a Lang value when the
// operator assigned one in .giraffe/exclude.
func TestExtract_GenericModuleUsesOperatorAssignment(t *testing.T) {
	f := scanner.WithContent("x.inc", []byte("int x;\n"))
	entries, declined := Extract(f, "giraffe", map[string]string{".inc": "c"}, nil)
	if declined != nil {
		t.Fatalf("unexpected declined error: %v", declined)
	}
	if len(entries) != 1 || entries[0].Lang != "c" {
		t.Fatalf("entries = %v, want one file entry with Lang %q", entries, "c")
	}
}

// TestExtract_SameExtensionDifferentLanguageFallsThrough (directive 016
// §1): a .go file whose content is not valid Go is declined by the go
// module (its own extension matched, its content did not) and still
// produces a file entry via the generic module, with the parse error
// surfaced as declined rather than dropped.
func TestExtract_SameExtensionDifferentLanguageFallsThrough(t *testing.T) {
	f := scanner.WithContent("not-really-go.go", []byte("this is not go source {{{"))
	entries, declined := Extract(f, "giraffe", nil, nil)
	if declined == nil {
		t.Fatal("expected a declined diagnostic from the go module's failed parse")
	}
	if len(entries) != 1 || entries[0].Kind != entry.KindFile {
		t.Fatalf("entries = %v, want exactly one file entry from the generic module", entries)
	}
	if entries[0].Lang != "" {
		// The go module declined, so the extension no longer implies
		// "go" — same-extension-different-language falls out without a
		// special case (directive 016 §1).
		t.Errorf("Lang = %q, want absent: a declined module must not still label the file", entries[0].Lang)
	}
}

// TestExtract_ExcludedExtensionProducesNoEntryAndIsNeverRead (directive
// 017 §1, §2): a denied extension is claimed by the exclusion module —
// no entries at all, not even the generic module's file entry — and,
// since scanner.Open defers the read, a file that does not even exist
// on disk still succeeds with no error, proving the content was never
// asked for.
func TestExtract_ExcludedExtensionProducesNoEntryAndIsNeverRead(t *testing.T) {
	f := scanner.Open(t.TempDir(), scanner.FileMeta{Path: "does-not-exist-anywhere.png"})
	entries, declined := Extract(f, "giraffe", nil, nil)
	if declined != nil {
		t.Fatalf("unexpected declined error (content should never have been read): %v", declined)
	}
	if len(entries) != 0 {
		t.Fatalf("entries = %v, want none: an excluded file produces nothing, not even a file entry", entries)
	}
}

// TestExtract_GoModuleDirectoryClaimMovesItFirst (directive 017 §3): a
// directory whose own marker (go.mod) is present dispatches its files
// through the go module first, ahead of where it would fall in plain
// registration order — exercised here through the same .go-content path
// TestExtract_DispatchesGoToTheGoModule already covers, but with
// dirMarkers actually populated, confirming the parameter reaches
// dispatch without changing the outcome for the one extraction module
// registered today.
func TestExtract_GoModuleDirectoryClaimMovesItFirst(t *testing.T) {
	f := scanner.WithContent("retry.go", []byte("package p\n\nfunc Retry() {}\n"))
	entries, declined := Extract(f, "giraffe", nil, map[string]bool{"go.mod": true})
	if declined != nil {
		t.Fatalf("unexpected declined error: %v", declined)
	}
	if len(entries) == 0 {
		t.Fatal("expected entries from the go module")
	}
}

// TestRegisteredNames_ExcludesTheGenericModulesOwnEmptyName: the
// generic module answers to no language name of its own, so it must
// never appear as a spurious entry in the breakdown ## index gives
// every known language (§25 C-24).
func TestRegisteredNames_ExcludesTheGenericModulesOwnEmptyName(t *testing.T) {
	got := RegisteredNames()
	if len(got) != 1 || got[0] != "go" {
		t.Errorf("RegisteredNames() = %v, want exactly [\"go\"]", got)
	}
}

// TestRegisteredMarkers_NamesGoModOnce (directive 017 §3): the one
// place go.mod is named as a root marker is the golang module; this is
// how scanner's root discovery learns it.
func TestRegisteredMarkers_NamesGoModOnce(t *testing.T) {
	got := RegisteredMarkers()
	if len(got) != 1 || got[0] != "go.mod" {
		t.Errorf("RegisteredMarkers() = %v, want exactly [\"go.mod\"]", got)
	}
}

// TestOrderExtractionModules_MarkerMatchMovesToFront (directive 017
// §3): against a synthetic module set — independent of how many real
// language modules are registered today — the module whose own marker
// is present in dirMarkers moves to the front, and modules that do not
// match keep their relative registration order.
func TestOrderExtractionModules_MarkerMatchMovesToFront(t *testing.T) {
	mods := []module{
		{name: "a", marker: "a.marker"},
		{name: "b", marker: "b.marker"},
		{name: "c", marker: ""}, // no marker of its own
	}
	got := orderExtractionModules(mods, map[string]bool{"b.marker": true})
	wantOrder(t, got, "b", "a", "c")
}

// TestOrderExtractionModules_NoClaimKeepsRegistrationOrder (directive
// 017 §3: "a simple single-language repository is unaffected — the
// chain is short and the answer is the same either way"): with no
// marker present, or dirMarkers nil, order is left exactly as
// registered.
func TestOrderExtractionModules_NoClaimKeepsRegistrationOrder(t *testing.T) {
	mods := []module{
		{name: "a", marker: "a.marker"},
		{name: "b", marker: "b.marker"},
	}
	got := orderExtractionModules(mods, nil)
	wantOrder(t, got, "a", "b")
}

func wantOrder(t *testing.T, got []module, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("order = %v, want %v", names(got), want)
	}
	for i, w := range want {
		if got[i].name != w {
			t.Errorf("order = %v, want %v", names(got), want)
			return
		}
	}
}

func names(mods []module) []string {
	out := make([]string, len(mods))
	for i, m := range mods {
		out[i] = m.name
	}
	return out
}
