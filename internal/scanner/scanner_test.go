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

// scanAll resolves dir's whole candidate set exactly the way
// index/enum.go's own enumerate stage does (directive 021): the tracked
// half via StreamTracked where HasGitDir, everything else via Walk +
// FilterWalked, skipping whatever the tracked half already reported. A
// scanner-package-only stand-in for that production wiring, so these
// tests exercise the real two-route mechanism rather than a retired
// batch API.
func scanAll(t *testing.T, dir string, scope Scope) []FileMeta {
	t.Helper()
	var metas []FileMeta
	trackedPaths := map[string]bool{}
	if HasGitDir(dir) {
		if err := StreamTracked(context.Background(), dir, scope,
			func(m FileMeta) { metas = append(metas, m); trackedPaths[m.Path] = true },
			func(string) {},
		); err != nil {
			t.Fatal(err)
		}
	}
	walked, _, err := Walk(context.Background(), dir, WalkCache{})
	if err != nil {
		t.Fatal(err)
	}
	FilterWalked(dir, walked, trackedPaths, scope, func(m FileMeta) { metas = append(metas, m) }, func(string) {})
	return metas
}

// TestScan_ResolvesTrackedAndWalked_AppliesExclude (§24 C-18): every
// file surviving exclusion is a candidate regardless of extension;
// which of those get an extractor run over them is a decision made
// downstream, not here.
func TestScan_ResolvesTrackedAndWalked_AppliesExclude(t *testing.T) {
	dir := gitRepo(t)
	metas := scanAll(t, dir, Scope{})

	got := make(map[string]bool)
	for _, m := range metas {
		got[m.Path] = m.BlobHash != ""
	}

	want := map[string]bool{
		"main.go":    true,  // tracked
		"pkg/new.go": false, // walked (untracked)
		"README.md":  false, // walked, no extractor, still a candidate (directive 017 §2)
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

func TestScan_NoDuplicatePaths(t *testing.T) {
	dir := gitRepo(t)
	metas := scanAll(t, dir, Scope{})
	seen := make(map[string]bool)
	for _, m := range metas {
		if seen[m.Path] {
			t.Fatalf("%s appeared twice in scan results", m.Path)
		}
		seen[m.Path] = true
	}
}

func TestScan_ScopeExcludeIsAdditional(t *testing.T) {
	dir := gitRepo(t)
	metas := scanAll(t, dir, Scope{Exclude: []Pattern{"pkg/"}})
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
// staging it, for TestScan_NoLongerAppliesAnyCoverageDefault below,
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

// TestScan_NoLongerAppliesAnyCoverageDefault (directive 017 §2: "the
// scanner stops knowing about extensions entirely" — supersedes §24
// C-19's coverage rules, which moved to giraffe/internal/lang/exclude).
// Every one of these paths used to be filtered by extension or
// extensionless shape here; scanning now returns all of them, since
// which of them is worth indexing is decided downstream, per file, by
// the language-module chain.
func TestScan_NoLongerAppliesAnyCoverageDefault(t *testing.T) {
	dir := gitRepo(t)
	writeUntracked(t, dir, "Makefile", "all:\n\techo hi\n")
	writeUntracked(t, dir, "configure", "#!/bin/sh\n") // unknown extensionless shape; still a candidate now
	writeUntracked(t, dir, "logo.png", "\x89PNG")      // denied extension; still a candidate now
	writeUntracked(t, dir, "widget.cpp", "int main() {}\n")

	metas := scanAll(t, dir, Scope{})
	got := make(map[string]bool)
	for _, m := range metas {
		got[m.Path] = true
	}
	for _, want := range []string{"Makefile", "configure", "logo.png", "widget.cpp"} {
		if !got[want] {
			t.Errorf("expected %s to be a candidate; scanning no longer filters by extension or name shape", want)
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

// TestScan_MetasCarryStatSize (directive 009 item 3): FileMeta.Size is
// a real stat, for both tracked and walked files — the fact a caller
// with no other signal (no extractor, or no declarations worth
// reporting) uses to decide whether to open a result directly or
// narrow it first.
func TestScan_MetasCarryStatSize(t *testing.T) {
	dir := gitRepo(t)
	metas := scanAll(t, dir, Scope{})
	want := map[string]int64{
		"main.go":    int64(len("package main\n")),
		"pkg/new.go": int64(len("package pkg\n")), // walked
		"README.md":  int64(len("not go\n")),      // walked
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

// TestHasGitDir (directive 021 item 1): the structural check that
// decides which route enumeration takes, without running git at all.
func TestHasGitDir(t *testing.T) {
	gitDir := gitRepo(t)
	if !HasGitDir(gitDir) {
		t.Error("expected HasGitDir to be true for a real git checkout")
	}

	noGit := t.TempDir()
	if HasGitDir(noGit) {
		t.Error("expected HasGitDir to be false for a plain directory")
	}
}

// TestWalk_NoGitRoot_FindsEverything (directive 021 item 1's own
// completion condition): a directory with no .git at all still
// produces a full result via Walk alone — the bug this directive
// fixes was List/StreamTracked erroring outright ("fatal: not a git
// repository") instead.
func TestWalk_NoGitRoot_FindsEverything(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "b.go"), []byte("package sub\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if HasGitDir(dir) {
		t.Fatal("test setup: expected no .git in a fresh temp dir")
	}
	files, _, err := Walk(context.Background(), dir, WalkCache{})
	if err != nil {
		t.Fatalf("Walk on a directory with no .git: %v", err)
	}
	want := map[string]bool{"a.go": true, "sub/b.go": true}
	got := make(map[string]bool, len(files))
	for _, f := range files {
		got[f] = true
	}
	for w := range want {
		if !got[w] {
			t.Errorf("expected %s among Walk's results, got %v", w, files)
		}
	}
}

// TestWalk_PrunesDefaultExcludeDirsAndGit confirms vendor/, node_modules/
// and .git are never descended into at all — not filtered out
// afterward, which the file count alone cannot distinguish, so this
// checks the returned WalkCache never gained an entry for them either
// (proof the directory itself was never even stat'd).
func TestWalk_PrunesDefaultExcludeDirsAndGit(t *testing.T) {
	dir := t.TempDir()
	for _, rel := range []string{"vendor/dep/dep.go", "node_modules/pkg/index.js", ".git/objects/pack", "keep.go"} {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	files, cache, err := Walk(context.Background(), dir, WalkCache{})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f != "keep.go" {
			t.Errorf("expected only keep.go, but Walk returned %s", f)
		}
	}
	for _, pruned := range []string{"vendor", "vendor/dep", "node_modules", "node_modules/pkg", ".git", ".git/objects"} {
		if _, ok := cache.Dirs[pruned]; ok {
			t.Errorf("expected %s to never be visited (pruned), but it has a WalkCache entry", pruned)
		}
	}
}

// TestWalk_ReusesCachedDirectoryWhenMtimeUnchanged is the positive
// proof of item 2's own mechanism: a directory whose prev cache entry
// claims the same mtime as disk is trusted outright, not re-read — this
// tampers the cached file list to a value that could not have come
// from a fresh os.ReadDir, and confirms Walk returns the tampered value,
// which only happens if it skipped ReadDir entirely.
func TestWalk_ReusesCachedDirectoryWhenMtimeUnchanged(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "real.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}

	prev := WalkCache{Dirs: map[string]WalkDirEntry{
		".": {ModTime: info.ModTime().UnixNano(), Files: []string{"cached-only.go"}},
	}}
	files, _, err := Walk(context.Background(), dir, prev)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0] != "cached-only.go" {
		t.Fatalf("Walk = %v, want [cached-only.go] (the cached entry, proving the unchanged directory was not re-read)", files)
	}
}

// TestWalk_RereadsDirectoryWhenMtimeMoved is
// TestWalk_ReusesCachedDirectoryWhenMtimeUnchanged's counterpart: a
// stale cache entry (any mtime that does not match disk) is ignored,
// and the directory is read fresh — the correctness half of item 2's
// own requirement, not just the speed half.
func TestWalk_RereadsDirectoryWhenMtimeMoved(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "real.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	prev := WalkCache{Dirs: map[string]WalkDirEntry{
		".": {ModTime: 1, Files: []string{"stale-cached.go"}}, // an mtime that cannot match disk
	}}
	files, _, err := Walk(context.Background(), dir, prev)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0] != "real.go" {
		t.Fatalf("Walk = %v, want [real.go] (a stale cache entry must not be trusted)", files)
	}
}

// TestFilterWalked_SkipsTrackedPaths (directive 021 item 2): a path the
// tracked half already reported is never reconsidered by the walked
// half, even if it would otherwise survive exclusion — the invariant
// that keeps a git repository's two enumeration routes from ever
// emitting the same path twice.
func TestFilterWalked_SkipsTrackedPaths(t *testing.T) {
	dir := t.TempDir()
	var got []FileMeta
	FilterWalked(dir, []string{"a.go", "b.go"}, map[string]bool{"a.go": true}, Scope{},
		func(m FileMeta) { got = append(got, m) },
		func(string) {},
	)
	if len(got) != 1 || got[0].Path != "b.go" {
		t.Errorf("FilterWalked = %v, want only b.go (a.go is in skip)", got)
	}
}
