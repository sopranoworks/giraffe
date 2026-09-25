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
	"regexp"
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
	// roots pairs each root directory with its own store, so a path an
	// argument names is resolved where repo-relative paths are relative
	// to (directive 062, h3).
	roots []rootSource
}

type rootSource struct {
	dir string
	src query.Entries
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
	// Anything that is not a subcommand or a flag is a search (directive
	// 062, h1): a caller told only that giraffe "searches this codebase"
	// runs `giraffe <what it wants>` — every such call in the measured
	// runs that did not first read --help — and until now got the help
	// on stderr and exit 2, and went back to grep. `giraffe find <word>`
	// is unchanged; `giraffe prepare` still prepares, so a search for the
	// word "prepare" or "find" is spelled `giraffe find prepare`.
	sub, rest := args[0], args[1:]
	if sub != "find" && sub != "prepare" {
		// A caller that reaches for giraffe the way it reaches for grep
		// writes grep's flags in front (-i, -r, -n): case is already
		// ignored and every file already searched, so they are dropped
		// rather than refused.
		anyWord := false
		for _, a := range args {
			if !strings.HasPrefix(a, "-") && len(wordsOf(a)) > 0 {
				anyWord = true
			}
		}
		if !anyWord {
			fmt.Fprint(os.Stderr, helpText)
			return 2
		}
		sub, rest = "find", args
	}

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
	roots := make([]rootSource, len(results))
	for i, r := range results {
		sources[i] = r.store
		stores[i] = r.store
		roots[i] = rootSource{dir: r.root, src: r.store}
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
		roots:             roots,
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
	args, paths := pathArgs(args, st.roots)
	words, queryRaw, page, err := parseArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "giraffe:", err)
		return 2
	}
	if len(words) == 0 && queryRaw == "" {
		fmt.Fprintln(os.Stderr, "usage: giraffe find <word>... [--query '<attr>:<value> ...'] [--page N] | giraffe find --query '<attr>:<value> ...' [--page N]; giraffe --help explains")
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
	if len(paths) > 0 {
		if filter == nil {
			filter = make(query.Filter)
		}
		filter["path"] = append(filter["path"], paths...)
	}

	summary := output.Summarize(st.entries.All(), st.commitLine, st.dispatched, st.warnings, st.ignoreWarnings, st.exclusions, st.knownLanguages)

	if len(words) == 0 {
		// Browsing a place (§31.3): no search word to rank against.
		matches := query.Browse(st.entries, filter, st.invocationProject)
		if len(matches) == 0 {
			summary.TryNext = query.DiagnoseFilter(st.entries, filter)
		}
		fmt.Print(output.FormatFrom(summary, matches, page))
		return 0
	}

	// Every word is its own search, answered in the one reply
	// (directive 062): a caller who already knows the three words it
	// will try pays for one index block, not three. Several words are
	// first ranked together (directive 065): the entries that carry two
	// or more of them, more and rarer ones first, with how many entries
	// each word matched on its own.
	answers := make([]output.WordAnswer, 0, len(words)+1)
	if len(words) > 1 {
		matches, counts := query.Ranked(st.entries, words, filter, st.invocationProject)
		answers = append(answers, output.WordAnswer{Word: strings.Join(words, " "), Words: words, Counts: counts, Matches: matches})
	}
	for _, word := range words {
		a := output.WordAnswer{Word: word, Words: []string{word}, Matches: query.Search(st.entries, word, filter, st.invocationProject)}
		if len(a.Matches) == 0 {
			if len(filter) > 0 {
				a.TryNext = append(a.TryNext, query.DiagnoseFilter(st.entries, filter)...)
			}
			a.TryNext = append(a.TryNext, query.Suggest(st.entries, word, st.invocationProject)...)
		}
		answers = append(answers, a)
	}
	// Where each result's name is called (directive 050, ported in
	// 065 g47) is attached to the entries a page will print, and no
	// other: a broad word ranks thousands, the page shows eight, and a
	// callers list is a store read each.
	for _, a := range answers {
		withCallers(a.Matches, page, st.entries)
	}
	summary.Sources = st.exactSources(words, answers)
	fmt.Print(output.FormatWords(summary, answers, page))
	return 0
}

// withCallers attaches where each result's name is called (directive
// 050) to the entries page will print — output.PageSize of them from
// the page's offset — and no other: a broad word ranks thousands, the
// page shows eight, and a callers list is a store read each (see
// query.Entries.Callers). The same enrichment-for-display exactSources
// does with a declaration's text, one step earlier.
func withCallers(matches []entry.Entry, page int, src query.Entries) {
	first := (page - 1) * output.PageSize
	for i := first; i < len(matches) && i < first+output.PageSize; i++ {
		matches[i] = query.WithCallers(matches[i], src)
	}
}

// maxSources caps how many exact matches ## source reads from disk: the
// section's byte budget bounds what prints, this bounds what is opened
// for it - a word that names a method on forty types should not cost
// forty file reads.
const maxSources = 3

// exactSources reads the text of the declarations whose name is exactly
// one of words (directive 065, g23; 044's ## source), rarer words'
// first and each word's in its own answer's rank order, up to
// maxSources: a caller handed a
// result's range reads the file anyway, and a name typed exactly is
// most often typed to see that declaration. A declaration whose file
// cannot be read is skipped - its result line still names the range.
func (st searchState) exactSources(words []string, answers []output.WordAnswer) []output.Source {
	var out []output.Source
	seen := make(map[string]bool)
	// Rarer words first: a word that is the name of one declaration
	// among a few says more about what was asked than one on thousands.
	own := make([]output.WordAnswer, 0, len(answers))
	for _, a := range answers {
		if len(a.Counts) == 0 { // not the several-word block
			own = append(own, a)
		}
	}
	sort.SliceStable(own, func(i, j int) bool { return len(own[i].Matches) < len(own[j].Matches) })
	for _, a := range own {
		for _, e := range a.Matches {
			if len(out) == maxSources {
				return out
			}
			if e.Kind == entry.KindFile || e.EndLine == 0 || !strings.EqualFold(e.Name, a.Word) {
				continue
			}
			k := e.Path + "\x00" + strconv.Itoa(e.Line)
			if seen[k] {
				continue
			}
			seen[k] = true
			if lines, ok := st.declarationLines(e); ok {
				out = append(out, output.Source{Entry: query.WithCallers(e, st.entries), Lines: lines})
			}
		}
	}
	return out
}

// declarationLines returns e's range from its file, under whichever
// root holds it.
func (st searchState) declarationLines(e entry.Entry) ([]string, bool) {
	for _, r := range st.roots {
		content, err := os.ReadFile(filepath.Join(r.dir, filepath.FromSlash(e.Path)))
		if err != nil {
			continue
		}
		lines := strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
		if e.Line < 1 || e.EndLine < e.Line || e.EndLine > len(lines) {
			return nil, false
		}
		return lines[e.Line-1 : e.EndLine], true
	}
	return nil, false
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
// see indexedAttributes. Directive 050 withdrew its last line, "Not
// indexed: call sites - grep for those": they are, and callers: says
// where. Directive 051 §1: test files are indexed too, and a callers:
// site now carries its own line, not just its place — round 9 (050's
// report) found the place alone still sent every agent to grep the
// name for exactly this text, so this says it is already there.
const helpText = `giraffe: an index of this repository's declarations (Go: types, funcs,
methods, struct fields, consts, vars) and files, tests included.

  giraffe [find] <word>...                     entries whose name, doc or path
                                               contains <word>, closest first;
                                               several: two or more first
  giraffe find <word> --query 'attr:value ...' the same, narrowed
  giraffe find --query 'attr:value ...'        browse a place: everything at it,
                                               in file order
  giraffe find ... --page N                    the next page of the same answer

A result's line:A-B is the declaration's whole extent: sed -n 'A,Bp' <path>
prints it. Under each result: its source line, the doc line the words
are on, and callers: where its name is called, likeliest first. --query
takes the attributes result lines print; path: and callers: match a
substring, the rest exactly. Bodies are not indexed.
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
func parseArgs(args []string) (words []string, queryRaw string, page int, err error) {
	page = 1
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--query":
			if i+1 >= len(args) {
				return nil, "", 0, fmt.Errorf("--query requires a value")
			}
			queryRaw = args[i+1]
			i++
		case args[i] == "--page":
			if i+1 >= len(args) {
				return nil, "", 0, fmt.Errorf("--page requires a value")
			}
			n, convErr := strconv.Atoi(args[i+1])
			if convErr != nil || n < 1 {
				return nil, "", 0, fmt.Errorf("--page must be a positive integer, got %q", args[i+1])
			}
			page = n
			i++
		case strings.HasPrefix(args[i], "-"):
			// A flag giraffe does not have — grep's -i, -r, -n, rg's
			// --type — is written by a caller who reached for giraffe the
			// way it reaches for grep (directive 062, h3). Case is already
			// ignored and every indexed file already searched, so it is
			// dropped rather than refused.
		default:
			words = append(words, wordsOf(args[i])...)
		}
	}
	return words, queryRaw, page, nil
}

// pathArgs takes out of args every positional argument that names a
// file or directory under a root, the way grep's operands do (directive
// 062, h3): `giraffe Send actor/mailbox.go` searches for Send in that
// file. They come back as repo-relative paths, for --query path:.
func pathArgs(args []string, roots []rootSource) (rest, paths []string) {
	cwd, _ := os.Getwd()
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--query" || a == "--page" {
			rest = append(rest, a)
			if i+1 < len(args) {
				rest = append(rest, args[i+1])
				i++
			}
			continue
		}
		if strings.HasPrefix(a, "-") || !strings.ContainsAny(a, "/.") {
			rest = append(rest, a)
			continue
		}
		found := false
		for _, r := range roots {
			abs := a
			if !filepath.IsAbs(abs) {
				abs = filepath.Join(cwd, a)
			}
			rel, err := filepath.Rel(r.dir, abs)
			if err != nil || strings.HasPrefix(rel, "..") {
				continue
			}
			if _, err := os.Stat(abs); err == nil {
				paths = append(paths, filepath.ToSlash(strings.TrimSuffix(rel, "/")))
				found = true
				break
			}
		}
		if !found {
			rest = append(rest, a)
		}
	}
	return rest, paths
}

// searchWord is what a search word is made of; anything else in an
// argument separates words.
var searchWord = regexp.MustCompile(`[\pL\pN_][\pL\pN_-]*`)

// wordsOf splits one argument into search words (directive 062, h3):
// "cluster metrics" quoted as one argument is two words, and so are
// grep's alternation "email\|mail" and a pattern "mail.*send" — a
// search for words, not a regular expression engine.
func wordsOf(arg string) []string {
	var out []string
	for _, w := range searchWord.FindAllString(arg, -1) {
		out = append(out, strings.TrimRight(w, "-"))
	}
	return out
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
