// Command giraffe is a search command, kept in the repository it indexes,
// that answers "does something like this already exist?" before new code
// is written (specifications/giraffe.md §1). This version implements only
// `giraffe find`.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"giraffe/internal/index"
	"giraffe/internal/lang"
	"giraffe/internal/limits"
	"giraffe/internal/output"
	"giraffe/internal/query"
	"giraffe/internal/scanner"
)

func main() {
	cfg, err := limits.FromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "giraffe:", err)
		os.Exit(2)
	}
	// The resource wrapper runs before anything else (§3): a failure from
	// here on is the command dying, never the host.
	ctx, cancel, err := limits.Enforce(context.Background(), cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "giraffe:", err)
		os.Exit(2)
	}

	done := make(chan struct{})
	go limits.Watch(ctx, done, cfg)

	code := run(ctx, os.Args[1:])
	close(done)
	cancel()
	os.Exit(code)
}

// rootResult is one project root's contribution to a `find` call: its own
// scan, its own local cache, its own freshness — merged with every
// other root's only after each is independently refreshed (§20 §2: one
// cache per root, not a shared one; §23 C-14 — that cache is never
// committed either).
type rootResult struct {
	root      string
	store     *index.Store
	stats     index.Stats
	commit    string
	exclusion []scanner.Pattern
}

// searchState is the outcome of resolving every root, refreshing its
// index, and merging the results — the work every subcommand needs
// before doing anything specific to itself. Building it once here keeps
// that shared setup in one place rather than copies that would drift
// (code-manners M1).
type searchState struct {
	entries           query.Entries  // §30 C-46: every root's store, merged; nothing decoded until Search/Suggest/Browse/Summarize actually range over it
	stores            []*index.Store // closed once this invocation is done querying — see run
	invocationProject string         // §21 C-6: home root's own project; unexported entries elsewhere are hidden unless a search runs from within them
	commitLine        string
	dispatched        int
	warnings          []string
	ignoreWarnings    []string
	exclusions        []string
	// knownLanguages names every language this invocation could possibly
	// report — every registered extractor's own name, plus every
	// operator assignment from every root's .giraffe/exclude — so
	// output.Summarize can give a registered-but-absent language
	// visibility (§25 C-24: "go 0, unknown 181099" for llvm-project).
	knownLanguages []string
}

func run(ctx context.Context, args []string) int {
	// find and prepare are mandatory and never implied (§4): a bare word
	// is not treated as a search term, since both are themselves
	// plausible identifiers in real code.
	if len(args) == 0 || (args[0] != "find" && args[0] != "prepare") {
		fmt.Fprintln(os.Stderr, "usage: giraffe find [<word>] [--query '<attr>:<value> ...'] | giraffe prepare")
		return 2
	}
	sub, rest := args[0], args[1:]

	st, code := buildSearchState(ctx)
	defer func() {
		for _, s := range st.stores {
			_ = s.Close()
		}
	}()
	if code != 0 {
		return code
	}

	switch sub {
	case "find":
		return runFind(rest, st)
	default: // "prepare", the only other value run reaches
		return runPrepare(rest, st)
	}
}

// buildSearchState resolves every configured root, refreshes each one's
// index, and merges them into the state every subcommand searches over.
func buildSearchState(ctx context.Context) (searchState, int) {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "giraffe:", err)
		return searchState{}, 1
	}
	homeRoot, err := scanner.Root(cwd)
	if err != nil {
		fmt.Fprintln(os.Stderr, "giraffe:", err)
		return searchState{}, 1
	}

	rootPaths, err := resolveRoots(homeRoot)
	if err != nil {
		fmt.Fprintln(os.Stderr, "giraffe:", err)
		return searchState{}, 1
	}

	var results []rootResult
	// closeAll releases every store opened so far — called on any error
	// return below, since a root failing partway through must not leak
	// the stores already opened for earlier roots. On success, ownership
	// of every store passes to the returned searchState instead (see
	// run's own defer), and closeAll is never called.
	closeAll := func() {
		for _, r := range results {
			_ = r.store.Close()
		}
	}
	var ignoreWarnings []string
	knownLanguageSet := make(map[string]bool)
	for _, l := range lang.RegisteredNames() {
		knownLanguageSet[l] = true
	}
	for _, r := range rootPaths {
		exclude, assignments, err := index.LoadExclude(r)
		if err != nil {
			fmt.Fprintln(os.Stderr, "giraffe:", err)
			closeAll()
			return searchState{}, 1
		}
		for _, name := range assignments {
			// An index-only assignment (directive 019 §4's "ext:<x>"
			// alone) carries no language name at all — its value is ""
			// by construction (index.parseAssignment), not a language
			// this build could report. Adding it here would seed a
			// phantom "" bucket in ## index's languages breakdown,
			// distinct from and invisible next to unknownLang.
			if name != "" {
				knownLanguageSet[name] = true
			}
		}
		scope := scanner.Scope{Exclude: exclude, Markers: lang.RegisteredMarkers()}

		store, stats, err := index.Refresh(ctx, r, scope, assignments)
		if err != nil {
			fmt.Fprintln(os.Stderr, "giraffe:", err)
			closeAll()
			// §21 C-8: a name collision is a configuration problem with
			// one clear fix, exit 2 — same treatment as the cross-root
			// collision below, whichever of the two catches it first.
			var collErr *scanner.CollisionError
			if errors.As(err, &collErr) {
				return searchState{}, 2
			}
			return searchState{}, 1
		}
		commit, err := commitHash(ctx, r)
		if err != nil {
			commit = "none"
		}
		results = append(results, rootResult{root: r, store: store, stats: stats, commit: commit, exclusion: scope.Exclude})

		// §23 C-14: the cache directory must never be committed — unlike
		// roots/exclude, which are hand-written and the operator's own
		// choice whether to commit, so they are not checked here. This is
		// not enforced by giraffe editing .gitignore (§17 I9 forbids a
		// tool silently doing that) — only reported if it is missing.
		cachePath := filepath.Join(index.Dir, "cache")
		if _, statErr := os.Stat(filepath.Join(r, cachePath)); statErr == nil && notIgnored(ctx, r, cachePath) {
			ignoreWarnings = append(ignoreWarnings,
				fmt.Sprintf("%s is not ignored by git in %s (add it to .gitignore yourself; giraffe will not)", cachePath, filepath.Base(r)))
		}
	}

	if err := checkNameCollisions(results); err != nil {
		fmt.Fprintln(os.Stderr, "giraffe:", err)
		closeAll()
		return searchState{}, 2
	}

	// §21 C-6: unexported results are hidden outside their own project.
	// The invocation's project is the home root's own — results[0],
	// since resolveRoots always puts it first.
	invocationProject := results[0].stats.RootProject

	var totalDispatched int
	var warnings []string
	var commitParts []string
	exclusionSet := make(map[string]bool)
	sources := make([]query.Entries, len(results))
	stores := make([]*index.Store, len(results))
	for i, r := range results {
		sources[i] = r.store
		stores[i] = r.store
		totalDispatched += r.stats.Dispatched
		warnings = append(warnings, r.stats.Warnings...)
		commitParts = append(commitParts, fmt.Sprintf("%s@%s", filepath.Base(r.root), r.commit))
		for _, p := range r.exclusion {
			exclusionSet[string(p)] = true
		}
	}
	exclusions := make([]string, 0, len(exclusionSet))
	for p := range exclusionSet {
		exclusions = append(exclusions, p)
	}

	// A single root keeps §7's original "commit <hash>" form exactly;
	// the "name@hash + name@hash" form only appears once there is
	// something to distinguish.
	commitLine := results[0].commit
	if len(results) > 1 {
		commitLine = strings.Join(commitParts, " + ")
	}

	knownLanguages := make([]string, 0, len(knownLanguageSet))
	for l := range knownLanguageSet {
		knownLanguages = append(knownLanguages, l)
	}
	sort.Strings(knownLanguages)

	return searchState{
		entries:           query.Concat(sources...),
		stores:            stores,
		invocationProject: invocationProject,
		commitLine:        commitLine,
		dispatched:        totalDispatched,
		warnings:          warnings,
		ignoreWarnings:    ignoreWarnings,
		exclusions:        exclusions,
		knownLanguages:    knownLanguages,
	}, 0
}

// runFind implements `giraffe find [<word>] [--query ...]`. Two modes,
// both in the specification (§31.3): a word searches and ranks by
// relevance (query.Search); no word — only --query — browses a place,
// everything at it in path/name order (query.Browse). At least one of
// the two must be given, or there is nothing to search or browse.
func runFind(args []string, st searchState) int {
	word, queryRaw, err := parseArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "giraffe:", err)
		return 2
	}
	if word == "" && queryRaw == "" {
		fmt.Fprintln(os.Stderr, "usage: giraffe find <word> [--query '<attr>:<value> ...'] | giraffe find --query '<attr>:<value> ...'")
		return 2
	}

	var filter query.Filter
	if queryRaw != "" {
		filter, err = query.Parse(queryRaw, query.ValidAttributes(st.entries))
		if err != nil {
			fmt.Fprintln(os.Stderr, "giraffe:", err)
			return 2
		}
	}

	summary := output.Summarize(st.entries.All(), st.commitLine, st.dispatched, st.warnings, st.ignoreWarnings, st.exclusions, st.knownLanguages)

	if word == "" {
		// Browsing a place (§31.3): no search word to rank against, so
		// there is nothing to suggest either — TryNext only makes sense
		// once a word has actually missed.
		matches := query.Browse(st.entries, filter, st.invocationProject)
		fmt.Print(output.Format(summary, matches))
		return 0
	}

	matches := query.Search(st.entries, word, filter, st.invocationProject)
	if len(matches) == 0 {
		summary.TryNext = query.Suggest(st.entries, word, st.invocationProject)
	}
	fmt.Print(output.Format(summary, matches))
	return 0
}

// runPrepare implements `giraffe prepare` (§4, §24 C-21). find already
// checks freshness and rebuilds what is stale before answering, so
// prepare has no bearing on correctness — buildSearchState already did
// prepare's whole job by the time run dispatches here. It exists only
// to move that cost out of the first search after a large pull, where
// kubernetes' measured 4.56s cold build (directive 005) makes it a
// real, if small, convenience. Reuses output.Format with no matches
// (code-manners D5) rather than a second rendering path: the evidence
// that the refresh happened is the same ## index line find prints.
func runPrepare(args []string, st searchState) int {
	if len(args) != 0 {
		fmt.Fprintln(os.Stderr, "usage: giraffe prepare")
		return 2
	}
	summary := output.Summarize(st.entries.All(), st.commitLine, st.dispatched, st.warnings, st.ignoreWarnings, st.exclusions, st.knownLanguages)
	fmt.Print(output.Format(summary, nil))
	return 0
}

// resolveRoots returns homeRoot plus every root listed in
// homeRoot/.giraffe/roots (directive 002 §2), each resolved to a
// canonical path and deduplicated.
func resolveRoots(homeRoot string) ([]string, error) {
	configured, err := index.LoadRoots(homeRoot)
	if err != nil {
		return nil, err
	}

	roots := []string{homeRoot}
	seen := map[string]bool{homeRoot: true}
	for _, p := range configured {
		resolved, err := resolveRootPath(homeRoot, p)
		if err != nil {
			return nil, fmt.Errorf(".giraffe/roots: %q: %w", p, err)
		}
		canon, err := scanner.Root(resolved)
		if err != nil {
			return nil, fmt.Errorf(".giraffe/roots: %q: %w", p, err)
		}
		if seen[canon] {
			continue
		}
		seen[canon] = true
		roots = append(roots, canon)
	}
	return roots, nil
}

func resolveRootPath(homeRoot, p string) (string, error) {
	if strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, p[2:])
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(homeRoot, p)
	}
	return p, nil
}

// checkNameCollisions errors, naming both paths, if two configured roots
// would produce the same project name. Uses the same detection as the
// intra-tree case in index.Refresh (§21 C-8: same failure, one check) —
// scanner.CheckNameCollisions over each root's own derived project name
// (its root marker, or its basename), not just the directory's basename,
// so this agrees with what a nested-marker root would actually report as
// its own project.
func checkNameCollisions(results []rootResult) error {
	names := make(map[string]string, len(results))
	for _, r := range results {
		names[r.root] = r.stats.RootProject
	}
	if err := scanner.CheckNameCollisions(names); err != nil {
		return fmt.Errorf("%w (rename one, or remove it from .giraffe/roots)", err)
	}
	return nil
}

func notIgnored(ctx context.Context, root, relPath string) bool {
	cmd := exec.CommandContext(ctx, "git", "check-ignore", "-q", relPath)
	cmd.Dir = root
	err := cmd.Run()
	if err == nil {
		return false // ignored
	}
	if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
		return true // explicitly not ignored
	}
	return false // some other failure (e.g. git itself errored); do not report a false positive
}

func parseArgs(args []string) (word, queryRaw string, err error) {
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--query":
			if i+1 >= len(args) {
				return "", "", fmt.Errorf("--query requires a value")
			}
			queryRaw = args[i+1]
			i++
		case word == "":
			word = args[i]
		default:
			return "", "", fmt.Errorf("unexpected argument %q", args[i])
		}
	}
	return word, queryRaw, nil
}

func commitHash(ctx context.Context, root string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--short", "HEAD")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
