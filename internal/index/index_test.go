package index

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	bolt "go.etcd.io/bbolt"

	"giraffe/internal/entry"
)

func TestStore_BuildAndQuery_RoundTrip(t *testing.T) {
	root := t.TempDir()
	s, err := createStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(t, s)

	changes := []fileChange{
		{
			Path: "internal/retry/retry.go", ModTime: 111, Size: 222,
			Entries: []entry.Entry{
				{
					Path: "internal/retry/retry.go", Name: "Retry", Kind: "func",
					Project: "giraffe", Scope: "exported", Exported: true, Doc: "Retry retries with backoff.",
					Position: []entry.Position{{Name: "module", Value: "internal/retry"}},
				},
				{
					Path: "internal/retry/retry.go", Name: "policyImpl", Kind: "type",
					Project: "giraffe", Scope: "package", Exported: false,
					Position: []entry.Position{{Name: "module", Value: "internal/retry"}},
				},
			},
		},
		{
			Path: "main.go", ModTime: 333, Size: 444,
			Entries: []entry.Entry{{Path: "main.go", Name: "main", Kind: "func", Project: "giraffe"}},
		},
	}
	if _, err := s.commit(changes, nil, cacheBlobs{}, commitBulk); err != nil {
		t.Fatal(err)
	}

	got := allEntries(s)
	if len(got) != 3 {
		t.Fatalf("All() returned %d entries, want 3", len(got))
	}
	byName := make(map[string]entry.Entry, len(got))
	for _, e := range got {
		byName[e.Name] = e
	}
	if byName["Retry"].Doc != "Retry retries with backoff." {
		t.Errorf("Retry.Doc = %q", byName["Retry"].Doc)
	}
	if len(byName["Retry"].Position) != 1 || byName["Retry"].Position[0].Value != "internal/retry" {
		t.Errorf("Retry.Position = %#v", byName["Retry"].Position)
	}
	if byName["policyImpl"].Exported {
		t.Error("policyImpl should not be Exported")
	}

	rec, ok := s.fileRecord("internal/retry/retry.go")
	if !ok {
		t.Fatal("expected a files record for internal/retry/retry.go")
	}
	if rec.ModTime != 111 || rec.Size != 222 {
		t.Errorf("fileRecord = %+v, want ModTime=111 Size=222", rec)
	}
	if len(rec.EntryIDs) != 2 {
		t.Errorf("expected 2 entry ids for internal/retry/retry.go, got %d", len(rec.EntryIDs))
	}

	// Candidates narrows to entries whose Name could contain the word —
	// the trigram posting list, not a scan (§30 C-47).
	var candidateNames []string
	for e := range s.Candidates("retry") {
		candidateNames = append(candidateNames, e.Name)
	}
	if len(candidateNames) != 1 || candidateNames[0] != "Retry" {
		t.Errorf("Candidates(retry) = %v, want [Retry]", candidateNames)
	}
	var noCandidates int
	for range s.Candidates("zzznomatch") {
		noCandidates++
	}
	if noCandidates != 0 {
		t.Errorf("Candidates(zzznomatch) returned %d entries, want 0", noCandidates)
	}
}

// TestOpenStore_MissingStoreIsNotAnError mirrors the old cache's "nothing
// generated yet" case (§9): a store that has never been built is not an
// error, readOnly or not.
func TestOpenStore_MissingStoreIsNotAnError(t *testing.T) {
	root := t.TempDir()
	if _, ok, err := openStore(root, true); err != nil || ok {
		t.Fatalf("openStore(readOnly) on a missing store = ok=%v err=%v, want ok=false err=nil", ok, err)
	}
}

// TestOpenStore_NotABoltFileIsDiscarded (§23 C-14, carried over to the
// binary store per §30 C-47): a file that does not even open as a bbolt
// database — a previous giraffe version's text cache sitting at the
// same path, or on-disk corruption — is discarded and rebuilt from
// scratch, never partially recovered.
func TestOpenStore_NotABoltFileIsDiscarded(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, Dir, cacheDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, storeFilename), []byte("not a bolt file\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, ok, err := openStore(root, true); err != nil || ok {
		t.Fatalf("openStore(readOnly) on a garbage file = ok=%v err=%v, want ok=false err=nil", ok, err)
	}
	s, err := createStore(root)
	if err != nil {
		t.Fatalf("createStore after a garbage file should succeed by discarding it: %v", err)
	}
	closeStore(t, s)
}

// TestOpenStore_SchemaMismatchIsDiscarded: a store written under a
// different schema version is treated exactly like an unreadable one —
// discarded, never migrated (§23 C-14).
func TestOpenStore_SchemaMismatchIsDiscarded(t *testing.T) {
	root := t.TempDir()
	s, err := createStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketMeta).Put(keySchemaVersion, []byte("stale-version"))
	}); err != nil {
		t.Fatal(err)
	}
	closeStore(t, s)

	if _, ok, err := openStore(root, true); err != nil || ok {
		t.Fatalf("openStore(readOnly) on a stale schema = ok=%v err=%v, want ok=false err=nil", ok, err)
	}
	fresh, ok, err := openStore(root, false)
	if err != nil || !ok {
		t.Fatalf("openStore(readWrite) after a stale schema = ok=%v err=%v, want ok=true err=nil (rebuilt)", ok, err)
	}
	defer closeStore(t, fresh)
	if len(allEntries(fresh)) != 0 {
		t.Error("expected the rebuilt store to be empty — nothing was written to it yet")
	}
}

// TestStore_DeterministicOutput (§3 C5, restated for a bbolt file by
// §23 C-14: not for merging, but so the same tree provably produces the
// same result): building the same logical content twice, in the same
// process or a fresh one, must not depend on Go's randomised map
// iteration order — checked here by feeding build the same maps twice
// (constructed fresh each time, so any accidental reliance on a
// particular map's internal iteration order would show up as flakiness
// across repeated runs of this test, not just a single comparison).
func TestStore_DeterministicOutput(t *testing.T) {
	build := func(root string) []byte {
		s, err := createStore(root)
		if err != nil {
			t.Fatal(err)
		}
		changes := []fileChange{
			{Path: "b.go", ModTime: 2, Size: 20, Entries: []entry.Entry{{Path: "b.go", Name: "B", Kind: "func"}}},
			{Path: "a.go", ModTime: 1, Size: 10, Entries: []entry.Entry{{Path: "a.go", Name: "A", Kind: "func"}}},
			{Path: "c.go", ModTime: 3, Size: 30, Entries: []entry.Entry{{Path: "c.go", Name: "Cluster", Kind: "type"}}},
		}
		caches := cacheBlobs{enum: []byte("enum-blob"), writeEnum: true}
		if _, err := s.commit(changes, nil, caches, commitBulk); err != nil {
			t.Fatal(err)
		}
		closeStore(t, s)
		b, err := os.ReadFile(filepath.Join(root, Dir, cacheDir, storeFilename))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	b1 := build(t.TempDir())
	b2 := build(t.TempDir())
	if len(b1) != len(b2) {
		t.Fatalf("store files differ in size: %d vs %d bytes", len(b1), len(b2))
	}
	for i := range b1 {
		if b1[i] != b2[i] {
			t.Fatalf("store files differ at byte %d", i)
		}
	}
}

// manyChanges builds n distinct single-entry fileChanges — enough of them,
// on a fresh store, to span several bbolt pages (the default page size is
// 4KiB; one entry's own encoding is on the order of 100 bytes), which is
// what TestStore_BulkFillPercent_FewerLeafPages below needs to actually
// exercise a page-fill difference rather than fit in one page regardless.
func manyChanges(n int) []fileChange {
	changes := make([]fileChange, n)
	for i := 0; i < n; i++ {
		path := fmt.Sprintf("pkg%03d/file%03d.go", i/20, i)
		changes[i] = fileChange{
			Path: path, ModTime: int64(i), Size: int64(i),
			Entries: []entry.Entry{{Path: path, Name: fmt.Sprintf("Symbol%03d", i), Kind: "func", Project: "giraffe"}},
		}
	}
	return changes
}

// TestStore_BulkFillPercent_FewerLeafPages (directive 020 §1): a first
// build (commitBulk) packs bbolt's leaf pages fuller than a
// differential-style build (commitDifferential) would, for the same
// content — the measured effect behind this directive's own FillPercent
// change (kubernetes: a 142MB store file at the default 0.5 fill vs 78MB
// at 1.0, cold build). Checked here via bbolt's own Bucket.Stats() (leaf
// bytes actually allocated), not the store file's overall size: bbolt
// grows a file in coarse steps, so a fixture small enough for a unit
// test to run quickly can easily fit inside one such step either way and
// show no file-size difference at all despite a real difference in how
// full the pages bbolt actually wrote are underneath — LeafAlloc reports
// that directly.
func TestStore_BulkFillPercent_FewerLeafPages(t *testing.T) {
	changes := manyChanges(3000)

	leafAllocOf := func(mode commitMode) int {
		root := t.TempDir()
		s, err := createStore(root)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.commit(changes, nil, cacheBlobs{}, mode); err != nil {
			t.Fatal(err)
		}
		defer closeStore(t, s)
		var alloc int
		_ = s.db.View(func(tx *bolt.Tx) error {
			alloc = tx.Bucket(bucketEntries).Stats().LeafAlloc
			return nil
		})
		return alloc
	}

	bulkAlloc := leafAllocOf(commitBulk)
	differentialAlloc := leafAllocOf(commitDifferential)
	if bulkAlloc >= differentialAlloc {
		t.Errorf("commitBulk entries bucket LeafAlloc (%d bytes) is not smaller than commitDifferential (%d bytes), want smaller", bulkAlloc, differentialAlloc)
	}
}

// TestStore_BulkFillPercent_SameContent: commitMode only changes how full
// bbolt packs its pages, never what is actually stored — both modes must
// read back identically.
func TestStore_BulkFillPercent_SameContent(t *testing.T) {
	changes := manyChanges(50)

	entriesFor := func(mode commitMode) []entry.Entry {
		root := t.TempDir()
		s, err := createStore(root)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.commit(changes, nil, cacheBlobs{}, mode); err != nil {
			t.Fatal(err)
		}
		defer closeStore(t, s)
		return allEntries(s)
	}

	bulkEntries := entriesFor(commitBulk)
	nonBulkEntries := entriesFor(commitDifferential)
	if len(bulkEntries) != len(nonBulkEntries) || len(bulkEntries) != 50 {
		t.Fatalf("got %d bulk / %d non-bulk entries, want 50 each", len(bulkEntries), len(nonBulkEntries))
	}
	byName := make(map[string]entry.Entry, len(nonBulkEntries))
	for _, e := range nonBulkEntries {
		byName[e.Name] = e
	}
	for _, e := range bulkEntries {
		other, ok := byName[e.Name]
		if !ok || other.Path != e.Path {
			t.Errorf("bulk entry %+v has no matching non-bulk entry", e)
		}
	}
}
