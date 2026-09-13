package index

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"giraffe/internal/entry"
	"giraffe/internal/lang"
	"giraffe/internal/scanner"
)

// Stats summarises one Refresh call — the evidence the directive's Step 6
// asks for (index size, generation time are measured by the caller around
// Refresh; these are the counts Refresh itself is positioned to know).
type Stats struct {
	Reused      int      // cached entries carried over unread (stat unchanged, §20 C-2)
	Reparsed    int      // files (re-)extracted this run
	Warnings    []string // files that could not be extracted; skipped, not fatal
	RootProject string   // this root's own project name — the marker at its own top, or its basename
}

// Refresh brings the local cache up to date with the current working
// tree, then returns the full current entry set.
//
// Freshness is decided by stat — mtime and size — over the candidate set
// scanner.List already resolved (§20 C-2), not by git blob hash, so a
// stat-based check does not need git's tracked/staged/unstaged
// distinctions at all: an edit is an edit, whether or not it has been
// `git add`ed. Only the extraction subset of that candidate set (§24
// C-18: files whose extension has a registered extractor) is ever
// stat'd or cached — the rest get a file entry and nothing else, since
// there is no read to spare a cache hit against.
//
// Tracked and untracked files are cached identically (§23 C-14). Before
// this directive, an untracked file was always re-extracted and never
// persisted, because §8 committed the index and "not yet shared with
// anyone" was a real distinction for something about to be committed.
// The cache is local now — nothing it holds is ever shared with anyone,
// tracked or not — so that distinction has nothing left to protect, and
// §19's original framing already anticipated this: an untracked file's
// "freshness is decided by content or mtime instead" of a blob hash, not
// that it goes uncached. A work-in-progress file, untracked by
// definition, is exactly the case this session's caching should speed up.
func Refresh(ctx context.Context, root string, scope scanner.Scope, assignments map[string]string) ([]entry.Entry, Stats, error) {
	cached, err := Load(root)
	if err != nil {
		return nil, Stats{}, err
	}

	// One enumeration serves both the scope-filtered candidate set and
	// marker discovery (scanner.Discover) — measured to matter at scale
	// (§23 Step 1): fetching each independently paid for `git ls-files`
	// twice, at roughly the cost of everything else Refresh does
	// combined. discoverCached (§26 C-28/item 5) additionally skips the
	// tracked half's own cost — enumeration and the filtering built on
	// top of it — when scanner.GitIndexFingerprint proves nothing that
	// could change the tracked set has changed; the untracked half is
	// always re-run fresh, since it has no comparably cheap freshness
	// signal.
	metas, markerPaths, err := discoverCached(ctx, root, scope)
	if err != nil {
		return nil, Stats{}, err
	}

	markerDirs := scanner.MarkerDirs(markerPaths)
	fallbackProject := filepath.Base(root)
	projectNames := scanner.ProjectNames(markerDirs, fallbackProject)
	// §21 C-8: nested marker directories sharing a name is the same
	// failure as two configured roots sharing one (checked by main, over
	// a different names map) — one detection, both call sites.
	if err := scanner.CheckNameCollisions(projectNames); err != nil {
		return nil, Stats{}, err
	}
	rootProject := scanner.ProjectFor(".", markerDirs, projectNames, fallbackProject)

	newCache := make(map[string]Record, len(metas))
	var all []entry.Entry
	var extractable []scanner.FileMeta
	var toRead []scanner.FileMeta
	var toReadFingerprint []Record // ModTime/Size only, aligned with toRead — avoids a second stat per file below

	// §22 item 4 / §24 C-18: one file entry per scanned file, so a file
	// can be found by name even when it has no declarations of its own —
	// every top-level declaration unexported (§21 C-6), or no registered
	// extractor at all (§24 C-18: file-name indexing does not need one).
	// Computed fresh every call from the same metas list Refresh already
	// has for freshness; never cached — see fileEntry's own comment for
	// why. Extraction is a narrower set: only files whose extension has a
	// registered extractor go on to the stat/cache/read/parse pipeline
	// below — everything else already got its file entry and needs
	// nothing more, so it is never stat'd, cached, or read.
	// m.Size (directive 009 item 3) already comes from discoverCached:
	// stat'd once when a tracked file first entered the enum cache and
	// reused on every hit thereafter, or stat'd fresh here for the
	// always-uncached untracked half (scanner.FilterTracked/
	// FilterUntracked). Stat'ing it again per call, for every file
	// regardless of cache state, was tried first and measured a ~1s
	// regression on llvm-project's warm path alone — undoing most of
	// directive 008 item 5's win — so fileEntry only ever reads it here,
	// never stats on its own.
	for _, m := range metas {
		fe := fileEntry(m, markerDirs, projectNames, fallbackProject, assignments)
		fe.Size = m.Size
		all = append(all, fe)
		if _, ok := lang.For(path.Ext(m.Path)); ok {
			extractable = append(extractable, m)
		}
	}

	for _, m := range extractable {
		info, statErr := os.Stat(filepath.Join(root, m.Path))
		if statErr != nil {
			// Gone between List and Stat (e.g. a race with an editor's
			// save-as-rename); drop it from this run rather than fail
			// the command. It reappears in the next List if it returns.
			continue
		}
		cur := Record{ModTime: info.ModTime().UnixNano(), Size: info.Size()}

		if prev, ok := cached[m.Path]; ok && prev.ModTime == cur.ModTime && prev.Size == cur.Size {
			newCache[m.Path] = prev
			all = append(all, prev.Entries...)
			continue
		}
		toRead = append(toRead, m)
		toReadFingerprint = append(toReadFingerprint, cur)
	}

	paths := make([]string, len(toRead))
	for i, m := range toRead {
		paths[i] = m.Path
	}
	files, err := scanner.Read(ctx, root, paths)
	if err != nil {
		return nil, Stats{}, err
	}

	var stats Stats
	stats.RootProject = rootProject
	for i, f := range files {
		m := toRead[i]
		project := scanner.ProjectFor(f.Path, markerDirs, projectNames, fallbackProject)
		entries, err := extract(f, project, assignments)
		if err != nil {
			// A file that fails to parse degrades the answer, not the
			// command: the rest of the index still builds (§2, "a stale
			// index degrades gracefully"). Reported in ## warnings by the
			// output layer.
			stats.Warnings = append(stats.Warnings, fmt.Sprintf("%s: %v", f.Path, err))
			continue
		}
		all = append(all, entries...)
		fp := toReadFingerprint[i]
		newCache[m.Path] = Record{ModTime: fp.ModTime, Size: fp.Size, Entries: entries}
		stats.Reparsed++
	}
	stats.Reused = len(extractable) - stats.Reparsed - len(stats.Warnings)

	// A file's declaration count is only knowable once every declaration
	// entry is in all — extraction above runs after fileEntry already
	// built this file's own KindFile entry, so it is patched here in one
	// pass rather than threaded through the extraction loop (directive
	// 009 item 3; same signal as Size, for the corpora that do have an
	// extractor).
	declCounts := make(map[string]int, len(extractable))
	for _, e := range all {
		if e.Kind != entry.KindFile {
			declCounts[e.Path]++
		}
	}
	for i := range all {
		if all[i].Kind == entry.KindFile {
			all[i].DeclCount = declCounts[all[i].Path]
		}
	}

	// Every entry newCache holds when nothing was reparsed came from a
	// cache hit (copied from cached verbatim, never mutated) — so
	// newCache is then a subset of cached, and equal length means it is
	// the same set of records already on disk. Skipping the rewrite in
	// that case matters at scale (§23 Step 1): serialising and writing
	// the whole cache back unchanged, on every single invocation, was
	// measured at close to a third of total wall time on a Go corpus two
	// orders of magnitude past gekka — pure waste on the "nothing
	// changed" run C-15 calls the case caching exists for.
	if stats.Reparsed == 0 && len(newCache) == len(cached) {
		return all, stats, nil
	}
	if err := Save(root, newCache); err != nil {
		return nil, Stats{}, err
	}
	return all, stats, nil
}

// fileEntry represents m itself as a search target, addressed by its own
// name rather than by any declaration inside it (§22 item 4). Not
// cached: computing it fresh here, from the same scanner.List result
// Refresh already has on every call, costs one small struct per file
// already being walked, with no file I/O of its own to save by caching
// it — unlike a declaration entry, there is nothing here a cache hit
// would spare.
//
// Name drops the extension so an exact query for "retry" hits the
// file retry.go at the same top rank an exact symbol-name match gets
// (tier 0) — the strongest signal, matching the observation that a file
// name resists naming drift better than a symbol name does. Exported is
// always true: a file's existence is not a language-visibility concept,
// so it gets the same "always visible" default an exported declaration
// gets, rather than being hidden cross-project the way an unexported
// declaration is.
func fileEntry(m scanner.FileMeta, markerDirs []string, projectNames map[string]string, fallbackProject string, assignments map[string]string) entry.Entry {
	base := path.Base(m.Path)
	ext := path.Ext(base)
	return entry.Entry{
		Path:     m.Path,
		Name:     strings.TrimSuffix(base, ext),
		Kind:     entry.KindFile,
		Project:  scanner.ProjectFor(m.Path, markerDirs, projectNames, fallbackProject),
		Ext:      strings.TrimPrefix(ext, "."),
		Lang:     lang.NameFor(ext, assignments),
		Exported: true,
	}
}

// extract runs the extractor registered for f's extension and stamps
// Ext and Lang on every entry it returns (§24 C-20, §25 C-24): one
// place does this for every language module, rather than each module
// naming itself. extractable already filtered Refresh's candidates
// down to extensions lang.For recognises, so the only way ok is false
// here is a defect in that filtering, not a normal input.
func extract(f scanner.File, project string, assignments map[string]string) ([]entry.Entry, error) {
	ext := path.Ext(f.Path)
	fn, ok := lang.For(ext)
	if !ok {
		return nil, fmt.Errorf("no extractor registered for %s", f.Path)
	}
	entries, err := fn(f, project)
	if err != nil {
		return nil, err
	}
	extNoDot := strings.TrimPrefix(ext, ".")
	name := lang.NameFor(ext, assignments)
	for i := range entries {
		entries[i].Ext = extNoDot
		entries[i].Lang = name
	}
	return entries, nil
}

// readLines reads root/Dir/name as a list of trimmed, non-empty lines
// with "#" comments removed, alongside each line's 1-based source line
// number (§26 C-29: a malformed assignment error names the file and the
// line, which requires knowing it — blank lines and comments are not
// renumbered away). A missing file returns no lines and no error —
// nothing configured yet is not a failure.
//
// Shared by LoadExclude and LoadRoots: both are "a small list of strings
// in a .giraffe/ file, one per line" — found by running `giraffe find
// exclude` before writing LoadRoots (code-manners D1) and factored out
// per D5, rather than writing LoadRoots as a second near-identical
// reader beside LoadExclude.
func readLines(root, name string) (lines []string, lineNos []int, err error) {
	data, err := os.ReadFile(filepath.Join(root, Dir, name))
	if os.IsNotExist(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read %s/%s: %w", Dir, name, err)
	}
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		lines = append(lines, line)
		lineNos = append(lineNos, i+1)
	}
	return lines, lineNos, nil
}

// LoadExclude reads the per-project scope configuration in
// .giraffe/exclude: exclusion patterns (and negations, §24 C-19) plus,
// since §25 C-26, operator language assignments — the only tier that
// can know a project's own convention (".inc" is C here, assembler
// there). A missing file means no project-level exclusions or
// assignments beyond the scanner's own defaults.
//
// A line is an assignment when it is exactly two whitespace-separated
// tokens "ext:<value>" and "lang:<value>", in either order — the same
// name:value shape --query already uses, so the mental model transfers
// without pulling this package's two-token grammar through query's
// tokenizer (built for arbitrary filters, quoting, validation against
// an index — heavier than this fixed shape needs). Anything else is an
// exclusion pattern, as before — except a line that starts with "ext:"
// or "lang:" but does not parse as a well-formed assignment, which is a
// hard error (§26 C-29): no real path starts that way, so a line
// shaped like a typoed assignment is never a plausible exclusion
// pattern either. Before this directive such a line silently fell
// through and excluded nothing — the exact class of failure §5 already
// forbids for --query, now closed here too.
//
// This file format is not specified by the directive (§19 open item 10);
// chosen here as the smallest thing that works, reusing the same pattern
// syntax scanner.Pattern already implements.
//
// Hand-written, unlike everything under cacheDir: authored by the
// operator, and theirs to commit or not — and now, per §25 C-27,
// exclude specifically is the portable, committable one, so an
// assignment written once serves everyone on the project.
func LoadExclude(root string) (patterns []scanner.Pattern, assignments map[string]string, err error) {
	lines, lineNos, err := readLines(root, "exclude")
	if err != nil {
		return nil, nil, err
	}
	assignments = make(map[string]string)
	for i, l := range lines {
		if ext, langName, ok := parseAssignment(l); ok {
			assignments["."+ext] = langName
			continue
		}
		if looksLikeAssignment(l) {
			return nil, nil, fmt.Errorf("%s/exclude:%d: malformed language assignment %q (want \"ext:<value> lang:<value>\", either order)", Dir, lineNos[i], l)
		}
		patterns = append(patterns, scanner.Pattern(l))
	}
	return patterns, assignments, nil
}

// looksLikeAssignment reports whether line was meant as an operator
// language assignment (§26 C-29) — it starts with "ext:" or "lang:" —
// regardless of whether it actually parses as one. Used only to decide
// whether a parseAssignment failure is a malformed assignment (an
// error) or an ordinary exclusion pattern (not one).
func looksLikeAssignment(line string) bool {
	return strings.HasPrefix(line, "ext:") || strings.HasPrefix(line, "lang:")
}

// parseAssignment recognises an operator language assignment line
// (§25 C-26): exactly "ext:<value> lang:<value>", in either order,
// neither value empty. Anything else — including a bare pattern, or a
// pattern that happens to contain a colon — is not an assignment; the
// caller (LoadExclude) decides whether that makes it an ordinary
// pattern or an error.
func parseAssignment(line string) (ext, langName string, ok bool) {
	fields := strings.Fields(line)
	if len(fields) != 2 {
		return "", "", false
	}
	var haveExt, haveLang bool
	for _, f := range fields {
		switch {
		case strings.HasPrefix(f, "ext:"):
			ext, haveExt = strings.TrimPrefix(f, "ext:"), true
		case strings.HasPrefix(f, "lang:"):
			langName, haveLang = strings.TrimPrefix(f, "lang:"), true
		default:
			return "", "", false
		}
	}
	if !haveExt || !haveLang || ext == "" || langName == "" {
		return "", "", false
	}
	return ext, langName, true
}

// LoadRoots reads .giraffe/roots: paths to other project roots to
// include in this invocation's search. A missing file means this
// invocation searches only its own root.
//
// This is the mechanism proposed for directive 002 §2 and confirmed by
// the operator: a local, uncommitted list — which sibling checkouts
// exist is a fact about this machine, not this project, so it stays out
// of the cache directory (it is hand-written, not derived) but is still
// expected to remain uncommitted in the ordinary case.
func LoadRoots(root string) ([]string, error) {
	lines, _, err := readLines(root, "roots")
	return lines, err
}
