package scanner

import (
	"os"
	"path/filepath"
	"strings"
)

// loadGitignore reads absRoot/relDir/.gitignore, if present, into
// GitignoreRules scoped to relDir (directive 022 item 1: ".gitignore
// is an exclusion source... regardless of whether .git exists"). A
// missing file returns nil, not an error — the ordinary case for most
// directories.
//
// Read fresh on every call this package makes, never gated by
// WalkCache's own directory-listing cache: editing a .gitignore file
// in place does not move its own directory's mtime (Walk's own
// documented miss, walk.go), so gating this read behind that cache
// would let a changed .gitignore go unnoticed until something else in
// the same directory also happened to change. The cost is one extra
// stat-and-maybe-read per directory per call — negligible next to a
// full directory listing, and never skipped for speed at correctness's
// expense.
//
// Parsed by ParsePattern (pattern.go) — the one complete gitignore-syntax
// parser directive 023 item 1 requires, shared verbatim with
// .giraffe/exclude (index.LoadExclude): globstar, escaping, anchoring,
// directory-only patterns, negation, and their precedence are all
// implemented there, not partially here. See ParsePattern's own doc
// comment for exactly what is and is not recognised.
//
// Every rule this produces is marked fromGitignore: true — a submodule
// is a gitignore boundary (directive 025), and included's own
// crossesBoundary check needs to tell a genuine .gitignore rule apart
// from defaultExclude/scope.Exclude, which are not git's concept and
// cross a submodule boundary freely by design.
func loadGitignore(absRoot, relDir string) []GitignoreRule {
	data, err := os.ReadFile(filepath.Join(absRoot, relDir, ".gitignore"))
	if err != nil {
		return nil
	}
	var rules []GitignoreRule
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSuffix(line, "\r") // tolerate a CRLF file
		p, ok := ParsePattern(line)
		if !ok {
			continue
		}
		rules = append(rules, GitignoreRule{dir: relDir, pattern: p, fromGitignore: true})
	}
	return rules
}
