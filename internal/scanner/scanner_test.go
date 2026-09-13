package scanner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(rel, content string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	run("init", "-q")
	write("main.go", "package main\n")
	write("vendor/dep/dep.go", "package dep\n")
	write("pkg/util_test.go", "package pkg\n")
	run("add", "main.go", "vendor/dep/dep.go", "pkg/util_test.go")
	run("commit", "-q", "-m", "initial")
	write("pkg/new.go", "package pkg\n") // untracked
	write("README.md", "not go\n")       // untracked, no extractor but covered by §24 C-19
	dir, err := Root(dir)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestList_ResolvesTrackedAndUntracked_AppliesCoverageAndExclude (§24
// C-18): before this directive, Scope.Extensions gated the candidate
// list itself, so README.md never survived scanning at all — the
// coupling that made file-name search Go-only. List now returns every
// file surviving exclusion regardless of extension; which of those get
// an extractor run over them is a decision made downstream, not here.
func TestList_ResolvesTrackedAndUntracked_AppliesCoverageAndExclude(t *testing.T) {
	dir := gitRepo(t)
	metas, err := List(context.Background(), dir, Scope{})
	if err != nil {
		t.Fatal(err)
	}

	got := make(map[string]bool)
	for _, m := range metas {
		got[m.Path] = m.BlobHash != ""
	}

	want := map[string]bool{
		"main.go":    true,  // tracked
		"pkg/new.go": false, // untracked
		"README.md":  false, // untracked, no extractor, still covered by §24 C-19
	}
	for path, tracked := range want {
		hadHash, ok := got[path]
		if !ok {
			t.Errorf("expected %s in scan results, got %v", path, got)
			continue
		}
		if hadHash != tracked {
			t.Errorf("%s: expected tracked=%v (blob hash present), got %v", path, tracked, hadHash)
		}
	}

	for excluded := range map[string]bool{"vendor/dep/dep.go": true, "pkg/util_test.go": true} {
		if _, present := got[excluded]; present {
			t.Errorf("expected %s to be excluded, but it was scanned", excluded)
		}
	}
}

func TestList_NoDuplicatePaths(t *testing.T) {
	dir := gitRepo(t)
	metas, err := List(context.Background(), dir, Scope{})
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, m := range metas {
		if seen[m.Path] {
			t.Fatalf("%s appeared twice in scan results", m.Path)
		}
		seen[m.Path] = true
	}
}

func TestList_ScopeExcludeIsAdditional(t *testing.T) {
	dir := gitRepo(t)
	metas, err := List(context.Background(), dir, Scope{
		Exclude: []Pattern{"pkg/"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range metas {
		if m.Path == "pkg/new.go" {
			t.Fatalf("expected pkg/ to be excluded by scope, but found %s", m.Path)
		}
	}
	foundMain := false
	for _, m := range metas {
		if m.Path == "main.go" {
			foundMain = true
		}
	}
	if !foundMain {
		t.Fatal("expected main.go to still be scanned")
	}
}

// writeUntracked adds an extra file to an already-created repo without
// staging it, for the coverage tests below (§24 C-19), which care about
// path shape rather than tracked/untracked status.
func writeUntracked(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestList_CoversKnownExtensionlessFiles (§24 C-19, list 1): a known
// build/doc file with no extension is included by name; an unlisted
// extensionless file is skipped, since it has exactly the shape of a
// compiled binary and defaulting to include is the dangerous direction.
func TestList_CoversKnownExtensionlessFiles(t *testing.T) {
	dir := gitRepo(t)
	writeUntracked(t, dir, "Makefile", "all:\n\techo hi\n")
	writeUntracked(t, dir, "CMakeLists.txt", "project(x)\n") // known name despite its ".txt" extension
	writeUntracked(t, dir, "configure", "#!/bin/sh\n")       // unknown, truly extensionless: shape of a compiled binary

	metas, err := List(context.Background(), dir, Scope{})
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]bool)
	for _, m := range metas {
		got[m.Path] = true
	}
	if !got["Makefile"] {
		t.Error("expected Makefile to be covered")
	}
	if !got["CMakeLists.txt"] {
		t.Error("expected CMakeLists.txt to be covered by name, despite its .txt extension")
	}
	if got["configure"] {
		t.Error("expected configure (unlisted extensionless) to be skipped")
	}
}

// TestList_DeniesBinaryExtensions_IncludesEverythingElse (§24 C-19,
// list 2): the extension already declares the type, so only the deny
// list is excluded; anything extensioned and not on it — including a
// language giraffe has no extractor for — is covered.
func TestList_DeniesBinaryExtensions_IncludesEverythingElse(t *testing.T) {
	dir := gitRepo(t)
	writeUntracked(t, dir, "logo.png", "\x89PNG")
	writeUntracked(t, dir, "widget.cpp", "int main() {}\n")

	metas, err := List(context.Background(), dir, Scope{})
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]bool)
	for _, m := range metas {
		got[m.Path] = true
	}
	if got["logo.png"] {
		t.Error("expected logo.png (a denied extension) to be excluded")
	}
	if !got["widget.cpp"] {
		t.Error("expected widget.cpp to be covered even though giraffe has no C++ extractor")
	}
}

// TestList_NegatedPatternOverridesCoverage (§24 C-19: "extendable and
// overridable through .giraffe/exclude"): a leading "!" forces
// inclusion of a file the built-in deny list would otherwise drop.
func TestList_NegatedPatternOverridesCoverage(t *testing.T) {
	dir := gitRepo(t)
	writeUntracked(t, dir, "logo.png", "\x89PNG")

	metas, err := List(context.Background(), dir, Scope{Exclude: []Pattern{"!logo.png"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range metas {
		if m.Path == "logo.png" {
			return
		}
	}
	t.Fatalf("expected a negated pattern to force logo.png back in, got %v", metas)
}

func TestRead_ReadsExactlyTheGivenPaths(t *testing.T) {
	dir := gitRepo(t)
	files, err := Read(context.Background(), dir, []string{"main.go"})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("expected exactly 1 file, got %d", len(files))
	}
	if files[0].Path != "main.go" {
		t.Errorf("path = %q", files[0].Path)
	}
	if string(files[0].Content) != "package main\n" {
		t.Errorf("content = %q", files[0].Content)
	}
}

// TestList_MetasCarryStatSize (directive 009 item 3): FileMeta.Size is
// a real stat, for both tracked and untracked files — the fact a
// caller with no other signal (no extractor, or no declarations worth
// reporting) uses to decide whether to open a result directly or
// narrow it first.
func TestList_MetasCarryStatSize(t *testing.T) {
	dir := gitRepo(t)
	metas, err := List(context.Background(), dir, Scope{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{
		"main.go":    int64(len("package main\n")),
		"pkg/new.go": int64(len("package pkg\n")), // untracked
		"README.md":  int64(len("not go\n")),      // untracked
	}
	got := make(map[string]int64, len(want))
	for _, m := range metas {
		if _, ok := want[m.Path]; ok {
			got[m.Path] = m.Size
		}
	}
	for path, size := range want {
		if got[path] != size {
			t.Errorf("%s: Size = %d, want %d", path, got[path], size)
		}
	}
}
