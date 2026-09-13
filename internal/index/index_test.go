package index

import (
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
	if _, err := s.commit(changes, nil, nil, false); err != nil {
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
		if _, err := s.commit(changes, nil, []byte("enum-blob"), true); err != nil {
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
