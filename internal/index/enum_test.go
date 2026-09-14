package index

import (
	"context"
	"os/exec"
	"sort"
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

// TestFilterTracked_MatchesFilterMetasAndMarkersOfCombined confirms
// FilterTracked/FilterUntracked (used only by the enum cache) agree
// with filterMetas/markersOf (used by the uncached scanner.Discover
// path) on the same input — the two must never quietly diverge, since
// nothing else would catch it before ranking or output.
func TestFilterTracked_MatchesFilterMetasAndMarkersOfCombined(t *testing.T) {
	root := gitRepo(t)
	writeFile(t, root, "a.go", "package p\n\nfunc A() {}\n")
	writeFile(t, root, "vendor/dep/dep.go", "package dep\n")
	writeFile(t, root, "go.mod", "module example\n")
	gitCommit(t, root, "a.go", "vendor/dep/dep.go", "go.mod")
	writeFile(t, root, "wip.go", "package p\n\nfunc WIP() {}\n") // untracked

	sc := scope()
	viaDiscover, markersViaDiscover, err := scanner.Discover(context.Background(), root, sc)
	if err != nil {
		t.Fatal(err)
	}

	tracked, err := scanner.EnumerateTracked(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	untracked, err := scanner.EnumerateUntracked(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	trackedMetas, trackedMarkers := scanner.FilterTracked(root, tracked, sc)
	untrackedMetas, untrackedMarkers := scanner.FilterUntracked(root, untracked, sc)
	viaSplit := scanner.MergeSortedMetas(trackedMetas, untrackedMetas)
	markersViaSplit := append(append([]string{}, trackedMarkers...), untrackedMarkers...)

	if len(viaDiscover) != len(viaSplit) {
		t.Fatalf("metas differ in count: Discover=%d split=%d", len(viaDiscover), len(viaSplit))
	}
	for i := range viaDiscover {
		if viaDiscover[i] != viaSplit[i] {
			t.Fatalf("metas differ at %d: Discover=%#v split=%#v", i, viaDiscover[i], viaSplit[i])
		}
	}
	if len(markersViaDiscover) != len(markersViaSplit) {
		t.Fatalf("markers differ in count: Discover=%v split=%v", markersViaDiscover, markersViaSplit)
	}
}

// TestStreamTracked_MatchesFilterTracked confirms the streaming path
// (directive 019 §2) accepts and marks exactly the same paths as the
// batch path (FilterTracked) it replaces inside enumerate — the two
// must never quietly diverge on what "included" and "marker" mean,
// since nothing else would catch it before ranking or output.
func TestStreamTracked_MatchesFilterTracked(t *testing.T) {
	root := gitRepo(t)
	writeFile(t, root, "a.go", "package p\n\nfunc A() {}\n")
	writeFile(t, root, "vendor/dep/dep.go", "package dep\n")
	writeFile(t, root, "go.mod", "module example\n")
	gitCommit(t, root, "a.go", "vendor/dep/dep.go", "go.mod")

	sc := scope()
	tracked, err := scanner.EnumerateTracked(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	wantMetas, wantMarkers := scanner.FilterTracked(root, tracked, sc)

	var gotMetas []scanner.FileMeta
	var gotMarkers []string
	if err := scanner.StreamTracked(context.Background(), root, sc,
		func(m scanner.FileMeta) { gotMetas = append(gotMetas, m) },
		func(p string) { gotMarkers = append(gotMarkers, p) },
	); err != nil {
		t.Fatal(err)
	}
	sort.Slice(gotMetas, func(i, j int) bool { return gotMetas[i].Path < gotMetas[j].Path })
	sort.Strings(gotMarkers)

	if len(gotMetas) != len(wantMetas) {
		t.Fatalf("metas differ in count: stream=%d batch=%d", len(gotMetas), len(wantMetas))
	}
	for i := range wantMetas {
		if gotMetas[i] != wantMetas[i] {
			t.Errorf("meta %d: stream=%#v batch=%#v", i, gotMetas[i], wantMetas[i])
		}
	}
	if len(gotMarkers) != len(wantMarkers) {
		t.Fatalf("markers differ: stream=%v batch=%v", gotMarkers, wantMarkers)
	}
}

// TestStreamUntracked_MatchesFilterUntracked is StreamTracked's own
// equivalence test for the untracked half.
func TestStreamUntracked_MatchesFilterUntracked(t *testing.T) {
	root := gitRepo(t)
	writeFile(t, root, "wip.go", "package p\n\nfunc WIP() {}\n")
	writeFile(t, root, "vendor/dep/dep.go", "package dep\n")

	sc := scope()
	untracked, err := scanner.EnumerateUntracked(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	wantMetas, wantMarkers := scanner.FilterUntracked(root, untracked, sc)

	var gotMetas []scanner.FileMeta
	var gotMarkers []string
	if err := scanner.StreamUntracked(context.Background(), root, sc,
		func(m scanner.FileMeta) { gotMetas = append(gotMetas, m) },
		func(p string) { gotMarkers = append(gotMarkers, p) },
	); err != nil {
		t.Fatal(err)
	}
	sort.Slice(gotMetas, func(i, j int) bool { return gotMetas[i].Path < gotMetas[j].Path })
	sort.Strings(gotMarkers)

	if len(gotMetas) != len(wantMetas) {
		t.Fatalf("metas differ in count: stream=%d batch=%d", len(gotMetas), len(wantMetas))
	}
	for i := range wantMetas {
		if gotMetas[i] != wantMetas[i] {
			t.Errorf("meta %d: stream=%#v batch=%#v", i, gotMetas[i], wantMetas[i])
		}
	}
	if len(gotMarkers) != len(wantMarkers) {
		t.Fatalf("markers differ: stream=%v batch=%v", gotMarkers, wantMarkers)
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
