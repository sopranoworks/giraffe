package index

import (
	"os"
	"path/filepath"
	"testing"
)

// Directive 087 §8: a .giraffe/ giraffe creates ignores itself; one that
// was already there is left as it is.
func TestCreateStore_GitignoreOfANewGiraffeDir(t *testing.T) {
	fresh := t.TempDir()
	s, err := createStore(fresh)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	b, err := os.ReadFile(filepath.Join(fresh, Dir, ".gitignore"))
	if err != nil || string(b) != "*\n" {
		t.Fatalf(".gitignore = %q, %v", b, err)
	}
	existing := t.TempDir()
	if err := os.MkdirAll(filepath.Join(existing, Dir), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err = createStore(existing)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if _, err := os.Stat(filepath.Join(existing, Dir, ".gitignore")); err == nil {
		t.Fatal("a .giraffe/ that was already there got a .gitignore")
	}
}
