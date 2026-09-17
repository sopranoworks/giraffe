package scanner

import "strings"

// defaultExclude is the baseline every project starts with: dependency and
// build-output trees a reuse search gets nothing from. It is the same set
// specifications/giraffe.md §11 excludes from its sizing measurement, kept
// here so a real run stays comparable to that evidence. Per-project
// configuration adds to this; it does not replace it. Written in
// gitignore syntax (directive 023 item 1: one syntax, one parser) —
// parsed by defaultExcludeRules below exactly as any .gitignore or
// .giraffe/exclude line is.
var defaultExclude = []string{
	// giraffe's own local cache and configuration (index.Dir — repeated
	// as a literal here, not imported, since index already imports this
	// package). A repository with no .gitignore entry for it — every
	// test repo in this codebase, and plausibly some real ones — would
	// otherwise have its own store file picked up by the untracked-file
	// scan the moment the store is created, indexing giraffe's cache as
	// a search target of itself. Directive 013 found this: its own
	// fix — a newly-appeared untracked file is no longer silently
	// dropped by the fast path (§20 C-2, extended) — is what turned this
	// from a latent gap into an observed one.
	".giraffe/",
	"vendor/",
	"node_modules/",
	"testdata/",
	"examples/",
	"*.pb.go",
}

// "*_test.go" stood here through directive 050: a reuse search gets
// nothing from a generated file, but a test file is not that — it holds
// real declarations and, since directive 050, real call sites, and 051
// found the gap it left was load-bearing (§28 C-39: two of the task's
// landing points and every test of Snapshot were both unreachable
// because the files they live in were never scanned). Removed in
// directive 051 §1; a Go test file is now an ordinary candidate, marked
// by the golang module (entry.Entry.Test, golang.isTestFile) rather
// than hidden by the scanner.

// GitignoreRule is one exclusion rule discovered from a .gitignore
// file, scoped to the directory that declared it (directive 022 item
// 1: nested .gitignore files apply to their own subtree). Opaque
// outside this package — index/enum.go holds the slice Walk returns
// only to hand it back to FilterWalked, never inspecting it — the same
// shape defaultExclude and Scope.Exclude are normalised into
// internally (dir "."), so pruning and filtering share one algorithm
// (included, below) regardless of where a rule came from.
type GitignoreRule struct {
	dir     string
	pattern Pattern

	// fromGitignore is true only for a rule loadGitignore produced —
	// this is what makes it a rule that must respect a submodule
	// boundary (directive 025: "a submodule is a separate repository...
	// the outer repository's .gitignore does not apply inside it").
	// defaultExclude and scope.Exclude rules leave this false: they are
	// giraffe's own policy, not git's, and apply uniformly regardless of
	// any submodule crossing — the same way baseRules (StreamTracked)
	// already applies them fresh, unscoped by any boundary, inside
	// every submodule runTrackedTree recurses into.
	fromGitignore bool
}

// scopedTo reports the part of relPath below dir, and whether relPath
// is under dir at all — "." matches everything (defaultExclude and
// Scope.Exclude are always scan-root-relative), any other dir requires
// relPath to be dir itself or begin with "dir/". A gitignore rule only
// ever applies within the subtree of the .gitignore file that declared
// it; this is the one place that boundary is enforced.
func scopedTo(relPath, dir string) (suffix string, ok bool) {
	if dir == "." {
		return relPath, true
	}
	if relPath == dir {
		return "", true
	}
	if strings.HasPrefix(relPath, dir+"/") {
		return relPath[len(dir)+1:], true
	}
	return "", false
}

func defaultExcludeRules() []GitignoreRule {
	rules := make([]GitignoreRule, 0, len(defaultExclude))
	for _, raw := range defaultExclude {
		p, ok := ParsePattern(raw)
		if !ok {
			// Every entry in defaultExclude is a fixed literal, known
			// valid gitignore syntax — this can only fire if a future
			// edit to that list breaks it, which a test catches
			// (TestDefaultExclude_AllPatternsParse) well before this
			// would ever run against a real tree.
			panic("scanner: defaultExclude entry does not parse: " + raw)
		}
		rules = append(rules, GitignoreRule{dir: ".", pattern: p})
	}
	return rules
}

func scopeExcludeRules(scope Scope) []GitignoreRule {
	rules := make([]GitignoreRule, 0, len(scope.Exclude))
	for _, p := range scope.Exclude {
		rules = append(rules, GitignoreRule{dir: ".", pattern: p})
	}
	return rules
}

// baseRules is defaultExclude plus scope.Exclude, both scan-root-
// relative — the tracked half's whole rule set (StreamTracked below):
// .gitignore is deliberately not part of it. Real git does not
// retroactively drop a file from `ls-files -s` because it was later
// added to .gitignore (a common, intentional case — a file tracked
// before a build-output pattern existed), so the tracked half must
// not either; only the walked half's rule set (walkedRules) adds
// .gitignore on top of this same base.
func baseRules(scope Scope) []GitignoreRule {
	rules := defaultExcludeRules()
	return append(rules, scopeExcludeRules(scope)...)
}

// walkedRules is the walked half's full rule set, in override order:
// defaultExclude first, then whatever .gitignore contributed (item
// 1) — Walk discovers deeper .gitignore files later, so a nested rule
// naturally overrides a shallower one the same way real git layers
// them — and scope.Exclude last, so an operator's own
// .giraffe/exclude keeps the final say it already had before this
// directive.
func walkedRules(scope Scope, gitignore []GitignoreRule) []GitignoreRule {
	rules := defaultExcludeRules()
	rules = append(rules, gitignore...)
	return append(rules, scopeExcludeRules(scope)...)
}

// included reports whether relPath survives scanning: everything is a
// candidate by default (directive 017 §2 — the scanner no longer has a
// coverage default keyed on extension or filename shape; that decision
// moved to the exclusion module, first in giraffe/internal/lang's
// chain) except what an exclusion rule (default, project-level, or —
// for the walked half only — .gitignore, §19, directive 022 item 1)
// removes. A negated pattern wins over a plain one, so an operator
// can pull a specific file back in without having to fight the
// built-in patterns. rules is checked in order, each scoped to its own
// dir (GitignoreRule's own doc comment) — this is the one algorithm
// both file-level filtering and directory-level pruning (walk.go's
// prunes) share. isDir is relPath's own kind — true only from prunes,
// which is only ever asked about a directory it is deciding whether to
// descend into; false from StreamTracked and FilterWalked, which only
// ever hand this a file (Pattern.excludes' own doc comment on why a
// dirOnly rule needs to know).
//
// boundaries is every submodule root Walk found on the way to relPath
// (walk.go's own accumulator) — a .gitignore-sourced rule (fromGitignore)
// declared at or above one of them is skipped for any relPath strictly
// inside it (crossesBoundary, below): a submodule is a gitignore
// boundary (directive 025), so an outer or sibling-submodule .gitignore
// must not reach into it, the same way git's own check-ignore, run
// inside a submodule, consults only that submodule's own rules. A rule
// not sourced from .gitignore (defaultExclude, scope.Exclude) is never
// checked against boundaries — see fromGitignore's own doc comment for
// why. nil is a valid, common boundaries value: StreamTracked's own
// rules (baseRules) never include a fromGitignore rule, so the check
// never fires there regardless.
func included(relPath string, rules []GitignoreRule, isDir bool, boundaries []string) bool {
	keep := true
	for _, r := range rules {
		suffix, ok := scopedTo(relPath, r.dir)
		if !ok {
			continue
		}
		if r.fromGitignore && crossesBoundary(r.dir, relPath, boundaries) {
			continue
		}
		if neg, plain := r.pattern.negation(); neg {
			if plain.excludes(suffix, isDir) {
				keep = true
			}
			continue
		}
		if r.pattern.excludes(suffix, isDir) {
			keep = false
		}
	}
	return keep
}

// crossesBoundary reports whether some submodule root in boundaries lies
// strictly between dir (a rule's own declaring directory) and relPath —
// meaning the rule would have to reach across a submodule's own
// repository boundary to apply to relPath. dir itself, and any boundary
// at or above dir, is never "between": a submodule's own .gitignore
// (dir equal to the boundary) still governs its own subtree, exactly as
// before this check existed. Only a boundary strictly below dir, that
// relPath is strictly inside, blocks the rule — relPath equal to the
// boundary itself (the submodule's own path, seen from outside it) is
// still a plain path in the declaring repository's own namespace, not
// yet across the boundary (real git still applies the outer .gitignore
// to decide whether the gitlink's own name matches; it just cannot see
// inside it).
func crossesBoundary(dir, relPath string, boundaries []string) bool {
	for _, b := range boundaries {
		if !isStrictlyUnder(b, dir) {
			continue
		}
		if isStrictlyUnder(relPath, b) {
			return true
		}
	}
	return false
}

// isStrictlyUnder reports whether path is strictly inside dir — dir
// itself does not count, matching crossesBoundary's own use (a
// boundary equal to dir, or relPath equal to a boundary, is not
// "strictly under" anything). dir "." is the scan root: every non-"."
// path is strictly under it.
func isStrictlyUnder(path, dir string) bool {
	if dir == "." {
		return path != "."
	}
	return strings.HasPrefix(path, dir+"/")
}
