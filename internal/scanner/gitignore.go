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
// Supported syntax, stated precisely (item 1's own requirement):
//   - blank lines and "#" comments are ignored
//   - a leading "!" negates, exactly as .giraffe/exclude already
//     supports (Pattern.negation)
//   - a pattern with no interior "/" matches at any depth under this
//     .gitignore's own directory (Pattern.excludes, extended by this
//     directive to match a whole path segment rather than only a
//     root-level prefix)
//   - a pattern with an interior "/", or a leading "/", is anchored to
//     this .gitignore's own directory (Pattern.excludes strips a
//     leading "/" for the actual comparison but still classifies the
//     pattern as anchored from it — "/build" and "sub/build" behave
//     the same as each other, and differently from bare "build")
//
// Not supported, named rather than handled silently:
//   - "**" (globstar) is matched as a literal by path.Match, not as
//     "zero or more path segments" the way real git interprets it
//   - "\"-escaping of a leading "!" or "#", or of a pattern containing
//     a literal space, is not recognised
func loadGitignore(absRoot, relDir string) []GitignoreRule {
	data, err := os.ReadFile(filepath.Join(absRoot, relDir, ".gitignore"))
	if err != nil {
		return nil
	}
	var rules []GitignoreRule
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		rules = append(rules, GitignoreRule{dir: relDir, pattern: Pattern(line)})
	}
	return rules
}
