package gconfig

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDir_XDGThenHome(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/x")
	if got := Dir(); got != "/x/giraffe" {
		t.Fatalf("Dir() = %q", got)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/h")
	if got := Dir(); got != "/h/.config/giraffe" {
		t.Fatalf("Dir() = %q", got)
	}
}

func TestLoad_LogIsOnUnlessOff(t *testing.T) {
	dir := t.TempDir()
	if !Load(dir).Log || !Load("").Log {
		t.Fatal("log must default to on")
	}
	for text, want := range map[string]bool{"log = off\n": false, "# c\nlog=OFF # x\n": false, "log = on\n": true, "other = off\n": true} {
		if err := os.WriteFile(filepath.Join(dir, "config"), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := Load(dir).Log; got != want {
			t.Errorf("config %q: Log = %v, want %v", text, got, want)
		}
	}
}
