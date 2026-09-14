// pipeline.go is index.Refresh's own concurrency boundary (directive
// 018 §1): produce, a fixed pool of extract workers, and Refresh itself
// as the commit-accumulating sink, connected by two bounded channels —
// design-review's own trigger ("a new concurrency... boundary") and
// patterns.md §6's named shape ("data passes through several
// transformation stages... a pipeline of stages with explicit
// backpressure — bounded queues or bounded channels between stages").
//
// This restructures Refresh's freshness-and-dispatch loop, not its
// storage layer: the final store.commit call (store.go) is still one
// differential transaction (directive 013), built from whatever
// runPipeline accumulated — that design is unchanged and out of scope
// here. What changes is that stat'ing file N+1 (produce) and extracting
// file N (an extract worker) now genuinely overlap, and this restores —
// via a fixed worker pool rather than the bulk eager reader it replaces
// — the concurrent read directive 017 traded away when it made content
// lazy (017's own report named that as a trade-off, not a fix).
package index

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"giraffe/internal/entry"
	"giraffe/internal/lang"
	"giraffe/internal/scanner"
)

// extractWorkers is the extract stage's fixed, small worker count (§3
// C4: "fixed concurrency... peak open descriptors is therefore
// computable in advance"). The same number scanner's own pre-017 bulk
// reader used (its own former workers constant) — restored here as a
// worker pool bounded the same way, not a number picked fresh.
const extractWorkers = 8

// fileContext is the read-only facts every pipeline stage needs to
// resolve one file's project, dispatch order and content — unchanged
// for the whole Refresh call, and otherwise repeated across produce,
// extractWorker and carryForward's own parameter lists (code-manners
// M2: "things that change together live together"; patterns.md §9:
// "extract a value object to replace a cluster of primitives passed
// together"). Not every stage uses every field (produce and
// carryForward never touch assignments or markersByDir), the same way
// a shared config value is not always read by every consumer of it.
type fileContext struct {
	root            string
	markerDirs      []string
	projectNames    map[string]string
	fallbackProject string
	markersByDir    map[string]map[string]bool
	assignments     map[string]string
}

// produceItem is one file the produce stage has decided needs an actual
// (re-)extraction. Everything an extract worker needs to process it
// without going back to produce or to ro for anything else.
type produceItem struct {
	meta    scanner.FileMeta
	modTime int64
	size    int64
}

// extracted is one file's fully-processed contribution to the store —
// entries with Size/DeclCount already patched, ready for commit — plus
// any diagnostic the module chain produced along the way (warnErr nil
// when there was none). dispatched is true only for a result that came
// from the extract stage (a file actually sent through lang.Extract
// this run) — false for a file produce resolved directly with no
// dispatch at all (see carryForward) — which is exactly what directive
// 018 §4's Stats.Dispatched counts.
type extracted struct {
	change     fileChange
	warnPath   string
	warnErr    error
	dispatched bool
}

// produceOutcome is produce's own findings, handed back once it has
// finished. Reading it is only safe after runPipeline's WaitGroup
// confirms produce has returned (see runPipeline) — a channel close and
// a WaitGroup both establish happens-before per the Go memory model, so
// this single write (by produce) / single later read (by runPipeline)
// needs no separate lock.
type produceOutcome struct {
	currentPaths map[string]bool
	unchanged    int
}

// patchFileEntry sets Size and DeclCount on entries' own file entry
// (Kind==KindFile) from facts only the caller knows — size is this
// call's own fresh stat, DeclCount is how many of entries are not the
// file entry itself. A no-op if entries holds no file entry.
func patchFileEntry(entries []entry.Entry, size int64) {
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
		entries[fileIdx].Size = size
		entries[fileIdx].DeclCount = declCount
	}
}

// carryForward builds a changed file's contribution when its content is
// known unchanged (no re-extraction needed) but enumChanged means its
// Project may have moved (directive 015's own diagnosis, unchanged by
// this directive): the declarations already stored under
// oldRec.EntryIDs are read back and re-stamped with a freshly resolved
// Project, the same way Size is refreshed for every changed file
// elsewhere in this stage. No file content is read to do this, so it
// runs directly in produce rather than being handed to an extract
// worker (see produce).
func carryForward(fc fileContext, m scanner.FileMeta, modTime, size int64, oldRec Record, ro *Store) extracted {
	project := scanner.ProjectFor(m.Path, fc.markerDirs, fc.projectNames, fc.fallbackProject)
	var entries []entry.Entry
	for _, e := range ro.entriesByID(oldRec.EntryIDs) {
		e.Project = project
		entries = append(entries, e)
	}
	patchFileEntry(entries, size)
	return extracted{change: fileChange{Path: m.Path, ModTime: modTime, Size: size, Entries: entries}}
}

// produce is the pipeline's first stage: it walks metas — the already-
// resolved candidate set discover returned (§3 C2: "the file set is
// resolved before reading begins... no file is opened twice") — stats
// each one, and classifies it against ro's last-known record. A file
// whose content may have changed is sent to extractCh for the extract
// stage to actually open; a file whose declarations do not need
// re-extraction but whose Project may have moved is resolved here
// directly, with no file content ever read, and sent straight to
// resultCh. This is walking an already-resolved candidate set, not
// discovering files while reading them, so it satisfies C2 even though
// it runs concurrently with the extract stage consuming extractCh (see
// runPipeline) — produce decides, from a stat alone, which files need a
// read; it never causes one itself.
//
// produce owns extractCh (language-policy G21: "the goroutine that
// writes to a channel owns closing it") and closes it once every meta
// has been visited — the only signal the extract stage needs to know no
// more work is coming. out is filled in just before returning; see
// produceOutcome's own comment for why that is safe with no lock.
func produce(fc fileContext, metas []scanner.FileMeta, ro *Store, enumChanged bool, extractCh chan<- produceItem, resultCh chan<- extracted, out *produceOutcome) {
	defer close(extractCh)
	currentPaths := make(map[string]bool, len(metas))
	unchanged := 0
	for _, m := range metas {
		// §20 C-2 puts freshness on stat over the whole candidate set:
		// every file is stat'd and compared against its own last-known
		// fingerprint the same way, regardless of what, if anything,
		// ends up extracting from it.
		oldRec, known := ro.fileRecord(m.Path)
		info, statErr := os.Stat(filepath.Join(fc.root, m.Path))
		if statErr != nil {
			// Gone between List and Stat (e.g. a race with an editor's
			// save-as-rename): left out of currentPaths, so the deletion
			// diff in Refresh removes any entries it already owned.
			continue
		}
		currentPaths[m.Path] = true
		modTime, size := info.ModTime().UnixNano(), info.Size()
		contentSame := known && oldRec.ModTime == modTime && oldRec.Size == size
		switch {
		case !contentSame:
			extractCh <- produceItem{meta: m, modTime: modTime, size: size}
		case enumChanged:
			resultCh <- carryForward(fc, m, modTime, size, oldRec, ro)
		default:
			unchanged++
		}
	}
	out.currentPaths = currentPaths
	out.unchanged = unchanged
}

// extractWorker is the pipeline's second stage: extractWorkers instances
// run concurrently (runPipeline starts them), each taking one
// produceItem at a time from extractCh and running it through
// lang.Extract. scanner.Open here is what makes a module's own
// Content() call actually read the file (directive 017 §1) — lazily,
// and only for whichever module needs it. Files are independent of one
// another (no shared mutable state between them; fc is read-only for
// the whole call), which is what makes running several at once correct.
//
// Terminates when extractCh is closed and drained — produce's own
// termination signal for this stage (language-policy G10). wg.Done() on
// the way out is this worker's contribution to runPipeline's overall
// completion signal.
func extractWorker(fc fileContext, extractCh <-chan produceItem, resultCh chan<- extracted, wg *sync.WaitGroup) {
	defer wg.Done()
	for item := range extractCh {
		f := scanner.Open(fc.root, item.meta)
		project := scanner.ProjectFor(item.meta.Path, fc.markerDirs, fc.projectNames, fc.fallbackProject)
		dirMarkers := fc.markersByDir[scanner.OwningDir(item.meta.Path, fc.markerDirs)]
		entries, declined := lang.Extract(f, project, fc.assignments, dirMarkers)
		patchFileEntry(entries, item.size)
		res := extracted{
			change:     fileChange{Path: item.meta.Path, ModTime: item.modTime, Size: item.size, Entries: entries},
			dispatched: true,
		}
		if declined != nil {
			res.warnPath, res.warnErr = item.meta.Path, declined
		}
		resultCh <- res
	}
}

// runPipeline runs the three stages and returns once every one of them
// has finished: fileChanges ready for store.commit, warnings ready for
// ## warnings, how many files were actually dispatched through
// lang.Extract (directive 018 §4's Stats.Dispatched), how many were
// found unchanged, and the full set of paths seen this call (for
// Refresh's own deletion diff).
//
// Channel bounds:
//   - extractCh (produceItem, bound 2×extractWorkers): a produceItem is a
//     few strings and two ints, not file content, so this bound is about
//     pacing, not memory — large enough that a worker finishing one item
//     always has another ready, without letting produce race arbitrarily
//     far ahead of what the extract stage can actually consume.
//   - resultCh (extracted, bound extractWorkers): one slot per worker, so
//     a worker that finishes handing off its result is never blocked on
//     ordinary hardware, but the sink below cannot fall arbitrarily far
//     behind either — bounded, not unbounded, buildup of finished-but-
//     uncommitted entries (§3's own bounded-resources ethos, applied to
//     this in-memory queue the same way it already applies to file
//     descriptors and worker count).
//
// Termination and errors (language-policy G10, applied to every
// goroutine this function starts): produce owns extractCh and closes it
// once every meta is visited. A dedicated goroutine waits on a
// WaitGroup covering produce and every extract worker, then closes
// resultCh — resultCh has two kinds of writer (produce, for a carried-
// forward file; every extract worker, for a re-extracted one), so no
// single one of them can close it alone (G21); closing is coordinated
// by the WaitGroup instead, not guessed at by any one writer. This
// function is resultCh's sink, and ranges over it — concurrently with
// produce and the extract workers still running, not after them — until
// it closes; that loop ending is this function's own termination path.
// Nothing here produces a hard error: a module declining a file is a
// diagnostic (warnErr, surfaced in ## warnings), never a reason to fail
// Refresh, exactly as before this directive.
//
// A concurrency-specific correctness point (§3 C5, "the same tree
// produces byte-identical index output"): extract workers finish in
// whatever order the scheduler happens to pick, so warnings arrive at
// resultCh in a nondeterministic order — sorted by path below before
// being returned, so C5 holds regardless of goroutine scheduling.
// fileChanges needs no equivalent sort: store.commit already sorts its
// own input by path before writing (store.go), a pre-existing
// invariant this directive does not disturb.
func runPipeline(fc fileContext, metas []scanner.FileMeta, ro *Store, enumChanged bool) (fileChanges []fileChange, warnings []string, dispatched, unchanged int, currentPaths map[string]bool) {
	extractCh := make(chan produceItem, 2*extractWorkers)
	resultCh := make(chan extracted, extractWorkers)

	var wg sync.WaitGroup
	wg.Add(1 + extractWorkers)
	var out produceOutcome
	go func() {
		defer wg.Done()
		produce(fc, metas, ro, enumChanged, extractCh, resultCh, &out)
	}()
	for i := 0; i < extractWorkers; i++ {
		go extractWorker(fc, extractCh, resultCh, &wg)
	}
	go func() {
		wg.Wait()
		close(resultCh)
	}()

	type pathWarning struct{ path, msg string }
	var warns []pathWarning
	for r := range resultCh {
		fileChanges = append(fileChanges, r.change)
		if r.warnErr != nil {
			warns = append(warns, pathWarning{path: r.warnPath, msg: r.warnErr.Error()})
		}
		if r.dispatched {
			dispatched++
		}
	}

	sort.Slice(warns, func(i, j int) bool { return warns[i].path < warns[j].path })
	warnings = make([]string, len(warns))
	for i, w := range warns {
		warnings[i] = fmt.Sprintf("%s: %s", w.path, w.msg)
	}

	return fileChanges, warnings, dispatched, out.unchanged, out.currentPaths
}
