// pipeline.go is index.Refresh's own concurrency boundary. Directive
// 018 §1 made the freshness-and-dispatch loop a produce/extract/commit
// pipeline over bounded channels; directive 019 §2 extends it one
// stage further upstream — enumeration itself (see enum.go's own
// enumerate) now streams git's output directly into this pipeline's
// classification stage, rather than resolving the whole candidate set
// before anything downstream can start. Directive 020 §3 splits
// runPipeline itself into its own named stages (enumerateAndClassify,
// resolvePipelineContext, dispatchAndExtract), for the same reason 018
// split index.Refresh: a boundary that passes state to the next stage
// is what a pipeline is, and a value object at each one (already
// established here — enumerateOutcome, produceOutcome, fileContext,
// pipelineResult) is what keeps the boundary from becoming a longer
// parameter list.
//
// This restructures Refresh's freshness-and-dispatch loop, not its
// storage layer: the final store.commit call (store.go) is still one
// differential transaction (directive 013), built from whatever
// runPipeline accumulated — that design is unchanged and out of scope
// here.
package index

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"sync"

	"giraffe/internal/entry"
	"giraffe/internal/lang"
	"giraffe/internal/scanner"
)

// extractWorkers is the extract stage's fixed, small worker count (§3
// C4: "fixed concurrency... peak open descriptors is therefore
// computable in advance").
const extractWorkers = 8

// metaBuffer bounds metaCh, the channel enumerate's two concurrent
// streams (tracked, untracked) both write to and classify reads from.
// A FileMeta is a path, a blob hash and an int, not file content, so
// this bound is about pacing, not memory — large enough that either
// stream can race a little ahead of classify's own stat calls without
// blocking on ordinary hardware, without letting an arbitrarily large
// backlog of unclassified paths build up in memory (§3's own
// bounded-resources ethos).
const metaBuffer = 4 * extractWorkers

// fileContext is the read-only facts extractWorker and carryForward
// need to resolve one file's project and content — unchanged for the
// whole call, and otherwise repeated across both functions' own
// parameter lists (code-manners M2: "things that change together live
// together"; patterns.md §9: "extract a value object to replace a
// cluster of primitives passed together"). Built once
// resolvePipelineContext knows markerDirs (directive 019 §2: that
// requires enumerate to have completely finished — see runPipeline's
// own comment), and passed by value from there on, since nothing
// mutates it again.
type fileContext struct {
	root            string
	markerDirs      []string
	projectNames    map[string]string
	fallbackProject string
	markersByDir    map[string]map[string]bool
	assignments     map[string]string
}

// produceItem is one file classify has decided needs an actual
// (re-)extraction. Everything an extract worker needs to process it
// without going back to ro for anything else.
type produceItem struct {
	meta    scanner.FileMeta
	modTime int64
	size    int64
}

// unchangedCandidate is one file classify found content-unchanged
// (its stat matches ro's stored record) — kept, not yet resolved,
// since whether it needs carrying forward at all depends on
// enumChanged, which is only known once enumerate finishes (see
// resolvePipelineContext).
type unchangedCandidate struct {
	meta    scanner.FileMeta
	modTime int64
	size    int64
	oldRec  Record
}

// extracted is one file's fully-processed contribution to the store —
// entries with Size/DeclCount already patched, ready for commit — plus
// any diagnostic the module chain produced along the way (warnErr nil
// when there was none). dispatched is true only for a result that came
// from the extract stage (a file actually sent through lang.Extract
// this run) — false for a file resolved via carryForward, with no
// dispatch at all — which is exactly what directive 018 §4's
// Stats.Dispatched counts.
type extracted struct {
	change     fileChange
	warnPath   string
	warnErr    error
	dispatched bool
}

// produceOutcome is classify's own findings, handed back once it has
// finished (metaCh closed). Reading it is only safe after that close
// is observed — a channel close establishes happens-before per the Go
// memory model, so this single write (by classify) / single later
// read (by enumerateAndClassify's own caller) needs no separate lock.
type produceOutcome struct {
	currentPaths      map[string]bool
	pending           []produceItem
	possiblyUnchanged []unchangedCandidate
}

// pipelineResult is everything runPipeline resolves in one call —
// bundled for the same reason fileContext is (patterns.md §9): every
// field here was a separate return value before directive 019, and
// every caller wants the same handful together, not some now and some
// later.
type pipelineResult struct {
	fileChanges  []fileChange
	warnings     []string
	dispatched   int
	unchanged    int
	currentPaths map[string]bool
	enumBlob     []byte
	walkBlob     []byte
	enumChanged  bool
	rootProject  string
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

// classify is the pipeline's freshness-classification stage (directive
// 019 §2's own "paths flow to freshness classification as they
// arrive"): for each meta enumerate sends, it stats the file and
// compares against ro's last-known record — pure per-file stat data,
// needing no project or marker information at all, which is exactly
// what makes it safe to run concurrently with enumerate still
// streaming later files (§20 C-2's own freshness-by-stat rule, applied
// the same way regardless of what, if anything, produced the meta).
//
// What to DO with the result — dispatch for extraction, carry forward,
// or count as reused — depends on facts only enumerate knows once it
// has completely finished (enumChanged; and markerDirs itself, for
// Project), so that decision is not made here: a file whose content
// may have changed is queued into out.pending, one whose content is
// unchanged into out.possiblyUnchanged, and resolvePipelineContext /
// dispatchAndExtract resolve both once enumerate's outcome is ready.
//
// Terminates when metaCh is closed and drained — enumerate's own
// termination signal for this stage (language-policy G10).
func classify(root string, ro *Store, metaCh <-chan scanner.FileMeta, out *produceOutcome) {
	currentPaths := make(map[string]bool)
	for m := range metaCh {
		oldRec, known := ro.fileRecord(m.Path)
		info, statErr := os.Stat(filepath.Join(root, m.Path))
		if statErr != nil {
			// Gone between enumerate and stat (e.g. a race with an
			// editor's save-as-rename): left out of currentPaths, so
			// the deletion diff in Refresh removes any entries it
			// already owned.
			continue
		}
		currentPaths[m.Path] = true
		modTime, size := info.ModTime().UnixNano(), info.Size()
		contentSame := known && oldRec.ModTime == modTime && oldRec.Size == size
		if contentSame {
			out.possiblyUnchanged = append(out.possiblyUnchanged, unchangedCandidate{meta: m, modTime: modTime, size: size, oldRec: oldRec})
		} else {
			out.pending = append(out.pending, produceItem{meta: m, modTime: modTime, size: size})
		}
	}
	out.currentPaths = currentPaths
}

// carryForward builds a changed file's contribution when its content is
// known unchanged (no re-extraction needed) but enumChanged means its
// Project may have moved (directive 015's own diagnosis, unchanged by
// this directive): the declarations already stored under
// oldRec.EntryIDs are read back and re-stamped with a freshly resolved
// Project, the same way Size is refreshed for every changed file
// elsewhere in this stage. No file content is read to do this.
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

// extractWorker is the pipeline's extract stage: extractWorkers
// instances run concurrently (dispatchAndExtract starts them, once fc
// is fully resolved — see resolvePipelineContext), each taking one
// produceItem at a time from extractCh and running it through
// lang.Extract. scanner.Open here is what makes a module's own
// Content() call actually read the file (directive 017 §1) — lazily,
// and only for whichever module needs it. Files are independent of one
// another (no shared mutable state between them; fc is read-only for
// the whole call), which is what makes running several at once
// correct.
//
// Terminates when extractCh is closed and drained (language-policy
// G10). wg.Done() on the way out is this worker's contribution to
// dispatchAndExtract's overall completion signal for resultCh.
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

// buildMarkersByDir is every marker directory's own set of marker
// basenames — go.mod, Cargo.toml, whichever were actually found there
// — derived from enumerate's own markerPaths. Looked up by a file's
// owning directory to decide which extraction module's own marker
// (directive 017 §3) that directory claims.
func buildMarkersByDir(markerPaths []string) map[string]map[string]bool {
	markersByDir := make(map[string]map[string]bool, len(markerPaths))
	for _, mp := range markerPaths {
		d := path.Dir(mp)
		if markersByDir[d] == nil {
			markersByDir[d] = make(map[string]bool)
		}
		markersByDir[d][path.Base(mp)] = true
	}
	return markersByDir
}

// enumerateAndClassify runs the pipeline's first two stages together —
// enumerate (enum.go; 2 internal goroutines, tracked and untracked) and
// classify (1 goroutine — this call itself, since it has nothing else
// to do until classify finishes anyway) — and returns once both have
// completely finished. Grouped in one function, rather than split
// further, because the two are one producer/consumer pair with nothing
// a caller could usefully do between starting one and the other
// finishing: enumerate sends scanner.FileMeta to metaCh as git streams
// each path; classify consumes metaCh concurrently, stating each meta
// and buffering it into produceOutcome.pending (content changed) or
// .possiblyUnchanged (content same), needing no marker information to
// do so (§20 C-2's own freshness-by-stat rule).
//
// Termination and errors (language-policy G10): enumerate owns metaCh
// and closes it once its own two internal streams are both done or one
// fails; classify terminates when metaCh closes, which is this
// function's own signal that both stages are finished. enumWG.Wait()
// makes that happens-before explicit rather than merely relied upon.
// enumOut.err — a git subprocess failing, the only hard error either
// stage produces — is checked here, before markerDirs or anything
// depending on it is even computed downstream.
func enumerateAndClassify(ctx context.Context, root string, scope scanner.Scope, assignments map[string]string, ro *Store) (enumerateOutcome, produceOutcome, error) {
	metaCh := make(chan scanner.FileMeta, metaBuffer)
	var enumOut enumerateOutcome
	var enumWG sync.WaitGroup
	enumWG.Add(1)
	go func() {
		defer enumWG.Done()
		enumerate(ctx, root, scope, assignments, ro, metaCh, &enumOut)
	}()

	var prodOut produceOutcome
	classify(root, ro, metaCh, &prodOut) // blocks until metaCh closes, i.e. until enumerate finishes

	enumWG.Wait()
	if enumOut.err != nil {
		return enumerateOutcome{}, produceOutcome{}, enumOut.err
	}
	return enumOut, prodOut, nil
}

// resolvePipelineContext turns enumerate's own findings (enumOut) into
// everything extraction and carry-forward need to resolve a file's
// Project — fileContext — plus enumChanged and rootProject, the two
// other facts that depend on the same complete marker set. This is the
// boundary directive 019 §2 found and this directive's own design
// record restates: a marker file can sort anywhere in git's own output
// relative to the files it governs, so none of this can be computed
// from a partial enumeration — everything here needs enumOut exactly
// as enumerateAndClassify returned it, complete.
//
// §21 C-8: nested marker directories sharing a name is the same
// failure as two configured roots sharing one, checked here over the
// marker directories this call found — a hard error, returned rather
// than silently merged under one project name.
func resolvePipelineContext(root string, ro *Store, enumOut enumerateOutcome, assignments map[string]string, fallbackProject string) (fc fileContext, enumChanged bool, rootProject string, err error) {
	markerDirs := scanner.MarkerDirs(enumOut.markerPaths)
	markersByDir := buildMarkersByDir(enumOut.markerPaths)
	projectNames := scanner.ProjectNames(markerDirs, fallbackProject)
	if err := scanner.CheckNameCollisions(projectNames); err != nil {
		return fileContext{}, false, "", err
	}
	rootProject = scanner.ProjectFor(".", markerDirs, projectNames, fallbackProject)

	fc = fileContext{
		root:            root,
		markerDirs:      markerDirs,
		projectNames:    projectNames,
		fallbackProject: fallbackProject,
		markersByDir:    markersByDir,
		assignments:     assignments,
	}

	// A changed enum blob means the tracked file set (or the
	// configuration it was filtered against) was actually
	// re-enumerated, which can move marker directories and therefore
	// project assignments even when every already-known file's own
	// stat is untouched.
	enumChanged = ro != nil && func() bool {
		old, _ := ro.enumBlob()
		return string(old) != string(enumOut.enumBlob)
	}()

	return fc, enumChanged, rootProject, nil
}

// dispatchAndExtract runs the pipeline's remaining stages — extraction
// and carry-forward — against prodOut, once fc is fully resolved
// (resolvePipelineContext), and returns every file's contribution plus
// diagnostics once every stage has finished.
//
// Stages, their input/output, and why:
//   - extract (extractWorkers=8 fixed goroutines, §3 C4): takes
//     produceItem from extractCh, fc, and produces extracted values on
//     resultCh.
//   - a dedicated dispatch goroutine feeds extractCh from
//     prodOut.pending and owns closing it; a dedicated carry-forward
//     goroutine sends resultCh a value for every possiblyUnchanged file
//     when enumChanged, so resultCh's writers are extractWorkers plus
//     this one — no single one of them can own closing it alone (G21),
//     so a WaitGroup-driven goroutine closes it once every writer is
//     done, the same coordination 018 already used for two writer
//     kinds, extended here to three (dispatch itself writes nothing to
//     resultCh but is included in the same WaitGroup for simplicity).
//
// Channel bounds:
//   - extractCh (produceItem, bound 2×extractWorkers=16): a produceItem
//     is a few strings and two ints, not file content, so this bound is
//     about pacing, not memory — large enough that a worker finishing
//     one item always has another ready.
//   - resultCh (extracted, bound extractWorkers=8): one slot per
//     worker, so a worker handing off a finished result is never
//     blocked on ordinary hardware, but this function's own sink
//     cannot fall arbitrarily far behind either.
//
// Termination and errors (language-policy G10): the dispatch goroutine
// owns extractCh and closes it once every pending item is sent. A
// WaitGroup covering dispatch, every extract worker and the
// carry-forward goroutine drives resultCh's close. This function is
// resultCh's sink and ranges over it until it closes; that loop ending
// is this function's own termination path. Nothing here produces a
// hard error — a module declining a file remains a diagnostic
// (warnErr, surfaced in ## warnings), never a reason to fail the call.
//
// §3 C5 ("the same tree produces byte-identical index output"): extract
// workers and the carry-forward goroutine finish in whatever order the
// scheduler picks, so warnings arrive at resultCh in a nondeterministic
// order — sorted by path below before being returned, so C5 holds
// regardless of scheduling. fileChanges needs no equivalent sort:
// store.commit already sorts its own input by path before writing
// (store.go), a pre-existing invariant this directive does not disturb.
func dispatchAndExtract(fc fileContext, prodOut produceOutcome, enumChanged bool, ro *Store) (fileChanges []fileChange, warnings []string, dispatched int) {
	extractCh := make(chan produceItem, 2*extractWorkers)
	resultCh := make(chan extracted, extractWorkers)

	var wg sync.WaitGroup
	wg.Add(1 + extractWorkers + 1) // dispatch, extractWorkers, carry-forward
	go func() {
		defer wg.Done()
		defer close(extractCh)
		for _, it := range prodOut.pending {
			extractCh <- it
		}
	}()
	for i := 0; i < extractWorkers; i++ {
		go extractWorker(fc, extractCh, resultCh, &wg)
	}
	go func() {
		defer wg.Done()
		if !enumChanged {
			return
		}
		for _, u := range prodOut.possiblyUnchanged {
			resultCh <- carryForward(fc, u.meta, u.modTime, u.size, u.oldRec, ro)
		}
	}()
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
	return fileChanges, warnings, dispatched
}

// runPipeline runs enumerate, classify, extract and the commit-
// accumulating sink, and returns once every one of them has finished.
// Itself just the three stages above in sequence, each handed the
// previous one's own value object (patterns.md §9) — see
// enumerateAndClassify, resolvePipelineContext and dispatchAndExtract
// for what each stage does, and why the boundaries sit where they do.
//
// Why enumeration streams into classify but not into extract or
// carryForward (directive 019 §2's own question, answered precisely;
// directive 020 §2 looked for a way to let dispatch start before
// enumeration finishes too — enumerating markers alone first, via a
// git call restricted to marker-name pathspecs — and found it is not
// actually cheap: timed on both corpora (kubernetes and llvm-project),
// a pathspec-restricted `git ls-files` cost as much as, and on
// llvm-project over twice as much as, the full unrestricted
// enumeration it was meant to avoid waiting for — git still walks
// every index entry to test each pathspec, so narrowing by name buys
// nothing at this index size. Timing decided it, not reasoning alone;
// see the completion report for the numbers): every entry this call
// produces is stamped with a Project, and Project resolution needs
// markerDirs — the complete set of marker directories found anywhere
// in the tree. A marker file can sort anywhere in git's own output
// relative to the files it governs (git's index is sorted
// lexicographically by path, not by directory depth or discovery
// order), so seeing a file gives no assurance every marker that will
// turn out to be its ancestor has already been seen. Filtering
// (inclusion/exclusion) is genuinely stateless per path and streams
// correctly; classify's stat comparison is genuinely stateless per
// path too, and streams correctly; project resolution is not, and does
// not. This is why extraction (and carryForward, which also stamps
// Project) still wait for enumerate to finish completely, while
// classify's own stat calls do not.
func runPipeline(ctx context.Context, root string, scope scanner.Scope, ro *Store, assignments map[string]string, fallbackProject string) (pipelineResult, error) {
	enumOut, prodOut, err := enumerateAndClassify(ctx, root, scope, assignments, ro)
	if err != nil {
		return pipelineResult{}, err
	}

	fc, enumChanged, rootProject, err := resolvePipelineContext(root, ro, enumOut, assignments, fallbackProject)
	if err != nil {
		return pipelineResult{}, err
	}

	fileChanges, warnings, dispatched := dispatchAndExtract(fc, prodOut, enumChanged, ro)

	// enumChanged files never reach "unchanged": they are always
	// carried forward instead (dispatchAndExtract's own carry-forward
	// goroutine), so this count is exactly the possiblyUnchanged set
	// when nothing about the enumeration moved, and zero when it did.
	unchanged := 0
	if !enumChanged {
		unchanged = len(prodOut.possiblyUnchanged)
	}

	return pipelineResult{
		fileChanges:  fileChanges,
		warnings:     warnings,
		dispatched:   dispatched,
		unchanged:    unchanged,
		currentPaths: prodOut.currentPaths,
		enumBlob:     enumOut.enumBlob,
		walkBlob:     enumOut.walkBlob,
		enumChanged:  enumChanged,
		rootProject:  rootProject,
	}, nil
}
