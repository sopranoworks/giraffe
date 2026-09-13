package index

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
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

// Refresh brings root's store up to date with the current working tree
// and returns it, open, ready to query (§30 C-46: entries are read
// through it lazily — All/Candidates — never materialized into a
// process-resident slice here). Caller closes it when done querying.
//
// Freshness is decided by stat — mtime and size — over the candidate set
// scanner.List already resolved (§20 C-2), exactly as before §30; only
// where that comparison is read from, and what a hit or miss does next,
// changed. When nothing at all needs to change — every extractable
// file's stat matches its store record, and the enumeration cache is
// still fresh — Refresh opens the existing store read-only and returns
// it untouched: no entry is decoded, no write transaction happens (the
// fast path §30 C-46 exists for). Any other outcome — a first run, a
// stale or unreadable store, a changed file, or a stale enumeration
// cache — rebuilds the whole store in one write transaction, the same
// full-rewrite shape Save always had; carried-over files' entries are
// looked up by id from the old store rather than re-extracted, but every
// bucket in the new store (entries, files, name_trigrams) is written
// fresh (§23 C-14's "discard and rebuild" ethos, applied to why a
// partial update is not attempted: convergence, not incrementality, is
// the property this cache needs).
func Refresh(ctx context.Context, root string, scope scanner.Scope, assignments map[string]string) (*Store, Stats, error) {
	ro, roOK, err := openStore(root, true)
	if err != nil {
		return nil, Stats{}, err
	}
	if roOK {
		defer func() {
			if ro != nil {
				_ = ro.Close()
			}
		}()
	}
	var roForDiscover *Store
	if roOK {
		roForDiscover = ro
	}

	metas, markerPaths, enumBlob, err := discover(ctx, root, scope, roForDiscover)
	if err != nil {
		return nil, Stats{}, err
	}
	// A changed enum blob means the tracked file set (or the exclude
	// fingerprint it was filtered against) was actually re-enumerated,
	// which can move marker directories and therefore project
	// assignments even when every already-known extractable file's own
	// stat is untouched — e.g. a project marker's content changing
	// without any file being added or removed. Persisted file entries
	// (fileEntry, cached in the store's entries bucket like anything
	// else now) would otherwise carry a stale Project forward on the
	// fast path below, since that path never recomputes them. Forcing a
	// rebuild here is conservative — most enum-cache misses already
	// force one via a changed or new extractable file too — but this is
	// the one case that would not otherwise be caught.
	enumChanged := roOK && func() bool {
		old, _ := ro.enumBlob()
		return string(old) != string(enumBlob)
	}()

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

	var all []entry.Entry
	var extractable []scanner.FileMeta
	var toRead []scanner.FileMeta
	var toReadFingerprint []Record // ModTime/Size only, aligned with toRead

	// §22 item 4 / §24 C-18: one file entry per scanned file, so a file
	// can be found by name even when it has no declarations of its own.
	// Computed fresh every call, never cached — see fileEntry's own
	// comment for why.
	for _, m := range metas {
		fe := fileEntry(m, markerDirs, projectNames, fallbackProject, assignments)
		fe.Size = m.Size
		all = append(all, fe)
		if _, ok := lang.For(path.Ext(m.Path)); ok {
			extractable = append(extractable, m)
		}
	}

	files := make(map[string]Record, len(extractable))
	changed := !roOK || enumChanged
	for _, m := range extractable {
		info, statErr := os.Stat(filepath.Join(root, m.Path))
		if statErr != nil {
			// Gone between List and Stat (e.g. a race with an editor's
			// save-as-rename); drop it from this run rather than fail
			// the command. It reappears in the next List if it returns.
			continue
		}
		cur := Record{ModTime: info.ModTime().UnixNano(), Size: info.Size()}

		if roOK {
			if prev, ok := ro.fileRecord(m.Path); ok && prev.ModTime == cur.ModTime && prev.Size == cur.Size {
				files[m.Path] = prev
				continue
			}
		}
		changed = true
		toRead = append(toRead, m)
		toReadFingerprint = append(toReadFingerprint, cur)
	}
	// A file present in the old store but no longer in the current
	// candidate set (deleted, or newly excluded) must not survive into
	// the rebuilt store — files only ever holds this run's own set, so a
	// removal is simply "not added here," never something to delete. A
	// length mismatch is the same conservative signal the old text
	// cache's Save shortcut used (§23 Step 1: len(newCache) ==
	// len(cached)), not a full set comparison.
	if roOK && len(files) != ro.fileCount() {
		changed = true
	}

	paths := make([]string, len(toRead))
	for i, m := range toRead {
		paths[i] = m.Path
	}
	readFiles, err := scanner.Read(ctx, root, paths)
	if err != nil {
		return nil, Stats{}, err
	}

	var stats Stats
	stats.RootProject = rootProject
	var freshlyParsed []entry.Entry
	for i, f := range readFiles {
		m := toRead[i]
		project := scanner.ProjectFor(f.Path, markerDirs, projectNames, fallbackProject)
		entries, err := extract(f, project, assignments)
		if err != nil {
			// A file that fails to parse degrades the answer, not the
			// command: the rest of the store still builds (§2, "a stale
			// index degrades gracefully"). Reported in ## warnings by
			// the output layer.
			stats.Warnings = append(stats.Warnings, fmt.Sprintf("%s: %v", f.Path, err))
			continue
		}
		freshlyParsed = append(freshlyParsed, entries...)
		fp := toReadFingerprint[i]
		files[m.Path] = Record{ModTime: fp.ModTime, Size: fp.Size}
		stats.Reparsed++
	}
	stats.Reused = len(extractable) - stats.Reparsed - len(stats.Warnings)

	// The zero-decode fast path: nothing about the extraction cache or
	// the enumeration cache needs to change, so the existing store is
	// returned exactly as it was opened — no entry, in this store or the
	// old one, was ever decoded to reach this point.
	if !changed && roOK {
		result := ro
		ro = nil // ownership transferred to the caller; the deferred Close above must not also close it
		return result, stats, nil
	}

	// Something changed: rebuild. Carried-over files' entries are looked
	// up by id from the old store (a targeted read, not a scan);
	// declarations for reparsed files are already in freshlyParsed;
	// every scanned file's own fileEntry is already in all.
	if roOK {
		// §3 C5: sorted rather than ranged over directly, so the order
		// carried-over entries join all in does not depend on Go's
		// randomised map iteration.
		paths := make([]string, 0, len(files))
		for path := range files {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		for _, path := range paths {
			rec := files[path]
			if len(rec.EntryIDs) == 0 {
				continue // reparsed this run, or a file with zero declarations
			}
			all = append(all, ro.entriesByID(rec.EntryIDs)...)
		}
	}
	all = append(all, freshlyParsed...)

	// A file's declaration count is only knowable once every declaration
	// entry is assembled — fileEntry already built this file's own
	// KindFile entry above, so DeclCount is patched here in one pass
	// (directive 009 item 3).
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

	fresh, err := createStore(root)
	if err != nil {
		return nil, Stats{}, err
	}
	if err := fresh.build(files, all, enumBlob); err != nil {
		_ = fresh.Close()
		return nil, Stats{}, err
	}
	return fresh, stats, nil
}

// fileEntry represents m itself as a search target, addressed by its own
// name rather than by any declaration inside it (§22 item 4). Not
// cached: computing it fresh here, from the same scanner.List result
// Refresh already has on every call, costs one small struct per file
// already being walked, with no file I/O of its own to save by caching
// it — unlike a declaration entry, there is nothing here a cache hit
// would spare.
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
// pattern either.
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
