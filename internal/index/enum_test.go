package index

import (
	"context"
	"os/exec"
	"testing"

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
	patterns := []scanner.Pattern{"vendor/"}
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
