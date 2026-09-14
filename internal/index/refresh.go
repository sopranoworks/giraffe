package index

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"giraffe/internal/entry"
	"giraffe/internal/lang"
	"giraffe/internal/scanner"
)

// Stats summarises one Refresh call — the evidence prior directives'
// measurement steps ask for (index size, generation time are measured by
// the caller around Refresh; these are the counts Refresh itself is
// positioned to know).
type Stats struct {
	Reused      int      // files whose stat matched their store record, left untouched this run (§20 C-2)
	Reparsed    int      // files (re-)extracted this run
	Warnings    []string // files that could not be extracted; skipped, not fatal
	RootProject string   // this root's own project name — the marker at its own top, or its basename

	// KeysWritten counts distinct bbolt keys the write transaction
	// actually touched — 0 on the fast path, where no transaction runs
	// at all. Directive 013's own completion condition is measured by
	// this number directly: "5 files changed" must write on the order
	// of those files' entries and postings, not the whole store.
	KeysWritten int
}

// Refresh brings root's store up to date with the current working tree
// and returns it, open, ready to query (§30 C-46: entries are read
// through it lazily — All/Candidates — never materialized into a
// process-resident slice here). Caller closes it when done querying.
//
// Freshness is decided by stat — mtime and size — over the candidate set
// scanner.List already resolved (§20 C-2), exactly as before; only what a
// hit or miss does next changed with directive 013. When nothing at all
// needs to change — every extractable file's stat matches its store
// record, no file has appeared or disappeared, and the enumeration cache
// is still fresh — Refresh opens the existing store read-only and
// returns it untouched: no entry is decoded, no write transaction
// happens (the fast path §30 C-46 exists for).
//
// Anything else is a differential update, not a rewrite: only the files
// that actually changed have their own entries and trigram postings
// replaced, and only the files that are actually gone have theirs
// removed (directive 013 — "the write transaction contains the
// difference, not the corpus"). The one exception is a first run, a
// stale or unreadable store, or a stale enumeration cache (a marker's
// content can reassign projects without any extractable file's own stat
// moving) — those still rebuild the whole store from scratch, via the
// same differential mechanism with every scanned file treated as
// changed and nothing to remove (§23 C-14's discard-and-rebuild ethos:
// one mechanism, not a full-rewrite path kept separately in step).
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
	// assignments even when every already-known file's own stat is
	// untouched — e.g. a project marker's content changing without any
	// file being added or removed. Persisted file entries would
	// otherwise carry a stale Project forward on the fast path below,
	// since that path never recomputes them. Forcing a full rebuild here
	// is conservative — most enum-cache misses already force one via a
	// changed or new file too — but this is the one case that would not
	// otherwise be caught, and differentially reconciling exactly which
	// files' Project actually moved is left for the next directive (see
	// the completion report).
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

	var stats Stats
	stats.RootProject = rootProject

	// changedFile is one scanned file this call must (re-)write: its own
	// meta, the freshness fingerprint its new Record will carry, whether
	// its declarations actually need re-extraction, and — when they do
	// not — the ids its declarations already live under, so they can be
	// carried forward rather than re-parsed.
	type changedFile struct {
		meta      scanner.FileMeta
		modTime   int64
		size      int64
		reExtract bool
		oldIDs    []uint64
	}
	currentPaths := make(map[string]bool, len(metas))
	var changed []changedFile
	unchanged := 0
	for _, m := range metas {
		var oldRec Record
		var known bool
		if roOK {
			oldRec, known = ro.fileRecord(m.Path)
		}
		// §20 C-2 puts freshness on stat over the whole candidate set:
		// every file is stat'd and compared against its own last-known
		// fingerprint the same way, regardless of what, if anything,
		// ends up extracting from it — directive 016 §1 removed the
		// only place that distinction was knowable in advance (there is
		// no extension table left to consult here; see reExtract below).
		info, statErr := os.Stat(filepath.Join(root, m.Path))
		if statErr != nil {
			// Gone between List and Stat (e.g. a race with an editor's
			// save-as-rename): left out of currentPaths, so the
			// deletion diff below removes any entries it already owned.
			// It reappears in the next List if it returns.
			continue
		}
		currentPaths[m.Path] = true
		cur := changedFile{meta: m, modTime: info.ModTime().UnixNano(), size: info.Size()}
		contentSame := known && oldRec.ModTime == cur.modTime && oldRec.Size == cur.size
		switch {
		case !contentSame:
			// Content changed: every registered module gets a chance to
			// claim it (lang.Extract, directive 016 §1) — "no extension
			// table, no capability query" means there is no cheaper way
			// left to decide this file is not worth reading than asking.
			cur.reExtract = true
			changed = append(changed, cur)
		case enumChanged:
			// Content itself is unchanged — its declarations do not
			// need re-extraction — but its Project may have moved, so
			// its own file entry still needs a fresh write; the
			// existing declaration ids are carried forward untouched
			// below rather than re-parsed.
			cur.oldIDs = oldRec.EntryIDs
			changed = append(changed, cur)
		default:
			unchanged++
		}
	}

	// Every path the store already knows about that this call did not
	// see at all is gone — deleted, or newly excluded (directive 013:
	// "entries belonging to deleted files: removed").
	var deletes []string
	if roOK {
		for _, p := range ro.filePaths() {
			if !currentPaths[p] {
				deletes = append(deletes, p)
			}
		}
	}

	// The zero-decode, zero-write fast path (§30 C-46, extended by
	// directive 013 from "no entry decoded" to "no key touched at
	// all"): nothing about any file, or the enumeration cache, needs to
	// change, so the existing store is returned exactly as it was
	// opened.
	if roOK && len(changed) == 0 && len(deletes) == 0 {
		result := ro
		ro = nil // ownership transferred to the caller; the deferred Close above must not also close it
		stats.Reused = unchanged
		return result, stats, nil
	}

	// Every file whose content changed is read: directive 016 §1 left
	// no extension table here to pre-filter toRead against, and no
	// capability query is allowed to stand in for one — the only way to
	// learn whether a registered module wants a file is to hand it the
	// content and ask (lang.Extract, below). A changed-only-because-of-
	// enumChanged file is excluded, same as before: its declarations
	// are carried forward, not re-parsed, so its content need not be
	// read again.
	var toRead []scanner.FileMeta
	for _, c := range changed {
		if c.reExtract {
			toRead = append(toRead, c.meta)
		}
	}
	readPaths := make([]string, len(toRead))
	for i, m := range toRead {
		readPaths[i] = m.Path
	}
	readFiles, err := scanner.Read(ctx, root, readPaths)
	if err != nil {
		return nil, Stats{}, err
	}
	// entriesByPath holds, per changed file, exactly what lang.Extract
	// returned: the file's own entry (whichever module produced it —
	// every module's contract includes it, entry.FileEntry) followed by
	// whatever declarations that module found, or just the file entry
	// alone when none did (§18; directive 016 §1 — "the generic module
	// ... produces the file's own entry").
	entriesByPath := make(map[string][]entry.Entry, len(toRead))
	for i, f := range readFiles {
		m := toRead[i]
		project := scanner.ProjectFor(f.Path, markerDirs, projectNames, fallbackProject)
		entries, declined := lang.Extract(f, project, assignments)
		if declined != nil {
			// A module that recognised the file and then failed to
			// parse it degrades the answer, not the command (§2, "a
			// stale index degrades gracefully"): the next module in
			// order — ultimately the generic one — still produced the
			// file's own entry, reported here in ## warnings rather
			// than dropped silently.
			stats.Warnings = append(stats.Warnings, fmt.Sprintf("%s: %v", f.Path, declined))
		}
		entriesByPath[m.Path] = entries
		stats.Reparsed++
	}
	stats.Reused = unchanged

	// fileEntry() is gone (directive 016 §1): the file's own entry now
	// arrives as part of whatever lang.Extract already produced, not as
	// a second, separately-computed value. This loop only patches the
	// two facts index.Refresh itself knows that no language module
	// could (Size, from this call's own fresh stat, and DeclCount, from
	// how many non-file entries came back) — the same two patches the
	// old fileEntry()-based loop made, applied to whichever entry in
	// the slice is Kind==KindFile rather than to a value held apart
	// from the rest.
	fileChanges := make([]fileChange, 0, len(changed))
	for _, c := range changed {
		var entries []entry.Entry
		switch {
		case c.reExtract:
			entries = entriesByPath[c.meta.Path]
		case len(c.oldIDs) > 0:
			// Carried forward, not re-parsed — but this branch only
			// runs because enumChanged means Project may have moved
			// (see above), so each entry's own stored Project is
			// exactly as stale as the file's, and is refreshed here the
			// same way Size is below (directive 015: "the same defect
			// C-2 fixed for declarations, still open for markers" —
			// 013's own diagnosis of what was left).
			project := scanner.ProjectFor(c.meta.Path, markerDirs, projectNames, fallbackProject)
			for _, e := range ro.entriesByID(c.oldIDs) {
				e.Project = project
				entries = append(entries, e)
			}
		}
		declCount := 0
		fileIdx := -1
		for i, e := range entries {
			if e.Kind == entry.KindFile {
				fileIdx = i
			} else {
				declCount++
			}
		}
		if fileIdx >= 0 {
			// c.size, not c.meta.Size: the latter can be the enum
			// cache's own stat, taken whenever the cache was last
			// written, not this call (scanner.FileMeta.Size's own doc
			// comment says so directly) — exactly the staleness this
			// whole loop exists to prevent. c.size is always this
			// call's own fresh stat (see the freshness loop above), for
			// every file in changed regardless of what handled it.
			entries[fileIdx].Size = c.size
			entries[fileIdx].DeclCount = declCount
		}
		fileChanges = append(fileChanges, fileChange{Path: c.meta.Path, ModTime: c.modTime, Size: c.size, Entries: entries})
	}

	writeEnum := !roOK || enumChanged
	var target *Store
	if roOK {
		_ = ro.Close()
		ro = nil
		target, err = openForWrite(root)
	} else {
		target, err = createStore(root)
	}
	if err != nil {
		return nil, Stats{}, err
	}
	keysWritten, err := target.commit(fileChanges, deletes, enumBlob, writeEnum)
	if err != nil {
		_ = target.Close()
		return nil, Stats{}, err
	}
	stats.KeysWritten = keysWritten
	return target, stats, nil
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
