package scanner

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestParsePattern_MatchesGitCheckIgnore is directive 023 item 1's own
// completion condition: "git check-ignore -v gives the reference answer
// per path. Compare your parser against it over a real corpus, path by
// path, and keep the comparison as a test." Every candidate below is a
// real file or directory on disk, under a real .gitignore exercising
// every construct ParsePattern implements (bare and anchored patterns,
// directory-only patterns and the file/directory distinction, negation,
// wildcards, character classes, and all three globstar forms) — `git
// check-ignore -q` is asked about each one directly, and its exit code
// (0: ignored, 1: not ignored) is compared against scanner.included
// applied to the exact same rule set, parsed by the exact same
// ParsePattern this package uses for every other .gitignore file.
func TestParsePattern_MatchesGitCheckIgnore(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")

	gitignore := "target/\n" +
		"*.log\n" +
		"!important.log\n" +
		"/only-root.txt\n" +
		"sub/anchored.txt\n" +
		"**/anywhere.tmp\n" +
		"gen/**\n" +
		"a/**/b.txt\n" +
		"[dD]ebug/\n" +
		"*.o\n" +
		"!keep.o\n"
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(gitignore), 0o644); err != nil {
		t.Fatal(err)
	}

	type candidate struct {
		path  string
		isDir bool
	}
	candidates := []candidate{
		{"target", true},
		{"target/inner.txt", false},
		{"keep/target", true},
		{"app.log", false},
		{"important.log", false},
		{"sub/app.log", false},
		{"only-root.txt", false},
		{"nested/only-root.txt", false},
		{"sub/anchored.txt", false},
		{"other/sub/anchored.txt", false},
		{"x/anywhere.tmp", false},
		{"anywhere.tmp", false},
		{"gen/foo.go", false},
		{"gen", true},
		{"a/b.txt", false},
		{"a/x/b.txt", false},
		{"a/x/y/b.txt", false},
		{"a/c.txt", false},
		{"Debug", true},
		{"debug", true},
		{"Foobar", true},
		{"build.o", false},
		{"keep.o", false},
	}
	for _, c := range candidates {
		full := filepath.Join(dir, c.path)
		if c.isDir {
			if err := os.MkdirAll(full, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var rules []GitignoreRule
	for _, line := range strings.Split(gitignore, "\n") {
		p, ok := ParsePattern(line)
		if !ok {
			continue
		}
		rules = append(rules, GitignoreRule{dir: ".", pattern: p})
	}

	mismatches := 0
	for _, c := range candidates {
		cmd := exec.Command("git", "check-ignore", "-q", c.path)
		cmd.Dir = dir
		gitIgnored := cmd.Run() == nil

		ours := !included(c.path, rules, c.isDir)
		if ours != gitIgnored {
			mismatches++
			t.Errorf("%s (isDir=%v): git says ignored=%v, parser says ignored=%v", c.path, c.isDir, gitIgnored, ours)
		}
	}
	t.Logf("compared %d paths against git check-ignore, %d mismatches", len(candidates), mismatches)
}
