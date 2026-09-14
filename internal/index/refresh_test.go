package index

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"giraffe/internal/entry"
	"giraffe/internal/lang"
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

// scope mirrors main.go's own scope construction (directive 017 §3:
// scanner does not know go.mod on its own any more — a test calling
// Refresh directly needs the same lang.RegisteredMarkers() wiring main
// gives it, or a nested go.mod stops being recognised as a root
// marker at all).
func scope() scanner.Scope {
	return scanner.Scope{Markers: lang.RegisteredMarkers()}
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
	if stats.Dispatched != 1 || stats.Reused != 0 {
		t.Fatalf("stats = %#v, want Dispatched=1 Reused=0", stats)
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
	if stats.Reused != 1 || stats.Dispatched != 0 {
		t.Fatalf("stats = %#v, want Reused=1 Dispatched=0 on an unchanged second run", stats)
	}
}

func TestRefresh_StagedEditIsDispatched(t *testing.T) {
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
	if stats.Dispatched != 1 {
		t.Fatalf("stats = %#v, want Dispatched=1 for the changed file", stats)
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

	// Directive 013: the files bucket now names every entry a path
	// owns, its own file entry included — that is what lets a
	// differential update remove or replace exactly what one file owns
	// without touching any other (see store.go's commit). A file whose
	// content never changes still has its declarations carried forward
	// unread; only the file entry itself is ever rewritten on its own
	// (e.g. when a marker moves its Project), which is why the mapping
	// needs to name it too.
	rec, ok := s.fileRecord("internal/retry/retry.go")
	if !ok {
		t.Fatal("expected a files-bucket record for internal/retry/retry.go")
	}
	foundFileEntry := false
	for _, e := range s.entriesByID(rec.EntryIDs) {
		if e.Kind == entry.KindFile {
			foundFileEntry = true
		}
	}
	if !foundFileEntry {
		t.Fatalf("expected internal/retry/retry.go's own file entry among its EntryIDs, got %#v", s.entriesByID(rec.EntryIDs))
	}
}

// TestRefresh_NoExtractorFileGetsANameOnlyEntry (§24 C-18/C-19, §25
// C-24): a file no registered module claims still gets a file entry —
// that is the property that justifies file-name indexing needing no
// parser — but no declarations, no doc, no scope, no lang (absent, not
// a placeholder). It still carries Ext: a fact about the file itself,
// independent of whether its language is known.
//
// It is dispatched through lang.Extract now (directive 016 §1): with
// no extension table left outside the language modules, the only way
// to learn that no module wants README.md is to hand it over and let
// the generic module answer — so Dispatched (files run through
// lang.Extract this call, directive 018 §4) counts it too, alongside
// retry.go. Its content is not actually read, though (directive 017
// §1): the generic module never calls File.Content, so the read
// scanner.Open defers never happens for this file.
//
// It gets a files-bucket record (directive 013): not because anything
// meaningful was extracted from it, but because the differential store
// needs to know, on a later call, whether this path is still there at
// all — the record's freshness fields are still stat-compared like any
// other file's (§20 C-2), only its declarations are always empty.
// TestRefresh_LineAndLineTextSurviveAStoreRoundTrip (§31.4): a
// declaration's Line and LineText are new store.go fields
// (encodeEntry/decodeEntry) — this exercises the real write-then-read
// path Refresh uses, not just the in-package extractor test, so a
// field-count mismatch between encode and decode would fail here even
// if it left every other field looking fine.
func TestRefresh_LineAndLineTextSurviveAStoreRoundTrip(t *testing.T) {
	root := gitRepo(t)
	writeFile(t, root, "retry.go", "package retry\n\n// Retry retries.\nfunc Retry() {}\n")
	gitCommit(t, root, "retry.go")

	s := mustRefresh(t, root)
	defer closeStore(t, s)
	for _, e := range allEntries(s) {
		if e.Name != "Retry" {
			continue
		}
		if e.Line != 4 {
			t.Errorf("Line = %d, want 4", e.Line)
		}
		if e.LineText != "func Retry() {}" {
			t.Errorf("LineText = %q, want %q", e.LineText, "func Retry() {}")
		}
		return
	}
	t.Fatal("expected a Retry entry in the store")
}

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
	if stats.Dispatched != 2 {
		t.Fatalf("stats = %#v, want Dispatched=2 (retry.go and README.md are both handed to lang.Extract, §1)", stats)
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

	if _, known := s.fileRecord("README.md"); !known {
		t.Fatal("expected README.md to have a files-bucket record even with no extractor — needed to detect it later if it is deleted")
	}
}

// TestRefresh_OperatorAssignmentFillsLangWithoutAddingAnExtractor
// (§25 C-26): an extension-to-language assignment gives a file no
// module claims a lang value, but does not make its content mean
// anything — no module reads .inc's syntax, assigned or not. It still
// gets a files-bucket presence record (directive 013: same as any
// other no-module file — see TestRefresh_NoExtractorFileGetsANameOnlyEntry)
// and, since directive 016 §1, its content is still read once (there is
// no extension table left to skip that with — see the same test) even
// though the assignment changes nothing about who reads it.
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
	if stats.Dispatched != 1 {
		t.Fatalf("stats = %#v, want Dispatched=1 — widget.inc is still handed to lang.Extract (§1), even though the assignment does not add a module", stats)
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

	if _, known := s.fileRecord("widget.inc"); !known {
		t.Fatal("expected widget.inc to have a files-bucket record even though an assignment does not make it extractable")
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
	if stats2.Dispatched != 0 {
		t.Fatalf("expected the second run to reuse the cache, got %#v", stats2)
	}
	check(allEntries(s2)) // served from cache
}

func TestRefresh_ZeroEntryFileIsReusedNotDispatched(t *testing.T) {
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
	if stats1.Dispatched != 1 {
		t.Fatalf("first run: stats = %#v, want Dispatched=1", stats1)
	}

	s2, stats, err := Refresh(context.Background(), root, scope(), nil)
	if err != nil {
		t.Fatal(err)
	}
	closeStore(t, s2)
	if stats.Dispatched != 0 || stats.Reused != 1 {
		t.Fatalf("second run: stats = %#v, want Reused=1 Dispatched=0 — a file with no "+
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
	if stats.Dispatched != 2 {
		t.Fatalf("stats = %#v, want Dispatched=2 on the first run", stats)
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
	if stats2.Dispatched != 0 || stats2.Reused != 2 {
		t.Fatalf("stats = %#v, want Reused=2 Dispatched=0 — an unchanged untracked file must be reused too", stats2)
	}
}

func TestRefresh_UnstagedEditIsDispatched(t *testing.T) {
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
	if stats.Dispatched != 1 {
		t.Fatalf("stats = %#v, want Dispatched=1 for the unstaged edit", stats)
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

// TestRefresh_KeysWritten_NothingChangedIsZero (directive 013): the fast
// path never opens a write transaction at all.
func TestRefresh_KeysWritten_NothingChangedIsZero(t *testing.T) {
	root := gitRepo(t)
	writeFile(t, root, "a.go", "package p\n\nfunc A() {}\n")
	writeFile(t, root, "b.go", "package p\n\nfunc B() {}\n")
	gitCommit(t, root, "a.go", "b.go")
	closeStore(t, mustRefresh(t, root))

	s, stats, err := Refresh(context.Background(), root, scope(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(t, s)
	if stats.KeysWritten != 0 {
		t.Errorf("KeysWritten = %d, want 0 when nothing changed", stats.KeysWritten)
	}
}

// TestRefresh_KeysWritten_OneChangedFileIsNotTheWholeCorpus (directive
// 013's own completion condition, checked directly rather than only by
// manual measurement): editing one file among several must write on the
// order of that one file's own entries and postings, never every key in
// the store.
func TestRefresh_KeysWritten_OneChangedFileIsNotTheWholeCorpus(t *testing.T) {
	root := gitRepo(t)
	names := []string{"a", "b", "c", "d", "e"}
	var paths []string
	for _, n := range names {
		p := n + ".go"
		writeFile(t, root, p, fmt.Sprintf("package p\n\nfunc %s() {}\n", strings.ToUpper(n)))
		paths = append(paths, p)
	}
	gitCommit(t, root, paths...)
	closeStore(t, mustRefresh(t, root))

	writeFile(t, root, "a.go", "package p\n\nfunc A() {}\nfunc A2() {}\n")

	s, stats, err := Refresh(context.Background(), root, scope(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(t, s)
	if stats.Dispatched != 1 {
		t.Fatalf("Dispatched = %d, want 1 (only a.go changed)", stats.Dispatched)
	}
	// a.go's own file entry, its two declarations, a's own files-bucket
	// record, and a handful of trigram grams — nowhere near the 5
	// files' worth of entries and postings a full rewrite would touch.
	if stats.KeysWritten == 0 || stats.KeysWritten > 20 {
		t.Errorf("KeysWritten = %d, want a small number on the order of one file's own entries and postings", stats.KeysWritten)
	}
}

// TestRefresh_DeletedUntrackedNonExtractorFileDropsItsEntry (directive
// 013's "before that": the files bucket must map every scanned file to
// its own entries well enough to remove one without touching others —
// including a file with no extractor, which previously had no files-
// bucket record at all and so could never be told apart from "still
// there").
func TestRefresh_DeletedUntrackedNonExtractorFileDropsItsEntry(t *testing.T) {
	root := gitRepo(t)
	writeFile(t, root, "tracked.go", "package p\n\nfunc Tracked() {}\n")
	gitCommit(t, root, "tracked.go")
	writeFile(t, root, "notes.md", "# notes\n") // untracked, no extractor
	closeStore(t, mustRefresh(t, root))

	s0 := mustRefresh(t, root)
	found := false
	for e := range s0.All() {
		if e.Path == "notes.md" {
			found = true
		}
	}
	closeStore(t, s0)
	if !found {
		t.Fatal("expected notes.md to be indexed before deletion")
	}

	if err := os.Remove(filepath.Join(root, "notes.md")); err != nil {
		t.Fatal(err)
	}

	s := mustRefresh(t, root)
	defer closeStore(t, s)
	for e := range s.All() {
		if e.Path == "notes.md" {
			t.Fatal("expected notes.md's entry to be dropped after it was deleted")
		}
	}
	if _, known := s.fileRecord("notes.md"); known {
		t.Fatal("expected notes.md's files-bucket record to be removed too")
	}
}

// TestRefresh_MarkerChangeUpdatesProjectWithoutReparsing (directive 013:
// the enumeration cache moving forces a file's own entry to be
// refreshed, since its Project may have changed, but must not force its
// unrelated declarations to be re-extracted — they are carried forward
// under fresh ids, not re-read).
func TestRefresh_MarkerChangeUpdatesProjectWithoutReparsing(t *testing.T) {
	root := gitRepo(t)
	writeFile(t, root, "go.mod", "module outer\n")
	writeFile(t, root, "main.go", "package main\n\nfunc Main() {}\n")
	gitCommit(t, root, "go.mod", "main.go")
	closeStore(t, mustRefresh(t, root))

	// Staged, so the git index (and therefore GitIndexFingerprint) moves
	// — an unstaged marker edit would not be caught by the enum cache at
	// all, a separate, pre-existing limitation named in the completion
	// report, not tested here.
	writeFile(t, root, "go.mod", "module renamed\n")
	gitCommit(t, root, "go.mod")

	s, stats, err := Refresh(context.Background(), root, scope(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(t, s)
	// go.mod's own content changed, so it — unlike main.go — is sent to
	// the extract stage and counted: Dispatched is "files run through
	// lang.Extract this call" (directive 016 §1: go.mod is itself a
	// candidate file, handed over and handled by the generic module,
	// same as README.md would be; directive 018 §4 named the field for
	// this), not "files a language module found declarations in."
	if stats.Dispatched != 1 {
		t.Fatalf("Dispatched = %d, want 1 (go.mod's own content changed) — main.go's did not, so it must not be counted", stats.Dispatched)
	}
	found := false
	for e := range s.All() {
		if e.Name == "Main" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected Main's declaration to survive, carried forward under a fresh id")
	}
}

// TestRefresh_UnstagedMarkerDeletionUpdatesProject (directive 015 item
// 4): `git ls-files -s` reports the index, not the working tree, so a
// tracked marker removed from disk without `git rm` changes neither
// GitIndexFingerprint nor a fresh enumeration's own output — the same
// blind spot C-2 fixed for declarations, left open for markers until
// now (013's own diagnosis, quoted by the directive). Markers are a
// handful of files, so freshness is decided by stat instead, the same
// way C-2 already decided it for everything else — no git mechanism to
// tell staged from unstaged.
func TestRefresh_UnstagedMarkerDeletionUpdatesProject(t *testing.T) {
	root := gitRepo(t)
	writeFile(t, root, "go.mod", "module outer\n")
	writeFile(t, root, "services/api/go.mod", "module inner\n")
	writeFile(t, root, "services/api/handler.go", "package api\n\nfunc Handle() {}\n")
	gitCommit(t, root, "go.mod", "services/api/go.mod", "services/api/handler.go")
	closeStore(t, mustRefresh(t, root))

	// Removed from the working tree, never `git rm`ed: .git/index still
	// lists services/api/go.mod as tracked.
	if err := os.Remove(filepath.Join(root, "services/api/go.mod")); err != nil {
		t.Fatal(err)
	}

	s := mustRefresh(t, root)
	defer closeStore(t, s)
	rootBase := filepath.Base(root)
	sawHandle, sawHandlerFile := false, false
	for e := range s.All() {
		switch {
		case e.Name == "Handle" && e.Kind != entry.KindFile:
			sawHandle = true
			if e.Project != rootBase {
				t.Errorf("Handle's project = %q, want %q — services/api/go.mod is gone, so it is no longer its own root", e.Project, rootBase)
			}
		case e.Path == "services/api/handler.go" && e.Kind == entry.KindFile:
			sawHandlerFile = true
			if e.Project != rootBase {
				t.Errorf("handler.go's own project = %q, want %q", e.Project, rootBase)
			}
		}
	}
	if !sawHandle || !sawHandlerFile {
		t.Fatalf("expected both Handle and handler.go's file entry to survive, sawHandle=%v sawHandlerFile=%v", sawHandle, sawHandlerFile)
	}
}

// TestRefresh_NonExtractableFileContentChangeUpdatesSize (directive 015
// item 5): §20 C-2 puts stat freshness over the whole candidate set,
// with no exception for a file no language module claims — 013 wrote
// one anyway ("never re-verified" for these files' own Record), so a
// content edit to a file like README.md silently left its Size
// attribute stale forever.
func TestRefresh_NonExtractableFileContentChangeUpdatesSize(t *testing.T) {
	root := gitRepo(t)
	writeFile(t, root, "README.md", "# short\n")
	gitCommit(t, root, "README.md")
	closeStore(t, mustRefresh(t, root))

	longer := "# a substantially longer README than it was before\n"
	writeFile(t, root, "README.md", longer)

	s, stats, err := Refresh(context.Background(), root, scope(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(t, s)
	// README.md's content changed, so — like any changed file since
	// directive 016 §1 removed the extension pre-filter — it is handed
	// to lang.Extract and counted, even though only the generic module
	// answers for it.
	if stats.Dispatched != 1 {
		t.Fatalf("stats = %#v, want Dispatched=1 — README.md's content changed and is handed to lang.Extract (§1)", stats)
	}
	entries := allEntries(s)
	var found *entry.Entry
	for i, e := range entries {
		if e.Path == "README.md" {
			found = &entries[i]
		}
	}
	if found == nil {
		t.Fatal("expected README.md's entry to still be present")
	}
	if found.Size != int64(len(longer)) {
		t.Errorf("README.md Size = %d, want %d (the edited content's length, re-verified)", found.Size, len(longer))
	}
}

// TestRefresh_ExcludedExtensionProducesNoEntryAndIsNeverOpened
// (directive 017 §1, §2): a denied extension gets no entry at all —
// not even the generic module's file entry — and, since the exclusion
// module never calls File.Content, it is never actually opened: made
// unreadable by permission here, so Refresh would fail if anything
// ever tried to read it.
func TestRefresh_ExcludedExtensionProducesNoEntryAndIsNeverOpened(t *testing.T) {
	root := gitRepo(t)
	writeFile(t, root, "logo.png", "\x89PNG\r\n\x1a\n")
	gitCommit(t, root, "logo.png")
	full := filepath.Join(root, "logo.png")
	if err := os.Chmod(full, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(full, 0o644)

	s, _, err := Refresh(context.Background(), root, scope(), nil)
	if err != nil {
		t.Fatalf("Refresh failed — logo.png must not have been read: %v", err)
	}
	defer closeStore(t, s)
	for _, e := range allEntries(s) {
		if e.Path == "logo.png" {
			t.Fatalf("expected no entry for logo.png, got %#v", e)
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
// declarations extracted from it — a fact about the file, patched onto
// whichever entry in lang.Extract's own result is Kind==KindFile once
// every declaration alongside it is known (directive 016 §1), not
// computed per declaration.
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

// TestRefresh_ManyChangedFilesAllExtracted (directive 018 §1): more
// changed files than extractWorkers, so the extract stage's fixed pool
// must genuinely process every one of them, not just the first
// extractWorkers-worth — every declaration from every file must still
// be found, and Dispatched must count every one of them, regardless of
// which worker happened to pick up which file.
func TestRefresh_ManyChangedFilesAllExtracted(t *testing.T) {
	root := gitRepo(t)
	const n = 3 * extractWorkers // comfortably more than one worker's share
	var paths []string
	for i := 0; i < n; i++ {
		p := fmt.Sprintf("f%02d.go", i)
		writeFile(t, root, p, fmt.Sprintf("package p\n\nfunc F%02d() {}\n", i))
		paths = append(paths, p)
	}
	gitCommit(t, root, paths...)

	s, stats, err := Refresh(context.Background(), root, scope(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(t, s)
	if stats.Dispatched != n {
		t.Fatalf("Dispatched = %d, want %d", stats.Dispatched, n)
	}
	found := make(map[string]bool, n)
	for _, e := range declarations(allEntries(s)) {
		found[e.Name] = true
	}
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("F%02d", i)
		if !found[name] {
			t.Errorf("expected declaration %s to survive concurrent extraction, got %v", name, found)
		}
	}
}

// TestRefresh_WarningsAreSortedByPathRegardlessOfConcurrency (directive
// 018 §1, §3 C5): extract workers finish in whatever order the
// scheduler picks, so runPipeline sorts warnings by path before
// returning them — otherwise the same tree could print ## warnings in a
// different order from one run to the next, breaking C5's "byte-
// identical index output." Deliberately writes the warning-producing
// files in reverse path order, so a pass-through-unsorted regression
// would show up as out-of-order output here rather than accidentally
// passing because creation order already matched sort order.
func TestRefresh_WarningsAreSortedByPathRegardlessOfConcurrency(t *testing.T) {
	root := gitRepo(t)
	var paths []string
	for i := extractWorkers; i >= 0; i-- {
		p := fmt.Sprintf("bad%02d.go", i)
		writeFile(t, root, p, "package p\n\nfunc ( { // invalid syntax\n")
		paths = append(paths, p)
	}
	gitCommit(t, root, paths...)

	s, stats, err := Refresh(context.Background(), root, scope(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(t, s)
	if len(stats.Warnings) != len(paths) {
		t.Fatalf("Warnings = %#v, want %d entries (one per malformed file)", stats.Warnings, len(paths))
	}
	sorted := append([]string(nil), stats.Warnings...)
	sort.Strings(sorted)
	for i := range stats.Warnings {
		if stats.Warnings[i] != sorted[i] {
			t.Fatalf("Warnings = %#v, not sorted by path", stats.Warnings)
		}
	}
}
