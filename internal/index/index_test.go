package index

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"giraffe/internal/entry"
)

func readCacheFile(root string) (string, error) {
	b, err := os.ReadFile(filepath.Join(root, Dir, cacheDir, cacheFilename))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func TestSaveLoad_RoundTrip(t *testing.T) {
	root := t.TempDir()
	records := map[string]Record{
		"internal/retry/retry.go": {
			ModTime: 111, Size: 222,
			Entries: []entry.Entry{
				{
					Path: "internal/retry/retry.go", Name: "Retry", Kind: "func",
					Project: "giraffe", Scope: "exported", Exported: true, Doc: "Retry retries with backoff.",
					Position: []entry.Position{{Name: "module", Value: "internal/retry"}},
				},
				{
					Path: "internal/retry/retry.go", Name: "policyImpl", Kind: "type",
					Project: "giraffe", Scope: "package", Exported: false, Doc: "", // undocumented
					Position: []entry.Position{{Name: "module", Value: "internal/retry"}},
				},
			},
		},
		"main.go": {
			ModTime: 333, Size: 444,
			Entries: []entry.Entry{
				{Path: "main.go", Name: "main", Kind: "func", Project: "giraffe"},
			},
		},
	}

	if err := Save(root, records); err != nil {
		t.Fatal(err)
	}
	got, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}

	for path, want := range records {
		rec, ok := got[path]
		if !ok {
			t.Fatalf("missing record for %s", path)
		}
		if rec.ModTime != want.ModTime || rec.Size != want.Size {
			t.Errorf("%s: freshness = %+v, want ModTime=%d Size=%d", path, rec, want.ModTime, want.Size)
		}
		sortByName(rec.Entries)
		sortByName(want.Entries)
		if !reflect.DeepEqual(rec.Entries, want.Entries) {
			t.Errorf("%s: entries = %#v, want %#v", path, rec.Entries, want.Entries)
		}
	}
}

func TestSaveLoad_ZeroEntryFileStillAnchorsFreshness(t *testing.T) {
	// A tracked file with zero top-level declarations at all (since §21
	// C-6, that is the only remaining zero-entry case — unexported ones
	// are indexed too) produces no entries. It must still round-trip a
	// presence line carrying its freshness fingerprint — otherwise
	// Refresh has nothing to compare against next run and re-reads it
	// every single time (found against gekka).
	root := t.TempDir()
	records := map[string]Record{
		"internal/priv/priv.go": {ModTime: 555, Size: 0, Entries: nil},
	}
	if err := Save(root, records); err != nil {
		t.Fatal(err)
	}
	got, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	rec, ok := got["internal/priv/priv.go"]
	if !ok {
		t.Fatal("expected a stored record for a zero-entry file")
	}
	if rec.ModTime != 555 {
		t.Errorf("ModTime = %d, want 555", rec.ModTime)
	}
	if len(rec.Entries) != 0 {
		t.Errorf("expected zero entries, got %v", rec.Entries)
	}
}

// TestLoad_UnreadableShapeDiscardsWholeCache (§23 C-14): before this
// directive, an undecodable line (a previous format version, or
// corruption) was skipped and its one file re-extracted — self-healing
// made sense because the index was shared and a format bump could not
// hard-break every clone's committed copy. Nothing depends on this
// cache but the invocation that wrote it, so the cheaper and simpler
// rule is: anything that does not parse discards the whole cache, not
// just its one line.
func TestLoad_UnreadableShapeDiscardsWholeCache(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, Dir, cacheDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// One line in a shape this build does not recognise (9 fields — the
	// pre-005 committed-index shape, missing this version's mtime/size
	// columns), one otherwise-valid current-format line.
	stale := "old.go\th1\tfunc\tOld\tgiraffe\texported\t1\t\tdoc\n"
	current := encodeLine(entry.Entry{Path: "new.go", Name: "New", Kind: "func", Project: "giraffe", Exported: true}, 1, 2) + "\n"
	if err := os.WriteFile(filepath.Join(dir, cacheFilename), []byte(stale+current), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := Load(root)
	if err != nil {
		t.Fatalf("an unreadable cache shape is discarded, not a command failure: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected the whole cache discarded once one line fails to decode, got %v", got)
	}
}

func TestLoad_MissingCacheIsNotAnError(t *testing.T) {
	root := t.TempDir()
	got, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty map for a missing cache, got %v", got)
	}
}

func TestSave_IsDeterministic(t *testing.T) {
	root1, root2 := t.TempDir(), t.TempDir()
	records := map[string]Record{
		"b.go": {ModTime: 2, Size: 20, Entries: []entry.Entry{{Path: "b.go", Name: "B", Kind: "func"}}},
		"a.go": {ModTime: 1, Size: 10, Entries: []entry.Entry{{Path: "a.go", Name: "A", Kind: "func"}}},
	}
	if err := Save(root1, records); err != nil {
		t.Fatal(err)
	}
	if err := Save(root2, records); err != nil {
		t.Fatal(err)
	}
	b1, err := readCacheFile(root1)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := readCacheFile(root2)
	if err != nil {
		t.Fatal(err)
	}
	if b1 != b2 {
		t.Fatalf("Save is not deterministic:\n%s\n---\n%s", b1, b2)
	}
}

func sortByName(entries []entry.Entry) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
}
