package scanner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
		if err := StreamTracked(context.Background(), dir, "", scope,
			func(m FileMeta) { metas = append(metas, m); trackedPaths[m.Path] = true },
			func(string) {},
			func(string) {},
		); err != nil {
			t.Fatal(err)
		}
	}
	walked, gitignore, boundaries, _, err := Walk(context.Background(), dir, WalkCache{})
	if err != nil {
		t.Fatal(err)
	}
	FilterWalked(dir, walked, trackedPaths, scope, gitignore, boundaries, func(m FileMeta) { metas = append(metas, m) }, func(string) {})
	return metas
}

// mustPattern parses raw as a gitignore-syntax Pattern (pattern.go),
// failing the test if it does not parse — every literal this test suite
// passes is meant to be valid, so a parse failure here is a test bug,
// not a case to tolerate silently the way ParsePattern's own caller
// (loadGitignore, index.LoadExclude) tolerates a genuinely unparseable
// line.
func mustPattern(t *testing.T, raw string) Pattern {
	t.Helper()
	p, ok := ParsePattern(raw)
	if !ok {
		t.Fatalf("ParsePattern(%q) = not ok, want a valid pattern", raw)
	}
	return p
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
	metas := scanAll(t, dir, Scope{Exclude: []Pattern{mustPattern(t, "pkg/")}})
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
	files, _, _, _, err := Walk(context.Background(), dir, WalkCache{})
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

	files, _, _, cache, err := Walk(context.Background(), dir, WalkCache{})
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
	files, _, _, _, err := Walk(context.Background(), dir, prev)
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
	files, _, _, _, err := Walk(context.Background(), dir, prev)
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
	FilterWalked(dir, []string{"a.go", "b.go"}, map[string]bool{"a.go": true}, Scope{}, nil, nil,
		func(m FileMeta) { got = append(got, m) },
		func(string) {},
	)
	if len(got) != 1 || got[0].Path != "b.go" {
		t.Errorf("FilterWalked = %v, want only b.go (a.go is in skip)", got)
	}
}

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestWalk_HonorsGitignore_NoGitRoot (directive 022 item 1): the gap
// named precisely against directive 021's own report — a root with no
// .git now produces an index (021), but nothing read .gitignore, so a
// Rust target/ or similar build-output tree would still be walked into
// in full. A bare, no-slash directory pattern prunes the directory
// outright (never gains a WalkCache entry, the same proof style
// TestWalk_PrunesDefaultExcludeDirsAndGit uses); a file-shaped pattern
// is not pruned during the walk (only a directory can be) but is
// still dropped by FilterWalked afterward.
func TestWalk_HonorsGitignore_NoGitRoot(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".gitignore", "target/\n*.log\n")
	writeFile(t, dir, "src/main.go", "package main\n")
	writeFile(t, dir, "target/debug/build/output.o", "x")
	writeFile(t, dir, "build.log", "x")

	files, gitignore, _, cache, err := Walk(context.Background(), dir, WalkCache{})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasPrefix(f, "target/") {
			t.Errorf("expected target/ to be pruned by .gitignore, but Walk returned %s", f)
		}
	}
	for _, pruned := range []string{"target", "target/debug", "target/debug/build"} {
		if _, ok := cache.Dirs[pruned]; ok {
			t.Errorf("expected %s to never be visited (gitignore-pruned), but it has a WalkCache entry", pruned)
		}
	}

	var got []FileMeta
	FilterWalked(dir, files, nil, Scope{}, gitignore, nil, func(m FileMeta) { got = append(got, m) }, func(string) {})
	byPath := make(map[string]bool, len(got))
	for _, m := range got {
		byPath[m.Path] = true
	}
	if !byPath["src/main.go"] {
		t.Error("expected src/main.go to survive filtering")
	}
	if byPath["build.log"] {
		t.Error("expected build.log to be excluded by the *.log gitignore pattern")
	}
}

// TestWalk_GitignoreScopedToOwnSubtree (directive 022 item 1: "nested
// .gitignore files apply to their own subtree"): a rule declared in
// pkg/a/.gitignore must not reach pkg/b, even though both share the
// same bare pattern name.
func TestWalk_GitignoreScopedToOwnSubtree(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "pkg/a/.gitignore", "gen/\n")
	writeFile(t, dir, "pkg/a/gen/x.go", "x")
	writeFile(t, dir, "pkg/b/gen/keep.go", "x")

	files, _, _, _, err := Walk(context.Background(), dir, WalkCache{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, f := range files {
		got[f] = true
	}
	if got["pkg/a/gen/x.go"] {
		t.Error("expected pkg/a/gen/ to be pruned by pkg/a/.gitignore")
	}
	if !got["pkg/b/gen/keep.go"] {
		t.Error("expected pkg/b/gen/ to be unaffected by pkg/a's own .gitignore")
	}
}

// TestWalk_GitignoreNegationCannotReachIntoPrunedDir (directive 022
// item 1/2): matches real git's own documented and empirically
// verified behaviour (`git check-ignore -v` against the same fixture:
// a file under an ignored directory stays ignored regardless of its
// own negation) — pruning by a .gitignore-sourced directory rule is
// safe precisely because negation can never undo it, in real git or
// here.
func TestWalk_GitignoreNegationCannotReachIntoPrunedDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".gitignore", "build/\n!build/keep.txt\n")
	writeFile(t, dir, "build/keep.txt", "x")
	writeFile(t, dir, "build/other.txt", "x")

	files, _, _, _, err := Walk(context.Background(), dir, WalkCache{})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasPrefix(f, "build/") {
			t.Errorf("expected build/ to stay pruned despite the negation (matches real git), got %s", f)
		}
	}
}

// TestWalk_PrunesDefaultExcludeDirs_AtAnyDepth (directive 022): pruning
// is no longer restricted to the scan root — a nested vendor/ (a
// second Go module inside a monorepo, say) is pruned exactly like a
// root-level one, matching Pattern.excludes' own extended "any depth"
// semantics for a bare directory-shaped pattern.
func TestWalk_PrunesDefaultExcludeDirs_AtAnyDepth(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "modules/sub/vendor/dep/dep.go", "x")
	writeFile(t, dir, "modules/sub/keep.go", "x")

	files, _, _, cache, err := Walk(context.Background(), dir, WalkCache{})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.Contains(f, "vendor/") {
			t.Errorf("expected the nested vendor/ to be pruned, got %s", f)
		}
	}
	if _, ok := cache.Dirs["modules/sub/vendor"]; ok {
		t.Error("expected modules/sub/vendor to never be visited")
	}
}

// submoduleRepo builds an outer git repository containing a real
// submodule — a fixture for directive 022 item 3's own questions,
// answered with actual `git` output rather than reasoning about it.
func submoduleRepo(t *testing.T) string {
	t.Helper()
	inner := t.TempDir()
	outer := t.TempDir()
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	run := func(dir string, args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	run(inner, "init", "-q")
	writeFile(t, inner, "inner.go", "package inner\n")
	writeFile(t, inner, "go.mod", "module inner\ngo 1.21\n")
	writeFile(t, inner, ".gitignore", "ignored.go\n")
	run(inner, "add", "inner.go", "go.mod", ".gitignore")
	writeFile(t, inner, "ignored.go", "package inner\n") // left untracked, gitignored
	run(inner, "commit", "-q", "-m", "init")

	run(outer, "init", "-q")
	writeFile(t, outer, "outer.go", "package outer\n")
	writeFile(t, outer, ".gitignore", "leaked.go\n")
	run(outer, "add", "outer.go", ".gitignore")
	run(outer, "commit", "-q", "-m", "init")
	run(outer, "-c", "protocol.file.allow=always", "submodule", "add", inner, "sub")
	run(outer, "add", ".gitmodules", "sub")
	run(outer, "commit", "-q", "-m", "add submodule")
	// Untracked, matching the outer .gitignore's own "leaked.go" pattern
	// only — a submodule is a gitignore boundary (directive 025), so this
	// must survive filtering even though the outer .gitignore would
	// otherwise exclude it at any depth.
	writeFile(t, outer, "sub/leaked.go", "package inner\n")

	dir, err := Root(outer)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestStreamTracked_SkipsSubmoduleGitlink (directive 022 item 3,
// answering "what does the tracked half do with that entry today":
// before 022, nothing skipped it, and the gitlink path was treated as an
// ordinary tracked file — the entry is recognised by its mode (160000),
// left out of onMeta and onMarker, and (directive 023 item 3: "a
// submodule is enumerated by git, inside itself") reported to onGitlink
// instead of simply discarded, so a caller can recurse into it.
func TestStreamTracked_SkipsSubmoduleGitlink(t *testing.T) {
	dir := submoduleRepo(t)
	var metas []FileMeta
	var gitlinks []string
	if err := StreamTracked(context.Background(), dir, "", Scope{},
		func(m FileMeta) { metas = append(metas, m) }, func(string) {},
		func(p string) { gitlinks = append(gitlinks, p) },
	); err != nil {
		t.Fatal(err)
	}
	if len(gitlinks) != 1 || gitlinks[0] != "sub" {
		t.Errorf("gitlinks = %v, want [sub] (the submodule entry, reported to the caller instead of skipped in silence)", gitlinks)
	}
	for _, m := range metas {
		if m.Path == "sub" {
			t.Errorf("expected the submodule gitlink entry not to be treated as a tracked file, got %+v", m)
		}
	}
}

// TestWalk_DescendsIntoSubmodule_PrunesItsGitFile_HonorsItsGitignore
// answers item 3's other questions: yes, Walk descends into a
// submodule's working tree (nothing distinguishes it from an ordinary
// directory); its own gitdir pointer file (sub/.git — a regular file,
// not a directory: directive 022 item 3, confirmed via `stat`) is
// never indexed; and its own .gitignore applies to its own subtree,
// the same as any other directory's.
//
// Extended, directive 025: a submodule is also a gitignore boundary —
// the OUTER .gitignore's own "leaked.go" pattern (submoduleRepo's own
// fixture) must not reach sub/leaked.go, even though a bare pattern
// with no slash ordinarily matches at any depth. Before this directive
// it did: Walk carried the outer .gitignore's rule down into the
// submodule with no boundary to stop it.
func TestWalk_DescendsIntoSubmodule_PrunesItsGitFile_HonorsItsGitignore(t *testing.T) {
	dir := submoduleRepo(t)
	files, gitignore, boundaries, _, err := Walk(context.Background(), dir, WalkCache{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, f := range files {
		got[f] = true
	}
	if !got["sub/go.mod"] {
		t.Error("expected Walk to descend into the submodule and find sub/go.mod")
	}
	if got["sub/.git"] {
		t.Error("expected the submodule's own gitdir pointer file never to be indexed")
	}
	if len(boundaries) != 1 || boundaries[0] != "sub" {
		t.Errorf("boundaries = %v, want [sub]", boundaries)
	}

	var filtered []FileMeta
	FilterWalked(dir, files, nil, Scope{}, gitignore, boundaries, func(m FileMeta) { filtered = append(filtered, m) }, func(string) {})
	byPath := make(map[string]bool, len(filtered))
	for _, m := range filtered {
		byPath[m.Path] = true
	}
	if byPath["sub/ignored.go"] {
		t.Error("expected the submodule's own .gitignore to exclude sub/ignored.go")
	}
	if !byPath["sub/leaked.go"] {
		t.Error("expected sub/leaked.go to survive: a submodule is a gitignore boundary, so the outer .gitignore's leaked.go pattern must not reach inside it")
	}
}
