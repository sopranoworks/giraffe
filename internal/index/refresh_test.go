package index

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"giraffe/internal/entry"
	"giraffe/internal/scanner"
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
	run("init", "-q")
	dir, err := scanner.Root(dir)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitCommit(t *testing.T, root string, paths ...string) {
	t.Helper()
	cmds := [][]string{{"commit", "-q", "-m", "update"}}
	if len(paths) > 0 {
		cmds = append([][]string{append([]string{"add"}, paths...)}, cmds...)
	}
	for _, args := range cmds {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

func scope() scanner.Scope {
	return scanner.Scope{}
}

// mustRefresh calls Refresh and fails the test on error — most tests in
// this file only care about the returned store's contents, not error
// handling, which has its own dedicated tests where needed.
func mustRefresh(t *testing.T, root string) *Store {
	t.Helper()
	s, _, err := Refresh(context.Background(), root, scope(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// closeStore closes s and fails the test if that errors — tests always
// want a clean close, since a leaked store handle would otherwise mask
// a real bug (e.g. a build that failed to commit).
func closeStore(t *testing.T, s *Store) {
	t.Helper()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

// allEntries materializes every entry in s — acceptable in tests, which
// care about correctness, not about avoiding exactly the materialization
// §30 exists to avoid in the real command.
func allEntries(s *Store) []entry.Entry {
	var out []entry.Entry
	for e := range s.All() {
		out = append(out, e)
	}
	return out
}

// declarations filters out the file entries §22 item 4 adds — one per
// scanned file, Kind "file" — so tests written against declaration
// counts before that item landed keep checking what they always checked.
func declarations(entries []entry.Entry) []entry.Entry {
	var out []entry.Entry
	for _, e := range entries {
		if e.Kind != entry.KindFile {
			out = append(out, e)
		}
	}
	return out
}

func TestRefresh_FirstRun_BuildsFromScratch(t *testing.T) {
	root := gitRepo(t)
	writeFile(t, root, "retry.go", "package retry\n\n// Retry retries.\nfunc Retry() {}\n")
	gitCommit(t, root, "retry.go")

	s, stats, err := Refresh(context.Background(), root, scope(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(t, s)
	decls := declarations(allEntries(s))
	if len(decls) != 1 || decls[0].Name != "Retry" {
		t.Fatalf("declarations = %#v", decls)
	}
	if stats.Reparsed != 1 || stats.Reused != 0 {
		t.Fatalf("stats = %#v, want Reparsed=1 Reused=0", stats)
	}
}

func TestRefresh_SecondRun_UnchangedFileIsReused(t *testing.T) {
	root := gitRepo(t)
	writeFile(t, root, "retry.go", "package retry\n\nfunc Retry() {}\n")
	gitCommit(t, root, "retry.go")

	closeStore(t, mustRefresh(t, root))

	s, stats, err := Refresh(context.Background(), root, scope(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(t, s)
	entries := allEntries(s)
	if len(declarations(entries)) != 1 {
		t.Fatalf("declarations = %#v", declarations(entries))
	}
	if stats.Reused != 1 || stats.Reparsed != 0 {
		t.Fatalf("stats = %#v, want Reused=1 Reparsed=0 on an unchanged second run", stats)
	}
}

func TestRefresh_StagedEditIsReparsed(t *testing.T) {
	root := gitRepo(t)
	writeFile(t, root, "retry.go", "package retry\n\nfunc Retry() {}\n")
	gitCommit(t, root, "retry.go")
	closeStore(t, mustRefresh(t, root))

	writeFile(t, root, "retry.go", "package retry\n\nfunc Retry() {}\n\nfunc Backoff() {}\n")
	gitCommit(t, root, "retry.go") // staged, so its blob hash changes

	s, stats, err := Refresh(context.Background(), root, scope(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(t, s)
	entries := allEntries(s)
	if len(declarations(entries)) != 2 {
		t.Fatalf("declarations = %#v, want Retry and Backoff", declarations(entries))
	}
	if stats.Reparsed != 1 {
		t.Fatalf("stats = %#v, want Reparsed=1 for the changed file", stats)
	}
}

// TestRefresh_FileItselfIsASearchTarget (§22 item 4): a file gets its
// own entry — Kind "file", Name without the extension — regardless of
// what declarations it has, so it can be found by name even when every
// declaration inside it is unexported and hidden outside its own
// project (§21 C-6's readLines/checkNameCollisions case).
func TestRefresh_FileItselfIsASearchTarget(t *testing.T) {
	root := gitRepo(t)
	writeFile(t, root, "internal/retry/retry.go", "package retry\n\nfunc unexported() {}\n")
	gitCommit(t, root, "internal/retry/retry.go")

	s := mustRefresh(t, root)
	defer closeStore(t, s)
	entries := allEntries(s)
	var found *entry.Entry
	for i := range entries {
		if entries[i].Kind == entry.KindFile {
			found = &entries[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("expected a file entry for internal/retry/retry.go, got %#v", entries)
	}
	if found.Name != "retry" {
		t.Errorf("file entry Name = %q, want %q (extension stripped)", found.Name, "retry")
	}
	if found.Path != "internal/retry/retry.go" {
		t.Errorf("file entry Path = %q", found.Path)
	}
	if !found.Exported {
		t.Error("expected a file entry to always be Exported (not a language-visibility concept)")
	}

	// Not carried forward by the extraction cache: a file entry is
	// recomputed fresh from the scan on every call (Refresh's own top
	// loop) and never part of what a file's EntryIDs names — see
	// Store.build's own comment for why: carrying it forward would
	// double it up with the freshly recomputed one on the next rebuild.
	rec, ok := s.fileRecord("internal/retry/retry.go")
	if !ok {
		t.Fatal("expected a files-bucket record for internal/retry/retry.go")
	}
	for _, e := range s.entriesByID(rec.EntryIDs) {
		if e.Kind == entry.KindFile {
			t.Fatalf("a file entry must never be part of the extraction cache's carried-forward set, found %#v", e)
		}
	}
}

// TestRefresh_NoExtractorFileGetsANameOnlyEntry (§24 C-18/C-19, §25
// C-24): a file with no registered extractor still gets a file entry —
// that is the property that justifies file-name indexing needing no
// parser — but no declarations, no doc, no scope, no lang (absent, not
// a placeholder), and no read or cache write of its own, since an
// unknown-language entry needs nothing an extractor computes. It still
// carries Ext: a fact about the file itself, independent of whether
// its language is known.
func TestRefresh_NoExtractorFileGetsANameOnlyEntry(t *testing.T) {
	root := gitRepo(t)
	writeFile(t, root, "retry.go", "package retry\n\nfunc Retry() {}\n")
	writeFile(t, root, "README.md", "# giraffe\n")
	gitCommit(t, root, "retry.go", "README.md")

	s, stats, err := Refresh(context.Background(), root, scope(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(t, s)
	entries := allEntries(s)
	if stats.Reparsed != 1 {
		t.Fatalf("stats = %#v, want Reparsed=1 (only retry.go has an extractor)", stats)
	}

	var found *entry.Entry
	for i := range entries {
		if entries[i].Path == "README.md" {
			found = &entries[i]
		}
	}
	if found == nil {
		t.Fatalf("expected a file entry for README.md, got %#v", entries)
	}
	if found.Kind != entry.KindFile {
		t.Errorf("README.md entry Kind = %q, want %q", found.Kind, entry.KindFile)
	}
	if found.Lang != "" {
		t.Errorf("README.md entry Lang = %q, want absent (no registered extractor, no assignment)", found.Lang)
	}
	if found.Ext != "md" {
		t.Errorf("README.md entry Ext = %q, want %q", found.Ext, "md")
	}
	if found.Doc != "" || found.Scope != "" {
		t.Errorf("expected a no-language entry to carry no doc or scope, got %#v", found)
	}

	if _, cached := s.fileRecord("README.md"); cached {
		t.Fatal("a no-language file must never be stat'd or cached by the extraction cache — it is never read or extracted")
	}
}

// TestRefresh_OperatorAssignmentFillsLangWithoutAddingAnExtractor
// (§25 C-26): an extension-to-language assignment gives an
// unextractable file a lang value, but does not make it extractable —
// it is still never stat'd, cached, or read.
func TestRefresh_OperatorAssignmentFillsLangWithoutAddingAnExtractor(t *testing.T) {
	root := gitRepo(t)
	writeFile(t, root, "widget.inc", "int widget();\n")
	gitCommit(t, root, "widget.inc")

	assignments := map[string]string{".inc": "c"}
	s, stats, err := Refresh(context.Background(), root, scope(), assignments)
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(t, s)
	entries := allEntries(s)
	if stats.Reparsed != 0 {
		t.Fatalf("stats = %#v, want Reparsed=0 — an assignment does not add an extractor", stats)
	}

	var found *entry.Entry
	for i := range entries {
		if entries[i].Path == "widget.inc" {
			found = &entries[i]
		}
	}
	if found == nil {
		t.Fatalf("expected a file entry for widget.inc, got %#v", entries)
	}
	if found.Lang != "c" {
		t.Errorf("widget.inc entry Lang = %q, want %q (operator assignment)", found.Lang, "c")
	}
	if found.Ext != "inc" {
		t.Errorf("widget.inc entry Ext = %q, want %q", found.Ext, "inc")
	}

	if _, cached := s.fileRecord("widget.inc"); cached {
		t.Fatal("an assigned-but-unextractable file must never be stat'd or cached by the extraction cache")
	}
}

// TestRefresh_ExtractedEntriesCarryTheirLanguage (§24 C-20, §25 C-24):
// both the file entry and its declarations carry the same lang and ext
// values, whether synthesised fresh or served from the cache — the
// cache-persistence bug directive 006 found and fixed for Lang applies
// equally to Ext, added in the same 12-field cache line.
func TestRefresh_ExtractedEntriesCarryTheirLanguage(t *testing.T) {
	root := gitRepo(t)
	writeFile(t, root, "retry.go", "package retry\n\nfunc Retry() {}\n")
	gitCommit(t, root, "retry.go")

	check := func(entries []entry.Entry) {
		t.Helper()
		for _, e := range entries {
			if e.Lang != "go" {
				t.Errorf("entry %#v: Lang = %q, want %q", e, e.Lang, "go")
			}
			if e.Ext != "go" {
				t.Errorf("entry %#v: Ext = %q, want %q", e, e.Ext, "go")
			}
		}
	}
	s1 := mustRefresh(t, root)
	check(allEntries(s1)) // freshly extracted
	closeStore(t, s1)

	s2, stats2, err := Refresh(context.Background(), root, scope(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(t, s2)
	if stats2.Reparsed != 0 {
		t.Fatalf("expected the second run to reuse the cache, got %#v", stats2)
	}
	check(allEntries(s2)) // served from cache
}

func TestRefresh_ZeroEntryFileIsReusedNotReparsed(t *testing.T) {
	// §21 C-6 indexes unexported declarations too, so a file needs zero
	// top-level declarations at all (not merely zero exported ones) to
	// exercise the presence-line case.
	root := gitRepo(t)
	writeFile(t, root, "priv.go", "package p\n") // no declarations at all
	gitCommit(t, root, "priv.go")

	s1, stats1, err := Refresh(context.Background(), root, scope(), nil)
	if err != nil {
		t.Fatal(err)
	}
	closeStore(t, s1)
	if stats1.Reparsed != 1 {
		t.Fatalf("first run: stats = %#v, want Reparsed=1", stats1)
	}

	s2, stats, err := Refresh(context.Background(), root, scope(), nil)
	if err != nil {
		t.Fatal(err)
	}
	closeStore(t, s2)
	if stats.Reparsed != 0 || stats.Reused != 1 {
		t.Fatalf("second run: stats = %#v, want Reused=1 Reparsed=0 — a file with no "+
			"declarations must still be recognised as unchanged, not re-read every run", stats)
	}
}

// TestRefresh_UntrackedFileIsCachedLikeAnyOther (§23 C-14): before this
// directive, an untracked file was always re-extracted and never
// written to the index, because the index was committed and "not yet
// shared with anyone" was a real distinction for something about to be
// shared. The cache is local now — nothing in it is shared with anyone,
// tracked or not — so an untracked, work-in-progress file gets the same
// stat-based freshness caching a tracked file does.
func TestRefresh_UntrackedFileIsCachedLikeAnyOther(t *testing.T) {
	root := gitRepo(t)
	writeFile(t, root, "tracked.go", "package p\n\nfunc Tracked() {}\n")
	gitCommit(t, root, "tracked.go")
	writeFile(t, root, "wip.go", "package p\n\nfunc WorkInProgress() {}\n") // never git add'ed

	s, stats, err := Refresh(context.Background(), root, scope(), nil)
	if err != nil {
		t.Fatal(err)
	}
	entries := allEntries(s)
	if len(declarations(entries)) != 2 {
		t.Fatalf("declarations = %#v, want both Tracked and WorkInProgress", declarations(entries))
	}
	if stats.Reparsed != 2 {
		t.Fatalf("stats = %#v, want Reparsed=2 on the first run", stats)
	}

	if _, cached := s.fileRecord("wip.go"); !cached {
		t.Fatal("expected the untracked file to be cached like any other")
	}
	if _, cached := s.fileRecord("tracked.go"); !cached {
		t.Fatal("tracked file's entries should be cached")
	}
	closeStore(t, s)

	// Second run, nothing changed: the untracked file must be reused
	// from cache, not re-read every time simply for being untracked.
	s2, stats2, err := Refresh(context.Background(), root, scope(), nil)
	if err != nil {
		t.Fatal(err)
	}
	closeStore(t, s2)
	if stats2.Reparsed != 0 || stats2.Reused != 2 {
		t.Fatalf("stats = %#v, want Reused=2 Reparsed=0 — an unchanged untracked file must be reused too", stats2)
	}
}

func TestRefresh_UnstagedEditIsReparsed(t *testing.T) {
	root := gitRepo(t)
	writeFile(t, root, "retry.go", "package retry\n\nfunc Retry() {}\n")
	gitCommit(t, root, "retry.go")
	closeStore(t, mustRefresh(t, root))

	// Edited but never `git add`ed. Under git-blob-hash freshness (001)
	// this file's stale hash meant the edit was invisible (§20 C-2, the
	// defect stat-based freshness exists to fix).
	writeFile(t, root, "retry.go", "package retry\n\nfunc Retry() {}\n\nfunc Backoff() {}\n")

	s, stats, err := Refresh(context.Background(), root, scope(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(t, s)
	if len(declarations(allEntries(s))) != 2 {
		t.Fatalf("declarations = %#v, want Retry and Backoff even though the edit was never staged", declarations(allEntries(s)))
	}
	if stats.Reparsed != 1 {
		t.Fatalf("stats = %#v, want Reparsed=1 for the unstaged edit", stats)
	}
}

func TestRefresh_NestedMarkerAssignsDistinctProjects(t *testing.T) {
	root := gitRepo(t)
	writeFile(t, root, "go.mod", "module outer\n")
	writeFile(t, root, "main.go", "package main\n\nfunc Main() {}\n")
	writeFile(t, root, "services/api/go.mod", "module inner\n")
	writeFile(t, root, "services/api/handler.go", "package api\n\nfunc Handle() {}\n")
	gitCommit(t, root, "go.mod", "main.go", "services/api/go.mod", "services/api/handler.go")

	s := mustRefresh(t, root)
	defer closeStore(t, s)
	projects := make(map[string]string)
	for e := range s.All() {
		projects[e.Name] = e.Project
	}
	rootBase := filepath.Base(root)
	if projects["Main"] != rootBase {
		t.Errorf("Main's project = %q, want the repo root's own basename %q", projects["Main"], rootBase)
	}
	if projects["Handle"] != "api" {
		t.Errorf("Handle's project = %q, want %q (nearest enclosing marker dir)", projects["Handle"], "api")
	}
}

func TestRefresh_NestedMarkerCollisionIsHardError(t *testing.T) {
	// §21 C-8: before this directive, two nested marker directories
	// sharing a basename merged silently under one project: value — the
	// same failure the cross-root case (main.checkNameCollisions) already
	// treated as a hard error. Two different services/*/util trees is a
	// realistic way to hit this by accident.
	root := gitRepo(t)
	writeFile(t, root, "go.mod", "module outer\n")
	writeFile(t, root, "services/a/util/go.mod", "module a-util\n")
	writeFile(t, root, "services/a/util/helper.go", "package util\n\nfunc A() {}\n")
	writeFile(t, root, "services/b/util/go.mod", "module b-util\n")
	writeFile(t, root, "services/b/util/helper.go", "package util\n\nfunc B() {}\n")
	gitCommit(t, root, "go.mod", "services/a/util/go.mod", "services/a/util/helper.go",
		"services/b/util/go.mod", "services/b/util/helper.go")

	_, _, err := Refresh(context.Background(), root, scope(), nil)
	if err == nil {
		t.Fatal("expected a hard error for two nested marker dirs sharing the project name \"util\"")
	}
	if !strings.Contains(err.Error(), "util") {
		t.Errorf("error = %v, want it to name the colliding project %q", err, "util")
	}
	if !strings.Contains(err.Error(), "services/a/util") || !strings.Contains(err.Error(), "services/b/util") {
		t.Errorf("error = %v, want it to name both paths", err)
	}
}

func TestRefresh_DeletedFileDropsItsEntries(t *testing.T) {
	root := gitRepo(t)
	writeFile(t, root, "a.go", "package p\n\nfunc A() {}\n")
	writeFile(t, root, "b.go", "package p\n\nfunc B() {}\n")
	gitCommit(t, root, "a.go", "b.go")
	closeStore(t, mustRefresh(t, root))

	cmd := exec.Command("git", "rm", "-q", "b.go")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git rm: %v\n%s", err, out)
	}
	gitCommit(t, root)

	s := mustRefresh(t, root)
	defer closeStore(t, s)
	for e := range s.All() {
		if e.Name == "B" {
			t.Fatal("expected B's entry to be dropped after b.go was deleted")
		}
	}
}

func writeExclude(t *testing.T, root, content string) {
	t.Helper()
	full := filepath.Join(root, Dir, "exclude")
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestLoadExclude_SeparatesAssignmentsFromPatterns (§25 C-26): a line
// shaped "ext:<x> lang:<y>" is an operator language assignment, not an
// exclusion pattern — the two live in the same file (§25 C-27:
// exclude is the portable, committable one) but are returned
// separately, since scanner.Scope only ever wants patterns.
func TestLoadExclude_SeparatesAssignmentsFromPatterns(t *testing.T) {
	root := t.TempDir()
	writeExclude(t, root, "vendor/\next:inc lang:c\n*.pb.go\nlang:cpp ext:hpp\n")

	patterns, assignments, err := LoadExclude(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(patterns) != 2 || patterns[0] != "vendor/" || patterns[1] != "*.pb.go" {
		t.Fatalf("patterns = %#v, want [vendor/ *.pb.go]", patterns)
	}
	if assignments[".inc"] != "c" {
		t.Errorf("assignments[.inc] = %q, want %q", assignments[".inc"], "c")
	}
	if assignments[".hpp"] != "cpp" {
		t.Errorf("assignments[.hpp] = %q, want %q (either token order)", assignments[".hpp"], "cpp")
	}
}

// TestLoadExclude_MissingFileYieldsEmptyAssignments mirrors the
// existing "no exclude file" case: nothing configured is not an error.
func TestLoadExclude_MissingFileYieldsEmptyAssignments(t *testing.T) {
	root := t.TempDir()
	patterns, assignments, err := LoadExclude(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(patterns) != 0 || len(assignments) != 0 {
		t.Fatalf("patterns = %#v, assignments = %#v, want both empty", patterns, assignments)
	}
}

// TestParseAssignment_RequiresExactlyBothTokens: anything short of a
// well-formed two-token "ext:_ lang:_" line is left for the exclusion
// pattern path, not partially interpreted as an assignment.
func TestParseAssignment_RequiresExactlyBothTokens(t *testing.T) {
	cases := []string{
		"ext:inc",                 // lang: missing
		"lang:c",                  // ext: missing
		"ext: lang:c",             // empty ext value
		"ext:inc lang:",           // empty lang value
		"ext:inc lang:c extra",    // three tokens
		"vendor/",                 // an ordinary pattern
		"*.pb.go",                 // an ordinary pattern
		"ext:inc lang:c lang:c++", // duplicate key, still not exactly one of each
	}
	for _, c := range cases {
		if _, _, ok := parseAssignment(c); ok {
			t.Errorf("parseAssignment(%q) = ok, want not-an-assignment", c)
		}
	}
}

// TestLoadExclude_MalformedAssignmentIsAnError (§26 C-29): a line
// beginning "ext:" or "lang:" that fails to parse as a well-formed
// assignment is a hard error naming the file and the line, not a
// silently-inert exclusion pattern. This is self-caused: the assignment
// syntax did not exist before 007, so a typo in it could not either.
func TestLoadExclude_MalformedAssignmentIsAnError(t *testing.T) {
	cases := []struct {
		content  string
		wantLine int
	}{
		{"ext:inc\n", 1},              // lang: missing
		{"lang:c\n", 1},               // ext: missing
		{"vendor/\nlang:cpp\n", 2},    // error on the second line specifically
		{"ext:inc lang:c extra\n", 1}, // three tokens
	}
	for _, c := range cases {
		root := t.TempDir()
		writeExclude(t, root, c.content)
		_, _, err := LoadExclude(root)
		if err == nil {
			t.Fatalf("LoadExclude(%q) = nil error, want an error for a malformed assignment", c.content)
		}
		if !strings.Contains(err.Error(), "exclude") {
			t.Errorf("error %v does not name the exclude file", err)
		}
		wantLineStr := fmt.Sprintf(":%d:", c.wantLine)
		if !strings.Contains(err.Error(), wantLineStr) {
			t.Errorf("error %v does not name line %d", err, c.wantLine)
		}
	}
}

// TestLoadExclude_OrdinaryPatternsAreNotMistakenForAssignments confirms
// the error path is narrow: a normal pattern, even one that happens to
// contain a colon, is unaffected.
func TestLoadExclude_OrdinaryPatternsAreNotMistakenForAssignments(t *testing.T) {
	root := t.TempDir()
	writeExclude(t, root, "vendor/\n*.pb.go\nsome:weird:path\n")
	patterns, _, err := LoadExclude(root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(patterns) != 3 {
		t.Fatalf("patterns = %#v, want 3 ordinary patterns", patterns)
	}
}

// TestRefresh_FileEntryCarriesSizeAndDeclCount (directive 009 item 3): a
// file's KindFile entry reports its byte size and the number of
// declarations extracted from it — a fact about the file, computed once
// over the whole entry set (declarations are not known until extraction
// finishes, after fileEntry already ran), not per declaration.
func TestRefresh_FileEntryCarriesSizeAndDeclCount(t *testing.T) {
	root := gitRepo(t)
	content := "package p\n\nfunc A() {}\nfunc B() {}\n"
	writeFile(t, root, "two.go", content)
	writeFile(t, root, "README.md", "# hello\n")
	gitCommit(t, root, "two.go", "README.md")

	s := mustRefresh(t, root)
	defer closeStore(t, s)
	entries := allEntries(s)

	var goFile, mdFile *entry.Entry
	for i, e := range entries {
		if e.Kind != entry.KindFile {
			continue
		}
		switch e.Path {
		case "two.go":
			goFile = &entries[i]
		case "README.md":
			mdFile = &entries[i]
		}
	}
	if goFile == nil || mdFile == nil {
		t.Fatalf("expected file entries for two.go and README.md, got %#v", entries)
	}
	if goFile.DeclCount != 2 {
		t.Errorf("two.go DeclCount = %d, want 2 (A and B)", goFile.DeclCount)
	}
	if goFile.Size != int64(len(content)) {
		t.Errorf("two.go Size = %d, want %d", goFile.Size, len(content))
	}
	if mdFile.DeclCount != 0 {
		t.Errorf("README.md DeclCount = %d, want 0 (no extractor)", mdFile.DeclCount)
	}
	if mdFile.Size != int64(len("# hello\n")) {
		t.Errorf("README.md Size = %d, want %d", mdFile.Size, len("# hello\n"))
	}
}
