package session

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestShownAndRemember (directive 046): a session sees its own record
// and nobody else's; nothing is remembered against no session; a
// record is the union of what was passed, without repeats.
func TestShownAndRemember(t *testing.T) {
	dir := t.TempDir()
	if got := Shown(dir, "s1"); got != nil {
		t.Fatalf("no record yet, Shown = %q", got)
	}
	if err := Remember(dir, "s1", []string{"a", "b", "a"}); err != nil {
		t.Fatal(err)
	}
	if got := Shown(dir, "s1"); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("Shown after Remember = %q, want [a b]", got)
	}
	if got := Shown(dir, "s2"); got != nil {
		t.Fatalf("another session must see nothing, got %q", got)
	}
	if err := Remember(dir, "s2", []string{"c"}); err != nil {
		t.Fatal(err)
	}
	if got := Shown(dir, "s1"); got != nil {
		t.Fatalf("s2's record replaced s1's; s1 must see nothing, got %q", got)
	}
	if got := Shown(dir, "s2"); !slices.Equal(got, []string{"c"}) {
		t.Fatalf("Shown for s2 = %q, want [c]", got)
	}
}

func TestNoSessionRemembersNothing(t *testing.T) {
	dir := t.TempDir()
	if err := Remember(dir, "", []string{"a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, filename)); !os.IsNotExist(err) {
		t.Fatalf("a record was written for an empty key (err=%v)", err)
	}
	if got := Shown(dir, ""); got != nil {
		t.Fatalf("Shown with no key = %q, want nil", got)
	}
}

// TestRememberIntoAMissingDirectoryIsAnError: the cache directory is
// the store's, created by the first refresh; a missing one is reported,
// not created here, so the record never outlives what it belongs to.
func TestRememberIntoAMissingDirectoryIsAnError(t *testing.T) {
	if err := Remember(filepath.Join(t.TempDir(), "absent"), "s1", []string{"a"}); err == nil {
		t.Fatal("expected an error writing into a missing directory")
	}
}

func TestKeyReadsTheEnvironment(t *testing.T) {
	t.Setenv(envKey, "abc")
	if Key() != "abc" {
		t.Fatalf("Key = %q", Key())
	}
	t.Setenv(envKey, "")
	if Key() != "" {
		t.Fatalf("Key with the variable empty = %q", Key())
	}
}
