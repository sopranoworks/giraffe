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
	f := scanner.File{Path: "retry.go", Content: []byte("package p\n\nfunc Retry() {}\n")}
	entries, declined := Extract(f, "giraffe", nil)
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
// extension no registered module claims still produces exactly the
// file's own entry, via the generic module last in the order — nothing
// reaches the end of the list unhandled.
func TestExtract_FallsThroughToTheGenericModule(t *testing.T) {
	f := scanner.File{Path: "README.md", Content: []byte("# hello\n")}
	entries, declined := Extract(f, "giraffe", nil)
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
	f := scanner.File{Path: "x.inc", Content: []byte("int x;\n")}
	entries, declined := Extract(f, "giraffe", map[string]string{".inc": "c"})
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
	f := scanner.File{Path: "not-really-go.go", Content: []byte("this is not go source {{{")}
	entries, declined := Extract(f, "giraffe", nil)
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
