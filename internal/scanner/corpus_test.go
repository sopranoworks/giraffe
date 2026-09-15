//go:build corpus

package scanner

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestCorpus_MatchesGitCheckIgnore is directive 025 item 3's own
// measurement — the parser's own sufficiency checked against a real
// repository's full tree, not a handful of paths written for the test
// — restored here, behind a build tag, by directive 026 item 1: 025
// ran this once by hand and deleted it, but the same measurement is
// needed again every time the parser changes, and rebuilding it from a
// report's prose on each occasion is the repeated one-off D7 rules
// out.
//
// The build tag (see this file's first line) keeps it out of the
// ordinary build and out of `go test ./...` entirely: it depends on
// GIRAFFE_CORPUS_ROOT, an external checkout this repository does not
// carry and no CI environment can be assumed to have. Invoked as:
//
//	git clone --depth 1 https://github.com/kubernetes/kubernetes /path/to/kubernetes
//	GIRAFFE_CORPUS_ROOT=/path/to/kubernetes go test -tags corpus ./internal/scanner/ -run TestCorpus -v
//
// Compares included's own decision — built from every .gitignore file
// actually present in the corpus (fromGitignore rules only;
// defaultExclude and .giraffe/exclude are excluded from both sides:
// they are giraffe's own policy, not git's, and check-ignore knows
// nothing about them — 025's own diagnosis of its first, false-positive
// run, which used defaultExclude's "vendor/" against kubernetes' own
// vendored LICENSE files) — against `git check-ignore --no-index` for
// every file the corpus contains. --no-index rather than plain
// check-ignore: plain check-ignore never reports an already-tracked
// path as ignored (023's own finding) — the tracked/walked split's own
// already-solved concern, not a parser question; --no-index asks
// exactly the question included computes.
func TestCorpus_MatchesGitCheckIgnore(t *testing.T) {
	root := os.Getenv("GIRAFFE_CORPUS_ROOT")
	if root == "" {
		t.Skip("GIRAFFE_CORPUS_ROOT not set")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	var allRules []GitignoreRule
	var boundaries []string
	var allFiles []string
	gitignoreFiles := 0

	var walk func(absDir, relDir string) error
	walk = func(absDir, relDir string) error {
		if relDir != "." && HasGitDir(absDir) {
			boundaries = append(boundaries, relDir)
		}
		if _, err := os.Stat(filepath.Join(absDir, ".gitignore")); err == nil {
			gitignoreFiles++
		}
		allRules = append(allRules, loadGitignore(root, relDir)...)

		entries, err := os.ReadDir(absDir)
		if err != nil {
			return err
		}
		for _, e := range entries {
			name := e.Name()
			if name == ".git" {
				continue
			}
			absChild := filepath.Join(absDir, name)
			relChild := name
			if relDir != "." {
				relChild = relDir + "/" + name
			}
			if e.IsDir() {
				if err := walk(absChild, relChild); err != nil {
					return err
				}
				continue
			}
			allFiles = append(allFiles, relChild)
		}
		return nil
	}
	if err := walk(root, "."); err != nil {
		t.Fatal(err)
	}
	t.Logf("corpus root: %s", root)
	t.Logf("gitignore files discovered: %d", gitignoreFiles)

	// One batched `git check-ignore --stdin` call rather than one
	// subprocess per path (streamGitLines' own philosophy, scanner.go).
	// --non-matching plus -v guarantees exactly one output record per
	// input path, in input order (verified against a small fixture
	// before this was pointed at a real corpus) — the simplest way to
	// keep tens of thousands of answers lined up with their paths
	// without relying on default check-ignore's "print only the
	// matches" behaviour, which has no positional correspondence at
	// all.
	var input bytes.Buffer
	for _, f := range allFiles {
		input.WriteString(f)
		input.WriteByte(0)
	}
	cmd := exec.Command("git", "check-ignore", "--no-index", "--stdin", "-v", "-z", "--non-matching")
	cmd.Dir = root
	cmd.Stdin = &input
	out, err := cmd.Output()
	// check-ignore exits 1 whenever at least one path is not ignored —
	// an ordinary outcome here, not a failure; only a missing output
	// (a real invocation problem) is fatal.
	if err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			t.Fatalf("git check-ignore --stdin: %v", err)
		}
	}
	fields := bytes.Split(out, []byte{0})
	fields = fields[:len(fields)-1] // trailing empty split after the last NUL
	if len(fields)%4 != 0 {
		t.Fatalf("git check-ignore --stdin: %d fields, not a multiple of 4", len(fields))
	}
	if len(fields)/4 != len(allFiles) {
		t.Fatalf("git check-ignore --stdin: %d records for %d paths", len(fields)/4, len(allFiles))
	}

	mismatches := 0
	for i, f := range allFiles {
		source := fields[i*4]
		pattern := fields[i*4+2]
		gitIgnored := len(source) > 0 && (len(pattern) == 0 || pattern[0] != '!')

		ours := !included(f, allRules, false, boundaries)
		if ours != gitIgnored {
			mismatches++
			if mismatches <= 20 {
				t.Errorf("%s: git=%v ours=%v", f, gitIgnored, ours)
			}
		}
	}
	t.Logf("compared %d paths against git check-ignore --no-index, %d mismatches", len(allFiles), mismatches)
}
