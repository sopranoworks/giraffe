package index

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sopranoworks/giraffe/internal/scanner"
)

// Stats summarises one Refresh call — the evidence prior directives'
// measurement steps ask for (index size, generation time are measured by
// the caller around Refresh; these are the counts Refresh itself is
// positioned to know).
type Stats struct {
	Reused int // files whose stat matched their store record, left untouched this run (§20 C-2)

	// Dispatched counts files actually sent through lang.Extract this
	// run (directive 018 §4 — named for exactly what it counts: since
	// directive 017 §1, a file the generic module handles is dispatched
	// without ever being opened, so "files read" and "files dispatched"
	// are different numbers, and this is the second, not the first).
	Dispatched  int
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

	// The pipeline (directive 018 §1, extended by directive 019 §2; see
	// pipeline.go) is Refresh's own concurrency boundary: it enumerates,
	// classifies, dispatches whatever actually needs (re-)extraction
	// through a fixed pool of extract workers, and resolves markerDirs
	// and this root's own project name — all of which now happen inside
	// the pipeline itself, since project resolution needs enumeration
	// completely finished (see runPipeline's own comment on why that
	// boundary sits where it does). ro may be nil (first run);
	// runPipeline and everything it calls tolerate that the same way the
	// code it replaced did.
	fallbackProject := filepath.Base(root)

	// A first run, or a stale or unreadable store being rebuilt from
	// scratch (§23 C-14's discard-and-rebuild path): there is no prior
	// store to read, so nothing needs the whole corpus's changes in
	// memory at once — each file's contribution goes straight from the
	// pipeline into a fresh store's batch, in path order, and is
	// committed in bounded transactions (directive 067; commitBatch).
	// The one transaction this replaced held kubernetes' whole index
	// as bbolt nodes at once, which is what put its build past the
	// heap ceiling and the timeout.
	if !roOK {
		target, err := createStore(root)
		if err != nil {
			return nil, Stats{}, err
		}
		w := target.beginCommit(commitBulk)
		result, err := runPipeline(ctx, root, scope, nil, assignments, fallbackProject, w.add)
		if err != nil {
			w.abort()
			_ = target.Close()
			_ = os.Remove(target.path)
			return nil, Stats{}, err
		}
		keysWritten, err := w.finish(cacheBlobs{enum: result.enumBlob, writeEnum: true, walk: result.walkBlob})
		if err != nil {
			_ = target.Close()
			_ = os.Remove(target.path)
			return nil, Stats{}, err
		}
		return target, Stats{
			RootProject: result.rootProject,
			Warnings:    result.warnings,
			Dispatched:  result.dispatched,
			Reused:      result.unchanged,
			KeysWritten: keysWritten,
		}, nil
	}

	// A store exists: its records decide what changed, and the pipeline
	// reads it (classify, carryForward) for as long as it runs, so the
	// changes are collected first and written once it is closed — the
	// differential update (directive 013), bounded by the change, not
	// the corpus.
	var changes []fileChange
	collect := func(c fileChange) error {
		changes = append(changes, c)
		return nil
	}
	result, err := runPipeline(ctx, root, scope, ro, assignments, fallbackProject, collect)
	if err != nil {
		return nil, Stats{}, err
	}

	var stats Stats
	stats.RootProject = result.rootProject
	stats.Warnings = result.warnings
	stats.Dispatched = result.dispatched
	stats.Reused = result.unchanged

	// Every path the store already knows about that this call did not
	// see at all is gone — deleted, or newly excluded (directive 013:
	// "entries belonging to deleted files: removed").
	var deletes []string
	for _, p := range ro.filePaths() {
		if !result.currentPaths[p] {
			deletes = append(deletes, p)
		}
	}

	// The zero-decode, zero-write fast path (§30 C-46, extended by
	// directive 013 from "no entry decoded" to "no key touched at
	// all"): nothing about any file, or the enumeration cache, needs to
	// change, so the existing store is returned exactly as it was
	// opened.
	if len(changes) == 0 && len(deletes) == 0 {
		r := ro
		ro = nil // ownership transferred to the caller; the deferred Close above must not also close it
		return r, stats, nil
	}

	writeEnum := result.enumChanged
	_ = ro.Close()
	ro = nil
	target, err := openForWrite(root)
	if err != nil {
		return nil, Stats{}, err
	}
	caches := cacheBlobs{enum: result.enumBlob, writeEnum: writeEnum, walk: result.walkBlob}
	keysWritten, err := target.commit(changes, deletes, caches, commitDifferential)
	if err != nil {
		_ = target.Close()
		return nil, Stats{}, err
	}
	stats.KeysWritten = keysWritten
	return target, stats, nil
}

// readLines reads root/Dir/name as a list of trimmed, non-empty lines
// with "#" comments removed, alongside each line's 1-based source line
// number (LoadExtensions' own malformed-assignment error names the file
// and the line, which requires knowing it — blank lines and comments are
// not renumbered away). A missing file returns no lines and no error —
// nothing configured yet is not a failure. A thin wrapper over
// readLinesAt for the common case (a file under this root's own Dir);
// LoadExtensions' global layer is not under any project root at all, so
// it calls readLinesAt directly.
//
// Shared by LoadExtensions and LoadRoots: both are "a small list of
// strings in a .giraffe/ file, one per line" — found by running
// `giraffe find exclude` before writing LoadRoots (code-manners D1) and
// factored out per D5, rather than writing LoadRoots as a second
// near-identical reader beside what was then LoadExclude. LoadExclude
// itself (above) no longer uses this: gitignore syntax defines its own
// blank/comment/trim rules, applied by ParsePattern, and pre-processing
// its lines here would only diverge from them.
func readLines(root, name string) (lines []string, lineNos []int, err error) {
	return readLinesAt(filepath.Join(root, Dir, name))
}

func readLinesAt(path string) (lines []string, lineNos []int, err error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
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
// .giraffe/exclude: exclusion patterns, in gitignore syntax — the same
// syntax and the same parser (scanner.ParsePattern) .gitignore itself
// uses (directive 023 item 1: "there is one parser to write, not two").
// A missing file means no project-level exclusions beyond the scanner's
// own defaults. A line ParsePattern cannot make sense of is skipped
// silently, exactly as an unrecognised .gitignore line is — the same
// forward-compatibility guard, not a licence to leave anything gitignore
// specifies today unimplemented (see ParsePattern's own doc comment).
//
// Read as raw lines, not through readLines: readLines' own trimming and
// blank/comment stripping is right for LoadRoots (a plain path list),
// but gitignore syntax defines its own blank-line, comment, and
// trailing-whitespace rules (ParsePattern), and this file must apply
// them exactly the way .gitignore does — pre-trimming here would only
// diverge from that, not simplify it.
//
// Operator language assignments ("ext:<x>", "ext:<x> lang:<y>") no
// longer live here (directive 023 item 2: "extension definitions are
// not exclusion rules") — see LoadExtensions, extensions.go. Before this
// directive they were distinguished from exclusion patterns by line
// shape, which is exactly why exclude could not simply be gitignore
// syntax; moving them out removes that conflict entirely, and with it
// §26 C-29's hard-error path for exclude specifically (an assignment
// mistyped in exclude is now just an ordinary, unparseable-or-not
// gitignore line — LoadExtensions keeps its own hard error for its own
// file, where the fixed assignment grammar is everything the file has).
//
// Hand-written, unlike everything under cacheDir: authored by the
// operator, and theirs to commit or not — portable and committable
// (§25 C-27).
func LoadExclude(root string) (patterns []scanner.Pattern, err error) {
	data, err := os.ReadFile(filepath.Join(root, Dir, "exclude"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s/exclude: %w", Dir, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSuffix(line, "\r")
		p, ok := scanner.ParsePattern(line)
		if !ok {
			continue
		}
		patterns = append(patterns, p)
	}
	return patterns, nil
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
