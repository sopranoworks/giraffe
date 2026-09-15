package index

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"giraffe/internal/scanner"
)

// enumerateOutcome is the enumerate stage's own findings: written once,
// by the goroutine that runs it, before that goroutine returns — and
// therefore before metaCh, which it owns, closes (see enumerate). Safe
// to read once metaCh's close is observed downstream, the same
// happens-before produceOutcome (pipeline.go) already relies on for
// the same reason: a channel close establishes happens-before under
// the Go memory model, so this single write / single later read needs
// no separate lock.
type enumerateOutcome struct {
	markerPaths []string
	enumBlob    []byte
	walkBlob    []byte
	err         error
}

// enumerate is the pipeline's own first stage (directive 019 §2,
// extended by directives 021 and 022): the tracked half (`git ls-files
// -s`, where root has its own .git — scanner.HasGitDir) and everything
// else (scanner.Walk's own incremental directory read — every file
// where there is no .git at all, or the untracked remainder of an
// ordinary repository, filtered by scanner.FilterWalked against
// whatever .gitignore contributed too) run as two concurrent
// goroutines, started here and read back once both finish. See
// runTrackedHalf and runWalkedHalf for each half's own reasoning, and
// finishEnumeration for why their results cannot be combined — the
// enum-cache blob, the walk-cache blob, and marker verification all
// need the complete, final state of both halves — until wg.Wait below
// confirms both are done.
func enumerate(ctx context.Context, root string, scope scanner.Scope, assignments map[string]string, ro *Store, metaCh chan<- scanner.FileMeta, out *enumerateOutcome) {
	defer close(metaCh)

	hasGit := scanner.HasGitDir(root)
	modTime, size, fpOK := scanner.GitIndexFingerprint(root)
	configFP := fingerprintConfig(scope.Exclude, assignments)

	oldEnumBlob, oldWalkCache := loadPriorCaches(ro)
	cached, cacheOK := tryEnumCache(root, hasGit, fpOK, oldEnumBlob, modTime, size, configFP)

	var wg sync.WaitGroup
	wg.Add(2)
	var tracked trackedHalfResult
	go func() {
		defer wg.Done()
		tracked = runTrackedHalf(ctx, root, scope, hasGit, cacheOK, cached, metaCh)
	}()
	var walked walkedHalfResult
	go func() {
		defer wg.Done()
		walked = runWalkedHalf(ctx, root, oldWalkCache)
	}()
	wg.Wait()

	// If one half failed, the other is not cancelled: it runs to
	// completion, and whatever it already sent to metaCh is real but
	// harmless wasted work once out.err is checked — nothing is
	// written to the store until well after this returns (see
	// runPipeline), so a failure here can only waste CPU, never
	// corrupt anything.
	if tracked.err != nil {
		out.err = tracked.err
		return
	}
	if walked.err != nil {
		out.err = walked.err
		return
	}

	finishEnumeration(root, scope, hasGit, fpOK, modTime, size, configFP, cacheOK, oldEnumBlob, tracked, walked, metaCh, out)
}

// loadPriorCaches reads root's own persisted caches from ro — nil/zero
// for a first run, or whenever ro is nil. Two independent blobs, two
// independent mechanisms (enumCache is fingerprint-keyed, WalkCache is
// per-directory-mtime-keyed — scanner.Walk's own doc comment), read
// together only because both live in the same store.
func loadPriorCaches(ro *Store) (oldEnumBlob []byte, oldWalkCache scanner.WalkCache) {
	if ro == nil {
		return nil, scanner.WalkCache{}
	}
	oldEnumBlob, _ = ro.enumBlob()
	oldWalkBlob, _ := ro.walkBlob()
	if oldWalkBlob != nil {
		if c, ok := decodeWalkCache(oldWalkBlob); ok {
			oldWalkCache = c
		}
	}
	return oldEnumBlob, oldWalkCache
}

// tryEnumCache reports whether root's persisted enum-cache blob is
// still valid: a tracked half must exist at all (hasGit), its git-index
// fingerprint and the operator's configuration must both still match
// what the cache was built under, and every cached tracked marker must
// still exist on disk. That last check is not redundant with the
// fingerprint one: `git ls-files -s` reports the index, not the
// working tree, so a marker removed from disk without `git rm` changes
// neither GitIndexFingerprint nor a fresh `git ls-files -s` output —
// only a stat catches it.
func tryEnumCache(root string, hasGit, fpOK bool, oldEnumBlob []byte, modTime, size int64, configFP string) (enumCache, bool) {
	if !hasGit || !fpOK || oldEnumBlob == nil {
		return enumCache{}, false
	}
	c, ok := decodeEnumCache(oldEnumBlob)
	if !ok || c.IndexModTime != modTime || c.IndexSize != size || c.Config != configFP {
		return enumCache{}, false
	}
	verified := existingMarkers(root, c.TrackedMarkers)
	if len(verified) != len(c.TrackedMarkers) {
		// A cached tracked marker no longer exists on disk: fall
		// through to a full re-enumeration so the corrected marker set
		// is both returned this call and persisted for the next one.
		return enumCache{}, false
	}
	c.TrackedMarkers = verified
	return c, true
}

// trackedHalfResult is runTrackedHalf's own findings, handed back to
// enumerate once its goroutine returns.
type trackedHalfResult struct {
	metas   []scanner.FileMeta
	markers []string
	err     error
}

// runTrackedHalf is enumerate's tracked-side goroutine body: replays a
// still-valid enum cache straight from memory (no subprocess at all), or
// resolves the tracked tree from scratch (runTrackedTree) — streaming
// `git ls-files -s` (scanner.StreamTracked), filtering and marking each
// path as it arrives (directive 019 §2's own streaming property,
// unchanged since), and recursing into every submodule it finds
// (directive 023 item 3). hasGit false (item 1: no .git at all) is not
// an error, just an empty tracked half — enumerate always starts this
// goroutine regardless, and this is simply what it finds.
func runTrackedHalf(ctx context.Context, root string, scope scanner.Scope, hasGit bool, cacheOK bool, cached enumCache, metaCh chan<- scanner.FileMeta) trackedHalfResult {
	if !hasGit {
		return trackedHalfResult{}
	}
	if cacheOK {
		for _, m := range cached.TrackedMetas {
			metaCh <- m
		}
		return trackedHalfResult{metas: cached.TrackedMetas, markers: cached.TrackedMarkers}
	}
	metas, candidates, err := runTrackedTree(ctx, root, root, "", scope, metaCh)
	// trackedMarkers comes from `git ls-files -s`, which reports the
	// index, not the working tree. Verifying by stat is what makes
	// marker freshness independent of staged-vs-unstaged, the same way
	// C-2 removed that distinction for everything else: markers are a
	// handful of files, so stat is cheap regardless of corpus size —
	// including a submodule's own markers, folded into candidates below
	// by runTrackedTree's own recursion, and verified here once, exactly
	// like every other candidate.
	return trackedHalfResult{metas: metas, markers: existingMarkers(root, candidates), err: err}
}

// runTrackedTree runs scanner.StreamTracked at absSubRoot — root itself
// on the top-level call, a submodule's own working tree on every
// recursive one — reporting every path under prefix ("" at the top, the
// gitlink's own path for a submodule, accumulated further for a
// submodule inside a submodule). A submodule is a complete repository:
// "git ls-files works inside it and is cheaper than walking it.
// Enumeration should use it there, for the same reason it uses it in the
// outer repository" (directive 023 item 3) — replacing 022's own choice
// to run no git inside a submodule at all, made on an assumption the
// directive names as unmeasured and backwards for the common case (a
// submodule is usually a library being consumed, often the larger tree).
//
// Every gitlink StreamTracked reports is one more level of this same
// recursion, gated by scanner.HasGitDir before a subprocess is spent on
// it: a submodule added but never `git submodule update`d has no .git of
// its own to ask, and contributes nothing here — whatever its working
// tree does or does not contain still reaches the index through the
// walked half, unchanged from directive 022. Nested submodules are the
// same case, recursively: a gitlink found while resolving one submodule
// recurses again, under its own accumulated prefix.
//
// Not itself cached the way the top-level tracked half is (runTrackedHalf's
// own cacheOK fast path, above): the outer enum-cache's fingerprint is
// root's own .git/index mtime and size, which says nothing about a
// change inside a submodule's own, separate .git/index. Giving a
// submodule's own tracked half a persistent cache of its own is a larger
// caching-architecture change — a new nested cache key, not just a
// bigger fingerprint — outside what this directive asks for, which is to
// measure cold/warm behaviour and decide the correctness question, not
// to design that key format. Named here, and in the completion report,
// as a disclosed scope limit: warm still benefits from the walked half's
// own per-directory cache (scanner.WalkCache) inside the submodule too,
// since Walk has always descended into it uniformly; only the
// submodule's own git subprocess call re-runs on every call.
func runTrackedTree(ctx context.Context, root, absSubRoot, prefix string, scope scanner.Scope, metaCh chan<- scanner.FileMeta) (metas []scanner.FileMeta, candidates []string, err error) {
	var gitlinks []string
	err = scanner.StreamTracked(ctx, absSubRoot, prefix, scope,
		func(m scanner.FileMeta) { metaCh <- m; metas = append(metas, m) },
		func(p string) { candidates = append(candidates, p) },
		func(p string) { gitlinks = append(gitlinks, p) },
	)
	if err != nil {
		return nil, nil, err
	}
	for _, g := range gitlinks {
		subAbs := filepath.Join(root, g)
		if !scanner.HasGitDir(subAbs) {
			continue
		}
		subMetas, subCandidates, subErr := runTrackedTree(ctx, root, subAbs, g, scope, metaCh)
		if subErr != nil {
			return nil, nil, subErr
		}
		metas = append(metas, subMetas...)
		candidates = append(candidates, subCandidates...)
	}
	return metas, candidates, nil
}

// walkedHalfResult is runWalkedHalf's own findings.
type walkedHalfResult struct {
	paths     []string
	gitignore []scanner.GitignoreRule
	cache     scanner.WalkCache
	err       error
}

// runWalkedHalf is enumerate's other goroutine body: scanner.Walk's
// own incremental directory read, covering item 1's whole tree (no
// .git at all) or item 2's untracked remainder (an ordinary
// repository) — Walk itself has no notion of which case it is in; that
// distinction lives entirely in what finishEnumeration does with its
// raw, unfiltered output (skipping whatever the tracked half already
// covered). Unlike the tracked half, this cannot stream into metaCh as
// it goes: Walk knows nothing about which paths are tracked, so a path
// it finds cannot be filtered against the tracked set until that set
// is completely known — sending an already-tracked path a second time
// would dispatch that file through extraction twice, violating §3 C2
// ("no file is opened twice"). It still runs concurrently with
// runTrackedHalf (the expensive, I/O-bound part overlaps exactly as
// before); only its own filtering, in finishEnumeration, waits.
func runWalkedHalf(ctx context.Context, root string, oldWalkCache scanner.WalkCache) walkedHalfResult {
	paths, gitignore, cache, err := scanner.Walk(ctx, root, oldWalkCache)
	return walkedHalfResult{paths: paths, gitignore: gitignore, cache: cache, err: err}
}

// finishEnumeration runs once both halves have completely finished
// (enumerate's own wg.Wait, with neither having failed): it filters the
// walked half against the now-final tracked path set
// (scanner.FilterWalked), and builds both cache blobs this call's
// result carries forward. Marker verification (existingMarkers, inside
// runTrackedHalf/tryEnumCache above) and both cache blobs all need the
// complete, sorted tracked set, and the walk cache needs Walk's own
// complete output — a marker file can sort anywhere in git's own path
// order relative to the files it governs, so nothing about encountering
// one file proves every marker that will turn out to be its ancestor
// has already been seen. This is why project resolution downstream
// (pipeline.go) also waits for enumerate to finish entirely, not just
// this function.
func finishEnumeration(root string, scope scanner.Scope, hasGit, fpOK bool, modTime, size int64, configFP string, cacheOK bool, oldEnumBlob []byte, tracked trackedHalfResult, walked walkedHalfResult, metaCh chan<- scanner.FileMeta, out *enumerateOutcome) {
	trackedPaths := make(map[string]bool, len(tracked.metas))
	for _, m := range tracked.metas {
		trackedPaths[m.Path] = true
	}
	var walkedMarkers []string
	scanner.FilterWalked(root, walked.paths, trackedPaths, scope, walked.gitignore,
		func(m scanner.FileMeta) { metaCh <- m },
		func(p string) { walkedMarkers = append(walkedMarkers, p) },
	)
	// Every walked marker exists on disk by construction (Walk found it
	// this call, or trusted a cache entry that is only valid when the
	// directory's own mtime still matches — scanner.Walk's own doc
	// comment on why that needs no separate stat-based verification the
	// way the tracked half's cache does).
	out.walkBlob = encodeWalkCache(walked.cache)
	out.markerPaths = append(append([]string{}, tracked.markers...), walkedMarkers...)

	if cacheOK {
		out.enumBlob = oldEnumBlob
		return
	}
	if hasGit && fpOK {
		// Best-effort: a failure to persist the cache changes nothing
		// about correctness, only whether the next call gets to skip
		// this same work. git's own index is already path-sorted, so
		// trackedMetas arrives in order in practice; sorted here
		// explicitly rather than trusted, since §3 C5 is a hard
		// constraint and the cache blob's own byte layout depends on
		// it directly.
		sortedMetas := append([]scanner.FileMeta(nil), tracked.metas...)
		sort.Slice(sortedMetas, func(i, j int) bool { return sortedMetas[i].Path < sortedMetas[j].Path })
		out.enumBlob = encodeEnumCache(modTime, size, configFP, sortedMetas, tracked.markers)
	}
}

// existingMarkers filters markers to the paths that still exist on
// disk (see enumerate's own comments for why git's enumeration alone
// is not enough to decide this).
func existingMarkers(root string, markers []string) []string {
	var kept []string
	for _, m := range markers {
		if _, err := os.Stat(filepath.Join(root, m)); err == nil {
			kept = append(kept, m)
		}
	}
	return kept
}

// fingerprintConfig renders scope's exclude patterns and the
// operator's language assignments as one comparable string — the enum
// cache's filtered result depends on both: an operator editing
// .giraffe/exclude between calls, whether adding a pattern or an
// `ext:<x> lang:<y>` assignment, must invalidate the cached filtering,
// not just the tracked set itself (directive 019 §3: 017 already put
// the pattern list in; the assignments map was simply left out of it).
// defaultExclude is a compile-time constant and needs no fingerprint
// of its own, since it cannot change without a new binary. .gitignore
// is not part of it either: it only ever affects the walked half
// (scanner.walkedRules), which has its own, separate cache
// (WalkCache) — this fingerprint governs the tracked half's cache
// alone.
//
// This alone does not make an assignment change retroactively
// re-extract a content-unchanged file — see runPipeline's own comment
// on enumChanged for why, and the completion report for this
// directive, which names the residual gap precisely rather than
// claiming this closes it.
func fingerprintConfig(patterns []scanner.Pattern, assignments map[string]string) string {
	parts := make([]string, len(patterns))
	for i, p := range patterns {
		parts[i] = p.String()
	}
	keys := make([]string, 0, len(assignments))
	for k := range assignments {
		keys = append(keys, k)
	}
	sort.Strings(keys) // map iteration order is not deterministic (§3 C5)
	for _, k := range keys {
		parts = append(parts, "assign:"+k+"="+assignments[k])
	}
	return strings.Join(parts, "\x1f")
}

// enumCache is the tracked-half enumeration cache's in-memory shape:
// the git-index and configuration fingerprint it was filtered under,
// and the already-filtered metas and marker paths themselves.
type enumCache struct {
	IndexModTime   int64
	IndexSize      int64
	Config         string
	TrackedMetas   []scanner.FileMeta
	TrackedMarkers []string
}

// decodeEnumCache parses the tracked-enumeration cache blob. Any shape
// it does not recognise is reported as ok=false, never an error: the
// caller's correct response to an unreadable cache is to rebuild it, the
// same discard-wholesale convention the rest of the store follows (§23
// C-14).
//
// Format: one header line ("modtime\tsize\tconfig"), then one line per
// filtered tracked file ("F\tpath\tblobhash\tsize") or marker path
// ("M\tpath") — two kinds of fact in one blob, distinguished by a
// leading tag, since a marker path may or may not also be a filtered
// meta (a go.mod excluded from search is still a marker, §20 C-3) and
// storing them as two implicit lists would need its own separator line.
func decodeEnumCache(blob []byte) (enumCache, bool) {
	lines := strings.Split(string(blob), "\n")
	if len(lines) == 0 {
		return enumCache{}, false
	}
	head := strings.SplitN(lines[0], "\t", 3)
	if len(head) != 3 {
		return enumCache{}, false
	}
	modTime, err1 := strconv.ParseInt(head[0], 10, 64)
	size, err2 := strconv.ParseInt(head[1], 10, 64)
	if err1 != nil || err2 != nil {
		return enumCache{}, false
	}
	c := enumCache{IndexModTime: modTime, IndexSize: size, Config: head[2]}
	for _, line := range lines[1:] {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 4)
		switch {
		case len(fields) == 4 && fields[0] == "F":
			size, err := strconv.ParseInt(fields[3], 10, 64)
			if err != nil {
				return enumCache{}, false
			}
			c.TrackedMetas = append(c.TrackedMetas, scanner.FileMeta{Path: fields[1], BlobHash: fields[2], Size: size})
		case len(fields) == 2 && fields[0] == "M":
			c.TrackedMarkers = append(c.TrackedMarkers, fields[1])
		default:
			return enumCache{}, false
		}
	}
	return c, true
}

// encodeEnumCache renders the filtered tracked enumeration under its
// git-index and configuration fingerprint. Metas are written already
// sorted by path (enumerate's own postcondition); markers are sorted
// here purely for reproducible output (§3 C5).
func encodeEnumCache(modTime, size int64, configFP string, metas []scanner.FileMeta, markers []string) []byte {
	sortedMarkers := append([]string(nil), markers...)
	sort.Strings(sortedMarkers)

	var buf strings.Builder
	fmt.Fprintf(&buf, "%d\t%d\t%s\n", modTime, size, sanitize(configFP))
	for _, m := range metas {
		fmt.Fprintf(&buf, "F\t%s\t%s\t%d\n", sanitize(m.Path), sanitize(m.BlobHash), m.Size)
	}
	for _, p := range sortedMarkers {
		fmt.Fprintf(&buf, "M\t%s\n", sanitize(p))
	}
	return []byte(buf.String())
}
