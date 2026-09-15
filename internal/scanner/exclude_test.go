package scanner

import (
	"fmt"
	"testing"
)

// benchRulesAndBoundaries builds directive 026 item 3's own worst-case
// shape: outerLines fromGitignore rules at the scan root (dir ".") — a
// large outer .gitignore — plus one fromGitignore rule per submodule
// (dir "subN"), and submodules boundaries. Every dir "." rule is a
// candidate for crossesBoundary on every path anywhere in the tree
// (scopedTo never rejects it), so this is the multiplicative case
// item 3 names: paths × (outer) gitignore rules × boundaries. Shared by
// both benchmarks below so neither can silently drift from the other's
// rule set (code-manners M2).
func benchRulesAndBoundaries(outerLines, submodules int) ([]GitignoreRule, []string) {
	rules := make([]GitignoreRule, 0, outerLines+submodules)
	for i := 0; i < outerLines; i++ {
		rules = append(rules, GitignoreRule{dir: ".", pattern: mustParsePattern(fmt.Sprintf("*.ext%d", i)), fromGitignore: true})
	}
	boundaries := make([]string, 0, submodules)
	for i := 0; i < submodules; i++ {
		dir := fmt.Sprintf("sub%d", i)
		boundaries = append(boundaries, dir)
		rules = append(rules, GitignoreRule{dir: dir, pattern: mustParsePattern("*.log"), fromGitignore: true})
	}
	return rules, boundaries
}

func mustParsePattern(raw string) Pattern {
	p, ok := ParsePattern(raw)
	if !ok {
		panic("scanner: bad benchmark pattern: " + raw)
	}
	return p
}

// path is outside every submodule on purpose: a path strictly inside
// one (e.g. "sub25/...") lets crossesBoundary short-circuit the outer
// rules' own pattern match entirely once it finds the boundary it
// crossed (continue, before r.pattern.excludes ever runs) — cheaper
// than the boundaries-nil case, not a fair comparison of crossesBoundary's
// own added cost. Outside every boundary, crossesBoundary must scan the
// full boundary list and still return false for every one of the
// outerLines dir "." rules, and pattern matching still runs after it —
// exactly item 3's own worst case, paths × gitignore rules ×
// boundaries, with nothing skipped in either benchmark below.
const benchPath = "toplevel/deep/file.go"

// BenchmarkIncluded_SubmoduleHeavy measures included's own cost — directive
// 026 item 3's own question, unmeasured since 025 introduced
// crossesBoundary — at a scale of several tens of submodules and a
// twenty-line outer .gitignore.
func BenchmarkIncluded_SubmoduleHeavy(b *testing.B) {
	rules, boundaries := benchRulesAndBoundaries(20, 50)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		included(benchPath, rules, false, boundaries)
	}
}

// BenchmarkIncluded_NoSubmodules is the same rule count and the same
// path, with boundaries nil — item 3's own baseline, "against one with
// none": the marginal cost crossesBoundary itself adds is the gap
// between this and BenchmarkIncluded_SubmoduleHeavy above.
func BenchmarkIncluded_NoSubmodules(b *testing.B) {
	rules, _ := benchRulesAndBoundaries(20, 50)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		included(benchPath, rules, false, nil)
	}
}
