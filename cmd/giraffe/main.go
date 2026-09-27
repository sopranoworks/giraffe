// Command giraffe is a search command, kept in the repository it indexes,
// that answers "does something like this already exist?" before new code
// is written (specifications/giraffe.md §1). Its grammar is run's.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"

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
	entries        query.Entries  // §30 C-46: every root's store, merged; nothing decoded until Search/Suggest/Browse/Summarize actually range over it
	stores         []*index.Store // closed once this invocation is done querying — see run
	commitLine     string
	dispatched     int
	warnings       []string
	ignoreWarnings []string
	exclusions     []string
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

// options are what giraffe itself is given, before the subcommand
// (directive 071): --query's value, --page's number, --help. One rule
// decides every argument: what comes before the subcommand is
// giraffe's; everything after find is the search.
type options struct {
	query    string
	hasQuery bool
	page     int
	help     bool
}

// parseOptions reads giraffe's own options off the front of args and
// returns the rest, whose first element is where the subcommand
// belongs. An option that takes a value takes the next argument
// verbatim, whatever it starts with: nothing inside a value is
// interpreted. Anything that is not one of the listed options ends the
// options — it is the subcommand, or a mistake run reports.
func parseOptions(args []string) (options, []string, error) {
	o := options{page: 1}
	i := 0
	for ; i < len(args); i++ {
		switch args[i] {
		case "--query":
			if i+1 >= len(args) {
				return o, nil, fmt.Errorf("--query takes a value: giraffe --query 'attr:value ...' find <word>...")
			}
			if o.hasQuery {
				return o, nil, fmt.Errorf("--query given twice; one value carries every attribute: giraffe --query 'attr:value attr:value' find <word>...")
			}
			o.query, o.hasQuery = args[i+1], true
			i++
		case "--page":
			if i+1 >= len(args) {
				return o, nil, fmt.Errorf("--page takes a number: giraffe --page N find <word>...")
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil || n < 1 {
				return o, nil, fmt.Errorf("--page must be a positive integer, got %q", args[i+1])
			}
			o.page = n
			i++
		case "--help":
			o.help = true
		default:
			return o, args[i:], nil
		}
	}
	return o, nil, nil
}

// run is the grammar (directive 071), whole:
//
//	giraffe [options] find <word>...
//	giraffe prepare
//	giraffe --help
//	giraffe
//
// Every argument after find is a search word exactly as given — find,
// prepare, --help, --query, --show-kind, -rn: nothing after find is an
// option, dropped or interpreted. prepare builds or refreshes the
// index. --help and no arguments print the usage, exit 0, and touch no
// index. Anything else where the subcommand belongs gets one line
// naming the exact command to run instead, exit 2, and nothing is
// searched or guessed — 062's bare search, its silent dropping of
// -prefixed arguments (h3) and its path operands are gone with it.
func run(ctx context.Context, args []string) int {
	opts, rest, err := parseOptions(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "giraffe:", err)
		return 2
	}
	if opts.help || len(args) == 0 {
		fmt.Print(helpText)
		return 0
	}
	if len(rest) == 0 {
		fmt.Fprintln(os.Stderr, "giraffe: nothing after the options; run: "+commandLine(opts, "find", []string{"<word>..."}))
		return 2
	}
	switch rest[0] {
	case "find":
		words := rest[1:]
		if len(words) == 0 && !opts.hasQuery {
			fmt.Fprintln(os.Stderr, "usage: giraffe [--query 'attr:value ...'] [--page N] find <word>... | giraffe --query 'attr:value ...' [--page N] find; giraffe --help explains")
			return 2
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
		return runFind(words, opts, st)
	case "prepare":
		if len(rest) > 1 || opts.hasQuery || opts.page != 1 {
			fmt.Fprintln(os.Stderr, "giraffe: prepare takes no options and no arguments; run: giraffe prepare")
			return 2
		}
		return runPrepare(ctx)
	}
	fmt.Fprintln(os.Stderr, "giraffe: no bare search; run: "+commandLine(opts, "find", rest))
	return 2
}

// commandLine renders a command in the grammar from the options as
// given, a subcommand, and the arguments after it — what the redirect
// line names, so it can be pasted as it stands.
func commandLine(o options, sub string, rest []string) string {
	parts := []string{"giraffe"}
	if o.hasQuery {
		parts = append(parts, "--query", output.ShellWord(o.query))
	}
	if o.page != 1 {
		parts = append(parts, "--page", strconv.Itoa(o.page))
	}
	parts = append(parts, sub)
	for _, a := range rest {
		parts = append(parts, output.ShellWord(a))
	}
	return strings.Join(parts, " ")
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

	var totalDispatched int
	var warnings []string
	var commitParts []string
	exclusionSet := make(map[string]bool)
	sources := make([]query.Entries, len(results))
	stores := make([]*index.Store, len(results))
	roots := make([]rootSource, len(results))
	for i, r := range results {
		// The home root is results[0] (resolveRoots puts it first) and is
		// searched whole; every other root is presented as foreign, its
		// unexported declarations hidden (§21 C-6, narrowed by directive
		// 072 to other roots).
		sources[i] = r.store
		if i > 0 {
			sources[i] = query.Foreign(r.store)
		}
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
		entries:        query.Concat(sources...),
		stores:         stores,
		commitLine:     commitLine,
		dispatched:     totalDispatched,
		warnings:       warnings,
		ignoreWarnings: ignoreWarnings,
		exclusions:     exclusions,
		knownLanguages: knownLanguages,
		roots:          roots,
	}, 0
}

// runFind implements `giraffe [--query ...] [--page N] find [<word>...]`.
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
func runFind(words []string, opts options, st searchState) int {
	queryRaw, page := opts.query, opts.page
	var err error
	var filter query.Filter
	if queryRaw != "" {
		filter, err = query.Parse(queryRaw, query.ValidAttributes(st.entries))
		if err != nil {
			fmt.Fprintln(os.Stderr, "giraffe:", err)
			return 2
		}
	}

	summary := output.Summarize(st.entries.All(), st.commitLine, st.dispatched, st.warnings, st.ignoreWarnings, st.exclusions, st.knownLanguages)
	// Every command the answer prints to be typed back — the next page,
	// a narrowing — is this call in the grammar (directive 071).
	summary.Call = output.Call{Query: queryRaw, Words: words}

	if len(words) == 0 {
		// Browsing a place (§31.3): no search word to rank against.
		matches := query.Browse(st.entries, filter)
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
		matches, counts := query.Ranked(st.entries, words, filter)
		answers = append(answers, output.WordAnswer{Word: strings.Join(words, " "), Words: words, Counts: counts, Matches: matches})
		// Directive 073: a word of the call that names a directory —
		// a package named for its subject — is where a reader narrows
		// to next, and 073's runs did in their second call. That
		// directory's block follows the several-words block, as
		// --query path: to it would print it, showing what the wider
		// block did not.
		if dir, n := wordDir(matches, words, filter); n >= wordDirMin {
			near := query.Filter{}
			for k, v := range filter {
				near[k] = v
			}
			near["path"] = []string{dir}
			inDir, _ := query.Ranked(st.entries, words, near)
			answers = append(answers, output.WordAnswer{Word: "in " + dir, Words: words, Matches: inDir})
		}
	}
	for _, word := range words {
		a := output.WordAnswer{Word: word, Words: []string{word}, Matches: query.Search(st.entries, word, filter)}
		if (len(a.Matches) == 0 || !nameLike(word)) && len(filter) == 0 {
			a.Text, a.TextFiles, a.TextSearched = st.textSites(word)
		}
		if len(a.Matches) == 0 {
			if len(filter) > 0 {
				a.TryNext = append(a.TryNext, query.DiagnoseFilter(st.entries, filter)...)
			}
			a.TryNext = append(a.TryNext, query.Suggest(st.entries, word)...)
		}
		answers = append(answers, a)
	}
	// Where each result's name is called (directive 050, ported in
	// 065 g47) is attached to the entries a page will print, and no
	// other: a broad word ranks thousands, the page shows eight, and a
	// callers list is a store read each.
	for i := range answers {
		answers[i].Matches = query.CodeFirst(answers[i].Matches)
	}
	for _, a := range answers {
		withCallers(a.Matches, page, st.entries)
	}
	withMembers(answers, page, st.entries)
	summary.Sources = st.exactSources(words, answers)
	fmt.Print(output.FormatWords(summary, answers, page))
	return 0
}

// withCallers attaches where each result's name is called (directive
// 050) to the entries page can print — the first page*output.PageSize
// of them, since a page holds at most PageSize and starts where the
// one before it ended (directive 072; output.fitShare) — and no other:
// a broad word ranks thousands, a page shows eight, and a callers list
// is a store read each (see query.Entries.Callers). Enriching every
// entry up to this page, not only this page's window, is what lets the
// pages before it be fitted again exactly as they printed. The same
// enrichment-for-display exactSources does with a declaration's text,
// one step earlier.
func withCallers(matches []entry.Entry, page int, src query.Entries) {
	for i := 0; i < len(matches) && i < page*output.PageSize; i++ {
		matches[i] = query.WithCallers(matches[i], src)
	}
}

// withMembers attaches a type's members (directive 073) to the entries
// page can print, in every answer, with one pass over the index; the
// same window withCallers enriches.
func withMembers(answers []output.WordAnswer, page int, src query.Entries) {
	var targets []*entry.Entry
	for _, a := range answers {
		for i := 0; i < len(a.Matches) && i < page*output.PageSize; i++ {
			targets = append(targets, &a.Matches[i])
		}
	}
	query.WithMembers(targets, src)
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

// nameLike reports whether word could be a declaration's name: letters,
// digits and underscores only. A word with anything else in it — a
// hyphen, a quote, a dot — is text a name cannot hold (a string, a
// message, a key), so its answer also says where it is in the files'
// text, whatever names or doc comments it matched (directive 073).
func nameLike(word string) bool {
	for _, r := range word {
		if !(r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)) {
			return false
		}
	}
	return true
}

// maxTextSites bounds how many sites a word's text: lines name, one
// per file.
const maxTextSites = 8

// textSites reads the source files the index holds — those with a
// language — under their root, for word as a case-insensitive
// substring of their text (directive 073): a word no name, doc comment
// or path contains may still be in a function body or a string, and
// the answer says so, or says it is nowhere, instead of leaving that
// search to grep. Files with no language (documentation, data) are not
// read. Code files come before test files. It returns the first site
// of each of the first maxTextSites files that hold it, how many files
// hold it, and how many were read.
func (st searchState) textSites(word string) (sites []string, files, searched int) {
	needle := []byte(strings.ToLower(word))
	var testSites []string
	for _, r := range st.roots {
		for e := range r.src.All() {
			if e.Kind != entry.KindFile || e.Lang == "" {
				continue
			}
			content, err := os.ReadFile(filepath.Join(r.dir, filepath.FromSlash(e.Path)))
			if err != nil {
				continue
			}
			searched++
			i := bytes.Index(bytes.ToLower(content), needle)
			if i < 0 {
				continue
			}
			files++
			line := 1 + bytes.Count(content[:i], []byte("\n"))
			start := bytes.LastIndexByte(content[:i], '\n') + 1
			end := bytes.IndexByte(content[i:], '\n')
			if end < 0 {
				end = len(content) - i
			}
			text := strings.TrimSpace(string(content[start : i+end]))
			if len(text) > 160 {
				text = text[:160]
			}
			site := fmt.Sprintf("%s:%d: %s", e.Path, line, text)
			if e.Test {
				testSites = append(testSites, site)
			} else if len(sites) < maxTextSites {
				sites = append(sites, site)
			}
		}
	}
	for _, t := range testSites {
		if len(sites) == maxTextSites {
			break
		}
		sites = append(sites, t)
	}
	return sites, files, searched
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

// helpText is what --help and a bare giraffe print (directive 042,
// taking 041's text and adding the range; directive 071's grammar). It is
// the tool's whole self-description to a caller that has never seen it
// — the measured agents read it once and then decided how to explore —
// so it says what is indexed, what a result line carries and what to
// do with the range, how a browse lists a place, how --query matches,
// and, as plainly, what is not indexed. It is also read once and then
// re-read on every later turn of that caller's, so it is bounded like a
// result page: 041's first version was 1,975 bytes and cost a 25-turn
// run some 20k tokens of re-reading; this one says the same in under
// 1,000 (TestHelpText_StaysSmall). It is static and does no index work
// (directive 071): what --query accepts here is what result lines
// print. Directive 050 withdrew its last line, "Not
// indexed: call sites - grep for those": they are, and callers: says
// where. Directive 051 §1: test files are indexed too, and a callers:
// site now carries its own line, not just its place — round 9 (050's
// report) found the place alone still sent every agent to grep the
// name for exactly this text, so this says it is already there.
const helpText = `giraffe: an index of this repository's declarations (Go: types, funcs,
methods, fields, consts, vars) and files, tests included.

  giraffe find <word>...           entries whose name, doc or path
                                   has <word>, closest first;
                                   several: two or more first
  giraffe --query 'attr:value ...' find <word>...  the same, narrowed
  giraffe --query 'attr:value ...' find  browse a place, in file order
  giraffe --page N find <word>...  the next page
  giraffe prepare  build or refresh the index
  giraffe --help  this text

Options go before find; every argument after find is a word.
line:A-B on a result is the declaration's extent: sed -n 'A,Bp' <path>
prints it. Under each: its source line, the doc line matched,
callers: its own sites, rest a count; uses: where a constant or
variable is used. --query takes the attributes result lines print:
path: and callers: match a substring, the rest exactly. Bodies are
not indexed.
`

// runPrepare implements `giraffe prepare` (§4, §24 C-21; directives
// 067, 071). find already checks freshness and rebuilds
// what is stale before answering, so this has no bearing on
// correctness — buildSearchState does the whole job. It exists to move
// that cost out of the first search: on gekka a build is a fraction of
// a second inside a search's own timeout; on kubernetes it is seconds,
// and a person building it once, before an agent's first question,
// is what §4 always meant. Reuses output.Format with no matches
// (code-manners D5) rather than a second rendering path: the evidence
// that the refresh happened is the same ## index line find prints.
func runPrepare(ctx context.Context) int {
	st, code := buildSearchState(ctx)
	defer func() {
		for _, s := range st.stores {
			_ = s.Close()
		}
	}()
	if code != 0 {
		return code
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

func commitHash(ctx context.Context, root string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--short", "HEAD")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// wordDirMin is how many of the several-words results must lie in a
// directory a word names for its block to follow the several-words block.
const wordDirMin = 3

// wordDir is the directory a word of the call names, among the
// directories the several-words results lie in (directive 073): one of
// its path segments contains the word, case aside, and it holds the most
// results of the directories that do. "" when no word names one, when
// the call already narrows by path:, or when the results are files at
// the root.
func wordDir(matches []entry.Entry, words []string, filter query.Filter) (string, int) {
	if _, narrowed := filter["path"]; narrowed {
		return "", 0
	}
	counts := make(map[string]int)
	for _, e := range matches {
		if d := path.Dir(e.Path); d != "." {
			counts[d]++
		}
	}
	best, n := "", 0
	for d, c := range counts {
		if c < wordDirMin || (c < n) || (c == n && d > best) {
			continue
		}
		if segmentNamed(d, words) {
			best, n = d, c
		}
	}
	return best, n
}

// segmentNamed reports whether a segment of dir contains one of words,
// case aside — pkg/standby for "standby", pkg/workloadlearning for
// "workload".
func segmentNamed(dir string, words []string) bool {
	for _, seg := range strings.Split(dir, "/") {
		lower := strings.ToLower(seg)
		for _, w := range words {
			if w != "" && strings.Contains(lower, strings.ToLower(w)) {
				return true
			}
		}
	}
	return false
}
