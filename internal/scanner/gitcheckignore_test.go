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
//
// Extended, directive 025 item 2: a second block below adds a nested
// repository ("vendor") with its own .gitignore, and compares
// included's boundary-aware answer against `git -C vendor check-ignore`
// — the reference for "a submodule is a gitignore boundary" (git's own
// rule, confirmed against a real submodule fixture in this directive's
// own report): the outer .gitignore's patterns must not reach inside
// it, and the reverse.
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
		rules = append(rules, GitignoreRule{dir: ".", pattern: p, fromGitignore: true})
	}

	mismatches := 0
	for _, c := range candidates {
		cmd := exec.Command("git", "check-ignore", "-q", c.path)
		cmd.Dir = dir
		gitIgnored := cmd.Run() == nil

		ours := !included(c.path, rules, c.isDir, nil)
		if ours != gitIgnored {
			mismatches++
			t.Errorf("%s (isDir=%v): git says ignored=%v, parser says ignored=%v", c.path, c.isDir, gitIgnored, ours)
		}
	}
	t.Logf("compared %d paths against git check-ignore, %d mismatches", len(candidates), mismatches)

	// A submodule is a gitignore boundary (directive 025): the outer
	// .gitignore above must not reach a path inside a nested repository,
	// and the nested repository's own .gitignore must not reach back
	// out. "vendor" is a plain `git init` repo with its own .gitignore —
	// HasGitDir does not care whether it is registered in .gitmodules,
	// so this is a faithful, cheaper stand-in for a real `git submodule
	// add` (the real thing is exercised in internal/index/enum_test.go's
	// own submodule fixtures; this test's own job is the gitignore-rule
	// comparison, not submodule registration).
	vendor := filepath.Join(dir, "vendor")
	if err := os.MkdirAll(vendor, 0o755); err != nil {
		t.Fatal(err)
	}
	runIn := func(cwd string, args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = cwd
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v (in %s): %v\n%s", args, cwd, err, out)
		}
	}
	runIn(vendor, "init", "-q")
	vendorGitignore := "*.md\n!keep.md\n"
	if err := os.WriteFile(filepath.Join(vendor, ".gitignore"), []byte(vendorGitignore), 0o644); err != nil {
		t.Fatal(err)
	}

	subCandidates := []candidate{
		{"README.md", false},       // vendor's own *.md — ignored by vendor's own rule
		{"keep.md", false},         // vendor's own negation of the above
		{"app.log", false},         // matches the OUTER *.log only — the boundary must block it
		{"target", true},           // matches the OUTER target/ only — the boundary must block it
		{"nested/deep.log", false}, // OUTER *.log, two directories deep inside vendor — still blocked
		{"plain.txt", false},       // matches neither — not ignored either way
	}
	for _, c := range subCandidates {
		full := filepath.Join(vendor, c.path)
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

	var vendorRules []GitignoreRule
	for _, line := range strings.Split(vendorGitignore, "\n") {
		p, ok := ParsePattern(line)
		if !ok {
			continue
		}
		vendorRules = append(vendorRules, GitignoreRule{dir: "vendor", pattern: p, fromGitignore: true})
	}
	allRules := append(append([]GitignoreRule{}, rules...), vendorRules...)
	boundaries := []string{"vendor"}

	subMismatches := 0
	for _, c := range subCandidates {
		full := filepath.Join("vendor", c.path)
		cmd := exec.Command("git", "check-ignore", "-q", c.path)
		cmd.Dir = vendor
		gitIgnored := cmd.Run() == nil

		ours := !included(full, allRules, c.isDir, boundaries)
		if ours != gitIgnored {
			subMismatches++
			t.Errorf("%s (isDir=%v, inside vendor): git -C vendor says ignored=%v, parser says ignored=%v", full, c.isDir, gitIgnored, ours)
		}
	}
	t.Logf("compared %d submodule-boundary paths against git -C vendor check-ignore, %d mismatches", len(subCandidates), subMismatches)
}
