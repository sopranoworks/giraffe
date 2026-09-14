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
	write("README.md", "not go\n")       // untracked, no extractor, still a candidate (directive 017 §2)
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
		"README.md":  false, // untracked, no extractor, still a candidate (directive 017 §2)
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
// staging it, for TestList_NoLongerAppliesAnyCoverageDefault below,
// which cares about path shape rather than tracked/untracked status.
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

// TestList_NoLongerAppliesAnyCoverageDefault (directive 017 §2: "the
// scanner stops knowing about extensions entirely" — supersedes §24
// C-19's coverage rules, which moved to giraffe/internal/lang/exclude).
// Every one of these paths used to be filtered by extension or
// extensionless shape here; List now returns all of them, since which
// of them is worth indexing is decided downstream, per file, by the
// language-module chain.
func TestList_NoLongerAppliesAnyCoverageDefault(t *testing.T) {
	dir := gitRepo(t)
	writeUntracked(t, dir, "Makefile", "all:\n\techo hi\n")
	writeUntracked(t, dir, "configure", "#!/bin/sh\n") // unknown extensionless shape; still a candidate now
	writeUntracked(t, dir, "logo.png", "\x89PNG")      // denied extension; still a candidate now
	writeUntracked(t, dir, "widget.cpp", "int main() {}\n")

	metas, err := List(context.Background(), dir, Scope{})
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]bool)
	for _, m := range metas {
		got[m.Path] = true
	}
	for _, want := range []string{"Makefile", "configure", "logo.png", "widget.cpp"} {
		if !got[want] {
			t.Errorf("expected %s to be a candidate; List no longer filters by extension or name shape", want)
		}
	}
}

func TestOpen_ReadsExactlyTheGivenPath(t *testing.T) {
	dir := gitRepo(t)
	f := Open(dir, FileMeta{Path: "main.go"})
	content, err := f.Content()
	if err != nil {
		t.Fatal(err)
	}
	if f.Path != "main.go" {
		t.Errorf("path = %q", f.Path)
	}
	if string(content) != "package main\n" {
		t.Errorf("content = %q", content)
	}
}

// TestOpen_DoesNotReadUntilContentIsCalled (directive 017 §1): Open
// itself never touches the filesystem — only Content does — so a File
// for a path that does not exist on disk is still constructed without
// error, and only errors once something actually asks for the bytes.
func TestOpen_DoesNotReadUntilContentIsCalled(t *testing.T) {
	dir := gitRepo(t)
	f := Open(dir, FileMeta{Path: "does-not-exist.go"})
	if _, err := f.Content(); err == nil {
		t.Fatal("expected Content() to fail for a nonexistent file")
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
