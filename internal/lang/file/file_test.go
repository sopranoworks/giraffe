package file

import (
	"testing"

	"github.com/sopranoworks/giraffe/internal/entry"
	"github.com/sopranoworks/giraffe/internal/scanner"
)

// TestExtract_AlwaysHandlesAndNeverReadsContent (directive 016 §1,
// directive 017 §1): the generic module always claims the file — and,
// since it never calls f.Content(), a path that does not exist on disk
// still succeeds without error, proving no read was attempted.
func TestExtract_AlwaysHandlesAndNeverReadsContent(t *testing.T) {
	f := scanner.Open(t.TempDir(), scanner.FileMeta{Path: "does-not-exist.xyz"})
	entries, _, handled, err := Extract(f, "giraffe", nil)
	if !handled || err != nil {
		t.Fatalf("Extract = %v, %v, %v, want handled=true, err=nil", entries, handled, err)
	}
	if len(entries) != 1 || entries[0].Kind != entry.KindFile || entries[0].Name != "does-not-exist" {
		t.Errorf("entries = %v, want exactly one file entry named does-not-exist", entries)
	}
}
