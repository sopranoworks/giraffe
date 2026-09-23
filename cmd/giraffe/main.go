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
	"strconv"
	"strings"

	"giraffe/internal/entry"
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
	// --help is the first thing every measured agent runs (041: every
	// aided transcript since 030), and until directive 042 it got one
	// usage line — nothing about what a result line carries, that
	// members are indexed, that a place can be browsed, or what --query
	// accepts. It answers with the help text now, on stdout, exit 0. A
	// bare `giraffe` gets the same text on stderr, exit 2: the command
	// was misused, but the reply is still the one that says how to use
	// it.
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, helpText)
		return 2
	}
	if isHelp(args) {
		fmt.Print(helpText)
		fmt.Print(indexedAttributes(ctx))
		return 0
	}
	// find and prepare are mandatory and never implied (§4): a bare word
	// is not treated as a search term, since both are themselves
	// plausible identifiers in real code.
	if args[0] != "find" && args[0] != "prepare" {
		fmt.Fprint(os.Stderr, helpText)
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
		exclude, err := index.LoadExclude(r)
		if err != nil {
			fmt.Fprintln(os.Stderr, "giraffe:", err)
			closeAll()
			return searchState{}, 1
		}
		assignments, err := index.LoadExtensions(r)
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
			exclusionSet[p.String()] = true
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

// runFind implements `giraffe find [<word>] [--query ...] [--page N]`.
// Two modes, both in the specification (§31.3): a word searches and ranks
// by relevance (query.Search); no word — only --query — browses a place,
// everything at it in path/name order (query.Browse). At least one of
// the two must be given, or there is nothing to search or browse.
// --page (directive 033, replacing 032's --offset) pages further into
// either mode's own ranking; it does not change what ranks where, only
// where the response budget starts reading from, and it is the same unit
// ## truncated now displays a position in.
//
// Zero results carries direction either way (directive 034): a filter
// value the index does not have anywhere is diagnosed first
// (query.DiagnoseFilter — the more specific fact when both a filter and
// a word are present, since it is about the corpus's own value set, not
// about this particular search), then a missed word's own near-name
// suggestions (query.Suggest, unchanged, word-only). Browsing a place
// always carries a filter (the caller below rejects word=="" with an
// empty filter too), so it now gets direction on a miss the same as a
// search does — before this directive it got none at all.
func runFind(args []string, st searchState) int {
	word, queryRaw, page, err := parseArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "giraffe:", err)
		return 2
	}
	if word == "" && queryRaw == "" {
		fmt.Fprintln(os.Stderr, "usage: giraffe find <word> [--query '<attr>:<value> ...'] [--page N] | giraffe find --query '<attr>:<value> ...' [--page N]; giraffe --help explains")
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

	var matches []entry.Entry
	if word == "" {
		// Browsing a place (§31.3): no search word to rank against.
		matches = query.Browse(st.entries, filter, st.invocationProject)
	} else {
		matches = query.Search(st.entries, word, filter, st.invocationProject)
	}

	if len(matches) == 0 {
		var tryNext []string
		if len(filter) > 0 {
			tryNext = append(tryNext, query.DiagnoseFilter(st.entries, filter)...)
		}
		if word != "" {
			tryNext = append(tryNext, query.Suggest(st.entries, word, st.invocationProject)...)
		}
		summary.TryNext = tryNext
	}
	fmt.Print(output.FormatFrom(summary, matches, page))
	return 0
}

// helpText is what --help, -h, help, and a bare or misused invocation
// print (directive 042, taking 041's text and adding the range). It is
// the tool's whole self-description to a caller that has never seen it
// — the measured agents read it once and then decided how to explore —
// so it says what is indexed, what a result line carries and what to
// do with the range, how a browse lists a place, how --query matches,
// and, as plainly, what is not indexed. It is also read once and then
// re-read on every later turn of that caller's, so it is bounded like a
// result page: 041's first version was 1,975 bytes and cost a 25-turn
// run some 20k tokens of re-reading; this one says the same in under
// 1,000 (TestHelpText_StaysSmall). It is static: the attributes and
// kinds actually present in this repository's index follow it, from
// the index itself (§6: "--help lists the four common attributes
// always, plus the language-specific attributes actually present"),
// see indexedAttributes.
const helpText = `giraffe: an index of this repository's declarations (Go: types, funcs,
methods, struct fields, consts, vars) and files, one entry each.

  giraffe find <word>                          entries whose name, doc line or
                                               path contains <word>, closest first
  giraffe find <word> --query 'attr:value ...' the same, narrowed
  giraffe find --query 'attr:value ...'        browse a place: everything at it,
                                               in file order
  giraffe find ... --page N                    the next page of the same answer

A result's line:A-B is the declaration's whole extent: sed -n 'A,Bp' <path>
prints exactly it. Under each result: its source line, then its doc's
first line - usually enough to judge it unopened. --query takes the
attribute names result lines print (receiver: and type: for members);
path: matches a substring, every other attribute exactly, no wildcards.
Not indexed: call sites and bodies - grep for those.
`

// isHelp reports whether args asks for help: --help, -h or help as the
// first argument, or --help anywhere after a subcommand (find --help).
func isHelp(args []string) bool {
	switch args[0] {
	case "--help", "-h", "help":
		return true
	}
	for _, a := range args[1:] {
		if a == "--help" || a == "-h" {
			return true
		}
	}
	return false
}

// indexedAttributes renders the part of --help that comes from this
// repository's own index (§6): the attribute names --query accepts
// here, and the kind values present. Outside a repository, or when the
// index cannot be built, it renders nothing — the static help already
// printed is still correct, and the failure is find's to report when a
// search is actually attempted, not help's.
func indexedAttributes(ctx context.Context) string {
	st, code := buildSearchState(ctx)
	if code != 0 {
		return ""
	}
	defer func() {
		for _, s := range st.stores {
			_ = s.Close()
		}
	}()
	attrs := make([]string, 0)
	for a := range query.ValidAttributes(st.entries) {
		attrs = append(attrs, a)
	}
	sort.Strings(attrs)
	kindSet := make(map[string]bool)
	for e := range st.entries.All() {
		kindSet[e.Kind] = true
	}
	kinds := make([]string, 0, len(kindSet))
	for k := range kindSet {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	return fmt.Sprintf("\nattributes in this index: %s\nkind values in this index: %s\n",
		strings.Join(attrs, " "), strings.Join(kinds, " "))
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

// parseArgs parses find's arguments: an optional positional word, an
// optional --query value, and an optional --page (directive 033,
// replacing 032's --offset: a count of entries to skip, a unit nothing
// giraffe displays, so a caller shown "353 more" had to invent the
// arithmetic to turn that into a number to type). --page N selects a
// page directly, 1-indexed — page defaults to 1, the top of the ranking,
// unchanged from before either flag existed, and matches the unit
// ## truncated's "page X of Y" now shows (output.FormatFrom).
func parseArgs(args []string) (word, queryRaw string, page int, err error) {
	page = 1
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--query":
			if i+1 >= len(args) {
				return "", "", 0, fmt.Errorf("--query requires a value")
			}
			queryRaw = args[i+1]
			i++
		case args[i] == "--page":
			if i+1 >= len(args) {
				return "", "", 0, fmt.Errorf("--page requires a value")
			}
			n, convErr := strconv.Atoi(args[i+1])
			if convErr != nil || n < 1 {
				return "", "", 0, fmt.Errorf("--page must be a positive integer, got %q", args[i+1])
			}
			page = n
			i++
		case word == "":
			word = args[i]
		default:
			return "", "", 0, fmt.Errorf("unexpected argument %q", args[i])
		}
	}
	return word, queryRaw, page, nil
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
