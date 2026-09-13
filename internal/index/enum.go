package index

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"giraffe/internal/scanner"
)

// enumCacheFilename holds the cached, already-filtered tracked half of
// enumeration (§26 C-28/item 5): the result of `git ls-files -s` run
// through scanner.FilterTracked, skipped when nothing that could change
// it has changed since it was last written.
//
// Only the tracked half is ever cached, and it is cached filtered, not
// raw. Measured directly (this directive's own probe, llvm-project,
// 184181 tracked files): the two git subprocess calls together cost
// roughly 260ms of Discover's ~590-600ms, but filtering the tracked set
// against exclude patterns and finding marker files within it costs
// close to as much again (~360ms) — caching only the raw tracked map,
// an earlier version of this cache, still paid that filtering cost on
// every call. Caching the filtered result instead skips both. The
// untracked half is cheap by comparison (there are usually few
// untracked files) and has no safe freshness signal of its own (see
// scanner.GitIndexFingerprint), so it is always re-enumerated and
// re-filtered fresh.
const enumCacheFilename = "enum"

// discoverCached is index.Refresh's replacement for calling
// scanner.Discover directly: it reuses a cached, filtered tracked-file
// enumeration when scanner.GitIndexFingerprint proves nothing that
// could change the tracked set has changed and the configured exclude
// patterns are the same ones the cache was filtered against, and always
// re-runs and re-filters the untracked half fresh, since untracked
// files have no comparably cheap freshness signal — a newly created or
// deleted untracked file changes neither HEAD nor .git/index (§26
// C-28/item 5: "a working tree that has moved without HEAD moving").
// Falling back to a full, uncached enumeration is always correct, only
// slower, so any failure to read, parse, or write the enum cache is
// swallowed rather than surfaced: a caching layer must never be a new
// way for `find` to fail.
func discoverCached(ctx context.Context, root string, scope scanner.Scope) (metas []scanner.FileMeta, markerPaths []string, err error) {
	modTime, size, fpOK := scanner.GitIndexFingerprint(root)
	excludeFP := fingerprintExclude(scope.Exclude)

	if fpOK {
		if cached, ok := loadEnumCache(root); ok &&
			cached.IndexModTime == modTime && cached.IndexSize == size && cached.Exclude == excludeFP {
			untracked, err := scanner.EnumerateUntracked(ctx, root)
			if err != nil {
				return nil, nil, err
			}
			untrackedMetas, untrackedMarkers := scanner.FilterUntracked(root, untracked, scope)
			metas = scanner.MergeSortedMetas(cached.TrackedMetas, untrackedMetas)
			markerPaths = append(append([]string{}, cached.TrackedMarkers...), untrackedMarkers...)
			return metas, markerPaths, nil
		}
	}

	tracked, err := scanner.EnumerateTracked(ctx, root)
	if err != nil {
		return nil, nil, err
	}
	untracked, err := scanner.EnumerateUntracked(ctx, root)
	if err != nil {
		return nil, nil, err
	}
	trackedMetas, trackedMarkers := scanner.FilterTracked(root, tracked, scope)
	untrackedMetas, untrackedMarkers := scanner.FilterUntracked(root, untracked, scope)
	if fpOK {
		// Best-effort: a failure to persist the cache changes nothing
		// about correctness, only whether the next call gets to skip
		// this same work.
		_ = saveEnumCache(root, modTime, size, excludeFP, trackedMetas, trackedMarkers)
	}
	metas = scanner.MergeSortedMetas(trackedMetas, untrackedMetas)
	markerPaths = append(append([]string{}, trackedMarkers...), untrackedMarkers...)
	return metas, markerPaths, nil
}

// fingerprintExclude renders scope's exclude patterns as one comparable
// string — the enum cache's filtered result depends on exactly this
// list (defaultExclude is a compile-time constant and cannot change
// without a new binary, so it needs no fingerprint of its own), and an
// operator editing .giraffe/exclude between calls must invalidate the
// cached filtering, not just the tracked set itself.
func fingerprintExclude(patterns []scanner.Pattern) string {
	parts := make([]string, len(patterns))
	for i, p := range patterns {
		parts[i] = string(p)
	}
	return strings.Join(parts, "\x1f")
}

// enumCache is the tracked-half enumeration cache's in-memory shape:
// the git-index and exclude-pattern fingerprint it was filtered under,
// and the already-filtered metas and marker paths themselves.
type enumCache struct {
	IndexModTime   int64
	IndexSize      int64
	Exclude        string
	TrackedMetas   []scanner.FileMeta
	TrackedMarkers []string
}

// loadEnumCache reads the persisted tracked-enumeration cache. Any
// shape it does not recognise — including none written yet — is
// reported as ok=false, never an error: the caller's correct response
// to an unreadable cache is to rebuild it, the same discard-wholesale
// convention Load already applies to the content cache (§23 C-14).
//
// Format: one header line ("modtime\tsize\texclude"), then one line per
// filtered tracked file ("F\tpath\tblobhash\tsize") or marker path
// ("M\tpath") — two kinds of fact in one file, distinguished by a
// leading tag, since a marker path may or may not also be a filtered
// meta (a go.mod excluded from search is still a marker, §20 C-3) and
// storing them as two implicit lists would need its own separator line.
// Size (directive 009 item 3) is stat'd once when a tracked file first
// enters this cache and reused on every hit — the alternative, stat'ing
// every file on every call regardless of cache state, measured a ~1s
// regression on llvm-project's warm path alone (report:
// reports/2026-09-13-009-cost-to-arrive-complete.md), undoing most of
// item 5's own win from directive 008.
func loadEnumCache(root string) (enumCache, bool) {
	data, err := os.ReadFile(filepath.Join(root, Dir, cacheDir, enumCacheFilename))
	if err != nil {
		return enumCache{}, false
	}
	lines := strings.Split(string(data), "\n")
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
	c := enumCache{IndexModTime: modTime, IndexSize: size, Exclude: head[2]}
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

// saveEnumCache persists the filtered tracked enumeration under its
// git-index and exclude-pattern fingerprint. Metas are written already
// sorted by path (FilterTracked's own postcondition); markers are
// sorted here purely for reproducible output (§3 C5), the same reason
// Save sorts the content cache.
func saveEnumCache(root string, modTime, size int64, excludeFP string, metas []scanner.FileMeta, markers []string) error {
	sortedMarkers := append([]string(nil), markers...)
	sort.Strings(sortedMarkers)

	var buf strings.Builder
	fmt.Fprintf(&buf, "%d\t%d\t%s\n", modTime, size, sanitize(excludeFP))
	for _, m := range metas {
		fmt.Fprintf(&buf, "F\t%s\t%s\t%d\n", sanitize(m.Path), sanitize(m.BlobHash), m.Size)
	}
	for _, p := range sortedMarkers {
		fmt.Fprintf(&buf, "M\t%s\n", sanitize(p))
	}

	dir := filepath.Join(root, Dir, cacheDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s/%s: %w", Dir, cacheDir, err)
	}
	return os.WriteFile(filepath.Join(dir, enumCacheFilename), []byte(buf.String()), 0o644)
}
