package index

import (
	"context"
	"os"
	"os/exec"
	"testing"

	"giraffe/internal/entry"
	"giraffe/internal/scanner"
)

// loadEnumCacheForTest opens root's store read-only and decodes its enum
// blob — the test-side equivalent of what discover does internally,
// used here to inspect enum-cache state directly rather than only
// through Refresh's externally-visible effects.
func loadEnumCacheForTest(t *testing.T, root string) (enumCache, bool) {
	t.Helper()
	s, ok, err := openStore(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		return enumCache{}, false
	}
	defer s.Close()
	blob, err := s.enumBlob()
	if err != nil {
		t.Fatal(err)
	}
	if blob == nil {
		return enumCache{}, false
	}
	return decodeEnumCache(blob)
}

func TestEnumCache_EncodeDecodeRoundTrip(t *testing.T) {
	metas := []scanner.FileMeta{{Path: "a.go", BlobHash: "hash-a"}, {Path: "b/c.go", BlobHash: "hash-c"}}
	markers := []string{"go.mod"}
	blob := encodeEnumCache(123, 456, "vendor/", metas, markers)

	got, ok := decodeEnumCache(blob)
	if !ok {
		t.Fatal("expected decodeEnumCache to succeed after encodeEnumCache")
	}
	if got.IndexModTime != 123 || got.IndexSize != 456 {
		t.Errorf("fingerprint = %d/%d, want 123/456", got.IndexModTime, got.IndexSize)
	}
	if got.Config != "vendor/" {
		t.Errorf("config fingerprint = %q, want %q", got.Config, "vendor/")
	}
	if len(got.TrackedMetas) != 2 || got.TrackedMetas[0].Path != "a.go" || got.TrackedMetas[0].BlobHash != "hash-a" {
		t.Errorf("trackedMetas = %#v", got.TrackedMetas)
	}
	if len(got.TrackedMarkers) != 1 || got.TrackedMarkers[0] != "go.mod" {
		t.Errorf("trackedMarkers = %#v", got.TrackedMarkers)
	}
}

func TestEnumCache_MissingStoreIsNotAnError(t *testing.T) {
	root := t.TempDir()
	if _, ok := loadEnumCacheForTest(t, root); ok {
		t.Fatal("expected ok=false for a store that was never written")
	}
}

func TestEnumCache_MalformedShapeIsDiscarded(t *testing.T) {
	// A previous format's shape, or on-disk corruption — not this
	// version's "modtime\tsize\texclude\n" then "F\t.../M\t..." lines
	// shape.
	if _, ok := decodeEnumCache([]byte("garbage\n")); ok {
		t.Fatal("expected ok=false for a blob whose header does not parse")
	}
}

// TestRefresh_NewUntrackedFileAppearsAfterEnumCacheIsWarm (§26 C-28/item
// 5): the correctness case the directive names outright. A cache keyed
// on the git index (or on HEAD) alone would miss this — an untracked
// file's appearance changes neither. discover always re-runs the
// untracked half of enumeration fresh specifically so this cannot
// regress into C-2's "freshly written code reads as absent" failure.
func TestRefresh_NewUntrackedFileAppearsAfterEnumCacheIsWarm(t *testing.T) {
	root := gitRepo(t)
	writeFile(t, root, "tracked.go", "package p\n\nfunc Tracked() {}\n")
	gitCommit(t, root, "tracked.go")

	// First call: warms the enum cache (the git index has a fingerprint
	// the moment a commit exists).
	closeStore(t, mustRefresh(t, root))
	if _, ok := loadEnumCacheForTest(t, root); !ok {
		t.Fatal("expected the enum cache to be warm after the first Refresh")
	}

	// The working tree moves — a brand-new untracked file — without
	// HEAD or the git index moving at all.
	writeFile(t, root, "wip.go", "package p\n\nfunc WorkInProgress() {}\n")

	s := mustRefresh(t, root)
	defer closeStore(t, s)
	found := false
	for e := range s.All() {
		if e.Name == "WorkInProgress" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected the new untracked file's declaration to appear even though the enum cache was warm")
	}
}

// TestRefresh_StagedAddIsPickedUpAfterEnumCacheIsWarm (§26 C-28/item 5):
// `git add`ing a new file changes the git index without moving HEAD.
// GitIndexFingerprint must catch this and rebuild the tracked cache —
// a cache keyed on HEAD alone would miss it, since the file is
// simultaneously removed from `git ls-files --others` (no longer
// "other") and added to `git ls-files -s`, so a stale cached tracked
// map would lose it entirely rather than merely misclassify it.
func TestRefresh_StagedAddIsPickedUpAfterEnumCacheIsWarm(t *testing.T) {
	root := gitRepo(t)
	writeFile(t, root, "tracked.go", "package p\n\nfunc Tracked() {}\n")
	gitCommit(t, root, "tracked.go")

	closeStore(t, mustRefresh(t, root))
	before, ok := loadEnumCacheForTest(t, root)
	if !ok {
		t.Fatal("expected the enum cache to be warm after the first Refresh")
	}

	writeFile(t, root, "staged.go", "package p\n\nfunc Staged() {}\n")
	cmd := exec.Command("git", "add", "staged.go")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}

	s := mustRefresh(t, root)
	found := false
	for e := range s.All() {
		if e.Name == "Staged" {
			found = true
		}
	}
	closeStore(t, s)
	if !found {
		t.Fatal("expected the staged-but-uncommitted file's declaration to appear")
	}

	afterCache, ok := loadEnumCacheForTest(t, root)
	if !ok {
		t.Fatal("expected the enum cache to be rewritten after the staged add")
	}
	if afterCache.IndexModTime == before.IndexModTime && afterCache.IndexSize == before.IndexSize {
		t.Error("expected `git add` to change the index fingerprint the enum cache is keyed on")
	}
}

// TestRefresh_EnumCacheReusedWhenNothingChanges: a second Refresh with
// no change to the working tree must not rewrite the enum cache to a
// different fingerprint — reuse, not silent rebuilding on every call.
func TestRefresh_EnumCacheReusedWhenNothingChanges(t *testing.T) {
	root := gitRepo(t)
	writeFile(t, root, "a.go", "package p\n\nfunc A() {}\n")
	gitCommit(t, root, "a.go")

	closeStore(t, mustRefresh(t, root))
	first, ok := loadEnumCacheForTest(t, root)
	if !ok {
		t.Fatal("expected the enum cache to be warm")
	}

	closeStore(t, mustRefresh(t, root))
	second, ok := loadEnumCacheForTest(t, root)
	if !ok {
		t.Fatal("expected the enum cache to still be present")
	}
	if first.IndexModTime != second.IndexModTime || first.IndexSize != second.IndexSize {
		t.Error("expected the fingerprint to stay the same across two no-op Refresh calls")
	}
	if len(first.TrackedMetas) != len(second.TrackedMetas) {
		t.Errorf("tracked set changed across no-op calls: %d vs %d", len(first.TrackedMetas), len(second.TrackedMetas))
	}
}

// loadWalkCacheForTest is loadEnumCacheForTest's counterpart for the
// walk cache (directive 021 item 2).
func loadWalkCacheForTest(t *testing.T, root string) (scanner.WalkCache, bool) {
	t.Helper()
	s, ok, err := openStore(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		return scanner.WalkCache{}, false
	}
	defer s.Close()
	blob, err := s.walkBlob()
	if err != nil {
		t.Fatal(err)
	}
	if blob == nil {
		return scanner.WalkCache{}, false
	}
	return decodeWalkCache(blob)
}

func TestWalkCache_EncodeDecodeRoundTrip(t *testing.T) {
	w := scanner.WalkCache{Dirs: map[string]scanner.WalkDirEntry{
		".":   {ModTime: 111, Files: []string{"a.go"}, Subdirs: []string{"pkg"}},
		"pkg": {ModTime: 222, Files: []string{"b.go", "c.go"}},
	}}
	got, ok := decodeWalkCache(encodeWalkCache(w))
	if !ok {
		t.Fatal("expected decodeWalkCache to succeed after encodeWalkCache")
	}
	if len(got.Dirs) != 2 {
		t.Fatalf("Dirs = %#v, want 2 entries", got.Dirs)
	}
	if got.Dirs["."].ModTime != 111 || len(got.Dirs["."].Files) != 1 || got.Dirs["."].Files[0] != "a.go" {
		t.Errorf(". entry = %#v", got.Dirs["."])
	}
	if got.Dirs["pkg"].ModTime != 222 || len(got.Dirs["pkg"].Files) != 2 {
		t.Errorf("pkg entry = %#v", got.Dirs["pkg"])
	}
}

func TestWalkCache_MalformedShapeIsDiscarded(t *testing.T) {
	if _, ok := decodeWalkCache([]byte("garbage\n")); ok {
		t.Fatal("expected ok=false for a blob whose line shape does not parse")
	}
}

// TestRefresh_NoGitRoot_ProducesAnIndex is directive 021 item 1's own
// completion condition, end to end: before this directive, a root with
// no .git at all made every enumeration call fail outright ("fatal: not
// a git repository") — Refresh now indexes it via Walk alone.
func TestRefresh_NoGitRoot_ProducesAnIndex(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.go", "package p\n\nfunc NoGit() {}\n")
	if scanner.HasGitDir(root) {
		t.Fatal("test setup: expected no .git in a fresh temp dir")
	}

	s := mustRefresh(t, root)
	defer closeStore(t, s)
	found := false
	for e := range s.All() {
		if e.Name == "NoGit" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected a no-git root to still be indexed via Walk alone")
	}
}

// TestRefresh_TrackedAndWalkedFilesBothAppearOnce confirms the two
// enumeration routes a git repository combines (StreamTracked, Walk)
// never emit the same path twice and never lose either half's own
// file — the invariant scanner.FilterWalked's skip set exists to
// guarantee.
func TestRefresh_TrackedAndWalkedFilesBothAppearOnce(t *testing.T) {
	root := gitRepo(t)
	writeFile(t, root, "tracked.go", "package p\n\nfunc Tracked() {}\n")
	gitCommit(t, root, "tracked.go")
	writeFile(t, root, "walked.go", "package p\n\nfunc Walked() {}\n") // untracked

	s := mustRefresh(t, root)
	defer closeStore(t, s)
	counts := map[string]int{}
	for e := range s.All() {
		counts[e.Name]++
	}
	if counts["Tracked"] != 1 {
		t.Errorf("Tracked appeared %d times, want 1", counts["Tracked"])
	}
	if counts["Walked"] != 1 {
		t.Errorf("Walked appeared %d times, want 1", counts["Walked"])
	}
}

// TestRefresh_WalkCacheWarmsAndPersists (directive 021 item 2): a
// store's walk cache is written on the first Refresh and still present
// — not merely non-empty, but still describing the root directory — on
// a second, unchanged call.
func TestRefresh_WalkCacheWarmsAndPersists(t *testing.T) {
	root := gitRepo(t)
	writeFile(t, root, "a.go", "package p\n\nfunc A() {}\n")
	gitCommit(t, root, "a.go")
	writeFile(t, root, "wip.go", "package p\n\nfunc WIP() {}\n")

	closeStore(t, mustRefresh(t, root))
	first, ok := loadWalkCacheForTest(t, root)
	if !ok {
		t.Fatal("expected the walk cache to be warm after the first Refresh")
	}
	if _, ok := first.Dirs["."]; !ok {
		t.Fatalf("expected the walk cache to describe the root directory, got %#v", first.Dirs)
	}

	closeStore(t, mustRefresh(t, root))
	second, ok := loadWalkCacheForTest(t, root)
	if !ok {
		t.Fatal("expected the walk cache to still be present after a second Refresh")
	}
	if second.Dirs["."].ModTime != first.Dirs["."].ModTime {
		t.Error("expected the root directory's cached mtime to stay the same across two no-op Refresh calls")
	}
}

// TestFingerprintConfig_ChangesWithAssignments (directive 019 §3):
// 017 already put the exclude-pattern list into the enum cache's
// fingerprint; this directive adds the assignments map, so an
// operator adding or changing an ext:/lang: line — with the pattern
// list byte-for-byte unchanged — still invalidates the cached tracked
// enumeration.
func TestFingerprintConfig_ChangesWithAssignments(t *testing.T) {
	patterns := []scanner.Pattern{mustPattern(t, "vendor/")}
	base := fingerprintConfig(patterns, nil)
	withAssignment := fingerprintConfig(patterns, map[string]string{".png": "image"})
	if base == withAssignment {
		t.Fatal("expected fingerprintConfig to differ once an assignment is added, with patterns unchanged")
	}
	changedLang := fingerprintConfig(patterns, map[string]string{".png": "raster"})
	if withAssignment == changedLang {
		t.Fatal("expected fingerprintConfig to differ when an assignment's language changes")
	}
	indexOnly := fingerprintConfig(patterns, map[string]string{".png": ""})
	if indexOnly == base || indexOnly == withAssignment {
		t.Fatal("expected an index-only assignment's fingerprint to differ from both no-assignment and a labelled one")
	}
	// Order must not matter (map iteration is not deterministic, §3 C5).
	multi1 := fingerprintConfig(patterns, map[string]string{".png": "image", ".inc": "c"})
	multi2 := fingerprintConfig(patterns, map[string]string{".inc": "c", ".png": "image"})
	if multi1 != multi2 {
		t.Errorf("fingerprintConfig is sensitive to map iteration order: %q vs %q", multi1, multi2)
	}
}

// TestRefresh_NoGitRoot_HonorsGitignore (directive 022 item 1, end to
// end): the gap named precisely against 021's own completion report —
// a no-git root now produces an index (021), but nothing read
// .gitignore, so a bare build-output directory would have been walked
// and indexed in full.
func TestRefresh_NoGitRoot_HonorsGitignore(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ".gitignore", "target/\n")
	writeFile(t, root, "src.go", "package p\n\nfunc Kept() {}\n")
	writeFile(t, root, "target/generated.go", "package p\n\nfunc Generated() {}\n")

	s := mustRefresh(t, root)
	defer closeStore(t, s)
	names := map[string]bool{}
	for e := range s.All() {
		names[e.Name] = true
	}
	if !names["Kept"] {
		t.Error("expected src.go's declaration to be indexed")
	}
	if names["Generated"] {
		t.Error("expected target/'s declaration to be excluded by .gitignore")
	}
}

// TestRefresh_GitRoot_UntrackedHalfHonorsGitignore is
// TestRefresh_NoGitRoot_HonorsGitignore's counterpart for an ordinary
// git repository's own untracked half — the condition directive 022's
// own item 2 names precisely: every measurement before this directive
// used a fresh, artifact-free clone, so a real build-output directory
// was never actually present during a walked-half test.
func TestRefresh_GitRoot_UntrackedHalfHonorsGitignore(t *testing.T) {
	root := gitRepo(t)
	writeFile(t, root, "tracked.go", "package p\n\nfunc Tracked() {}\n")
	gitCommit(t, root, "tracked.go")
	writeFile(t, root, ".gitignore", "target/\n") // untracked, same as most real repos
	writeFile(t, root, "target/generated.go", "package p\n\nfunc Generated() {}\n")

	s := mustRefresh(t, root)
	defer closeStore(t, s)
	names := map[string]bool{}
	for e := range s.All() {
		names[e.Name] = true
	}
	if !names["Tracked"] {
		t.Error("expected tracked.go's declaration to be indexed")
	}
	if names["Generated"] {
		t.Error("expected target/'s declaration to be excluded by the untracked .gitignore")
	}
}

// submoduleRepo is enum_test.go's own copy of
// scanner_test.go's fixture of the same name (code-manners D6: a
// second occurrence, noted here rather than extracted — a third would
// force it) — index has no access to scanner's unexported test helpers
// across a package boundary, and this directive's own submodule
// questions need an index-level (project resolution, Refresh)
// assertion the scanner-level fixture cannot give by itself.
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
	writeFile(t, inner, "inner.go", "package inner\n\nfunc InSubmodule() {}\n")
	writeFile(t, inner, "go.mod", "module inner\ngo 1.21\n")
	run(inner, "add", "inner.go", "go.mod")
	run(inner, "commit", "-q", "-m", "init")

	run(outer, "init", "-q")
	writeFile(t, outer, "outer.go", "package outer\n\nfunc InOuter() {}\n")
	run(outer, "add", "outer.go")
	run(outer, "commit", "-q", "-m", "init")
	run(outer, "-c", "protocol.file.allow=always", "submodule", "add", inner, "sub")
	run(outer, "add", ".gitmodules", "sub")
	run(outer, "commit", "-q", "-m", "add submodule")

	dir, err := scanner.Root(outer)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestRefresh_SubmoduleContentsIndexedUnderTheirOwnProject (directive
// 022 item 3): a submodule's own go.mod is discovered as its own
// marker directory, the same as any other nested marker — no special
// casing for submodules, decided and confirmed here rather than
// assumed. The submodule's own gitlink entry must not itself produce a
// bogus entry (the bug this directive fixes: before it, `sub` was
// dispatched through extraction as if it were a file).
func TestRefresh_SubmoduleContentsIndexedUnderTheirOwnProject(t *testing.T) {
	root := submoduleRepo(t)
	s := mustRefresh(t, root)
	defer closeStore(t, s)

	byName := map[string]entry.Entry{}
	for e := range s.All() {
		byName[e.Name] = e
	}
	inSub, ok := byName["InSubmodule"]
	if !ok {
		t.Fatal("expected the submodule's own declaration to be indexed")
	}
	if inSub.Project != "sub" {
		t.Errorf("InSubmodule.Project = %q, want %q (the submodule's own marker directory)", inSub.Project, "sub")
	}
	inOuter, ok := byName["InOuter"]
	if !ok {
		t.Fatal("expected the outer repository's own declaration to be indexed")
	}
	if inOuter.Project == "sub" {
		t.Errorf("InOuter.Project = %q, expected the outer project's own name, not the submodule's", inOuter.Project)
	}

	if _, ok := s.fileRecord("sub"); ok {
		t.Error("expected the submodule's own gitlink entry not to become a file record")
	}
}

// TestRefresh_EnumCacheInvalidatedByAssignmentChangeAlone confirms the
// same fact end to end through Refresh, with the exclude pattern list
// held fixed across both calls: only the assignments map changes.
func TestRefresh_EnumCacheInvalidatedByAssignmentChangeAlone(t *testing.T) {
	root := gitRepo(t)
	writeFile(t, root, "a.go", "package p\n\nfunc A() {}\n")
	gitCommit(t, root, "a.go")

	s1, _, err := Refresh(context.Background(), root, scope(), nil)
	if err != nil {
		t.Fatal(err)
	}
	closeStore(t, s1)
	before, ok := loadEnumCacheForTest(t, root)
	if !ok {
		t.Fatal("expected the enum cache to be warm after the first Refresh")
	}

	s2, _, err := Refresh(context.Background(), root, scope(), map[string]string{".png": "image"})
	if err != nil {
		t.Fatal(err)
	}
	closeStore(t, s2)
	after, ok := loadEnumCacheForTest(t, root)
	if !ok {
		t.Fatal("expected the enum cache to still be present")
	}
	if before.Config == after.Config {
		t.Error("expected the enum cache's own config fingerprint to change once an assignment is added, even with .git/index untouched")
	}
}

// TestRunTrackedTree_RecursesIntoSubmodule_ReportsBlobHashes (directive
// 023 item 3): "a submodule is enumerated by git, inside itself" — a
// submodule's tracked files are resolved via its own `git ls-files -s`,
// not the walker, and carry a real blob hash the way any other tracked
// file does (022's own walked-file route always reported BlobHash "").
func TestRunTrackedTree_RecursesIntoSubmodule_ReportsBlobHashes(t *testing.T) {
	root := submoduleRepo(t)
	metaCh := make(chan scanner.FileMeta, 64)
	metas, candidates, err := runTrackedTree(context.Background(), root, root, "", scope(), metaCh)
	close(metaCh)
	if err != nil {
		t.Fatal(err)
	}

	var got *scanner.FileMeta
	for i := range metas {
		if metas[i].Path == "sub/inner.go" {
			m := metas[i]
			got = &m
		}
	}
	if got == nil {
		t.Fatalf("expected sub/inner.go among the tracked tree's metas, got %#v", metas)
	}
	if got.BlobHash == "" {
		t.Error("expected sub/inner.go to carry a real blob hash (resolved via the submodule's own git, not walked)")
	}

	hasGoMod := false
	for _, c := range candidates {
		if c == "sub/go.mod" {
			hasGoMod = true
		}
	}
	if !hasGoMod {
		t.Errorf("expected sub/go.mod among the marker candidates, got %v", candidates)
	}
}

// nestedSubmoduleRepo builds an outer repository whose submodule itself
// contains a submodule — directive 023 item 3's "nested submodules are
// the same case recursively" — three repositories deep.
func nestedSubmoduleRepo(t *testing.T) string {
	t.Helper()
	leaf := t.TempDir()
	mid := t.TempDir()
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

	run(leaf, "init", "-q")
	writeFile(t, leaf, "leaf.go", "package leaf\n\nfunc InLeaf() {}\n")
	run(leaf, "add", "leaf.go")
	run(leaf, "commit", "-q", "-m", "init")

	run(mid, "init", "-q")
	writeFile(t, mid, "mid.go", "package mid\n\nfunc InMid() {}\n")
	run(mid, "add", "mid.go")
	run(mid, "commit", "-q", "-m", "init")
	run(mid, "-c", "protocol.file.allow=always", "submodule", "add", leaf, "leafsub")
	run(mid, "add", ".gitmodules", "leafsub")
	run(mid, "commit", "-q", "-m", "add leaf submodule")

	run(outer, "init", "-q")
	writeFile(t, outer, "outer.go", "package outer\n\nfunc InOuter() {}\n")
	run(outer, "add", "outer.go")
	run(outer, "commit", "-q", "-m", "init")
	run(outer, "-c", "protocol.file.allow=always", "submodule", "add", mid, "midsub")
	run(outer, "add", ".gitmodules", "midsub")
	run(outer, "commit", "-q", "-m", "add mid submodule")
	run(outer, "-c", "protocol.file.allow=always", "submodule", "update", "--init", "--recursive")

	dir, err := scanner.Root(outer)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRunTrackedTree_NestedSubmodule_RecursesAgain(t *testing.T) {
	root := nestedSubmoduleRepo(t)
	metaCh := make(chan scanner.FileMeta, 64)
	metas, _, err := runTrackedTree(context.Background(), root, root, "", scope(), metaCh)
	close(metaCh)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]scanner.FileMeta{}
	for _, m := range metas {
		byPath[m.Path] = m
	}
	for _, want := range []string{"outer.go", "midsub/mid.go", "midsub/leafsub/leaf.go"} {
		m, ok := byPath[want]
		if !ok {
			t.Errorf("expected %s among the tracked tree's metas, got %v", want, byPath)
			continue
		}
		if m.BlobHash == "" {
			t.Errorf("expected %s to carry a real blob hash", want)
		}
	}
}

// TestRefresh_SubmoduleFileAddedAfterEnumCacheWarm_StillIndexed
// (directive 023 item 3's own measurement finding, locked in as a test):
// the outer enum cache's own fingerprint is the outer root's .git/index
// mtime and size (fingerprintConfig/tryEnumCache) — it says nothing
// about a submodule's own, separate .git/index, so a file added inside
// a submodule after the outer cache is warm does not invalidate it.
// runTrackedHalf's cache-hit path therefore replays the old
// TrackedMetas without ever calling runTrackedTree again — but the new
// file is not silently lost (§3's own "does not exist" rule): Walk
// re-reads the submodule's own directory unconditionally every call (its
// mtime moved when the new file was added), finds it, and — since the
// stale tracked skip-set does not name it — FilterWalked reports it
// through the walked route instead of the tracked one. Correct in
// outcome, not in mechanism: named here, and in the completion report,
// as a real, disclosed nuance rather than claimed as a clean win.
func TestRefresh_SubmoduleFileAddedAfterEnumCacheWarm_StillIndexed(t *testing.T) {
	root := submoduleRepo(t)
	closeStore(t, mustRefresh(t, root)) // warm the outer enum cache

	subDir := root + "/sub"
	writeFile(t, subDir, "newfile.go", "package inner\n\nfunc BrandNew() {}\n")
	gitCommit(t, subDir, "newfile.go") // changes sub's own .git/index, not the outer one

	s := mustRefresh(t, root)
	defer closeStore(t, s)
	found := false
	for e := range s.All() {
		if e.Name == "BrandNew" {
			found = true
		}
	}
	if !found {
		t.Error("expected a file added inside the submodule after the outer enum cache warmed to still be indexed (via the walked route, since the tracked cache is not itself invalidated by a submodule-internal change)")
	}
}

// TestRunTrackedTree_UncheckedOutSubmodule_ContributesNothing: a
// submodule registered in the outer index (a gitlink entry exists) but
// never `git submodule update`d has no .git of its own — HasGitDir is
// false, so this recursion contributes nothing for it, exactly as the
// directive names ("a submodule that is not checked out... contributes
// nothing here"). Whatever the directory does contain on disk is still
// reachable through the walked half, unchanged from directive 022 — not
// this function's concern.
func TestRunTrackedTree_UncheckedOutSubmodule_ContributesNothing(t *testing.T) {
	root := submoduleRepo(t)
	if err := os.Remove(root + "/sub/.git"); err != nil {
		t.Fatal(err)
	}

	metaCh := make(chan scanner.FileMeta, 64)
	metas, candidates, err := runTrackedTree(context.Background(), root, root, "", scope(), metaCh)
	close(metaCh)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range metas {
		if m.Path == "sub/inner.go" {
			t.Errorf("expected no tracked entry for sub/inner.go once sub/.git is gone, got %+v", m)
		}
	}
	for _, c := range candidates {
		if c == "sub/go.mod" {
			t.Error("expected no marker candidate from inside an uninitialised submodule")
		}
	}
}
