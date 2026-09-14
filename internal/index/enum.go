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
	err         error
}

// enumerate is the pipeline's own first stage (directive 019 §2): the
// tracked half (`git ls-files -s`) and the untracked half (`git
// ls-files --others --exclude-standard`) run as two concurrent
// streams — each read incrementally from git's own stdout pipe
// (scanner.StreamTracked/StreamUntracked), not buffered until the
// process exits — filtered against scope as each path arrives, and
// sent to metaCh immediately rather than only once the whole
// candidate set is known. This is what lets classify's own stat
// comparison (pipeline.go) begin on early-arriving files while git is
// still writing later ones; filtering itself needs nothing but the
// one path in front of it, so there is nothing to wait for there.
//
// A cache hit (ro's stored enum blob still valid under root's current
// .git/index fingerprint and configuration — see fingerprintConfig)
// replays the tracked half straight from memory: there is no
// subprocess to stream on that half at all. The untracked half is
// always re-enumerated and streamed fresh regardless of cache state
// (§27 C-34: no comparably cheap fingerprint exists for it).
//
// Marker verification (a cached or freshly-read tracked marker must
// still exist on disk — existingMarkers, below) and the enum-cache
// blob both need the complete, sorted set from both halves, so out is
// only filled in once both streams finish, or one fails. This is a
// real constraint, not an oversight: a marker file can sort anywhere
// in git's own path order relative to the files it governs — nothing
// about encountering one file proves every marker that will turn out
// to be its ancestor has already been seen — so project resolution
// (and therefore dispatch to extraction, which stamps every entry
// with a Project) cannot safely begin before enumerate finishes. See
// runPipeline's own comment for what this does and does not let
// overlap.
//
// enumerate owns metaCh and closes it once both streams are fully
// drained or one fails (language-policy G10, G21). If one stream's
// subprocess fails partway, the other is not cancelled: it runs to
// completion, and whatever it already sent to metaCh is real but
// harmless wasted work once out.err is checked — nothing is written
// to the store until well after this returns (see runPipeline), so a
// failure here can only waste CPU, never corrupt anything.
func enumerate(ctx context.Context, root string, scope scanner.Scope, assignments map[string]string, ro *Store, metaCh chan<- scanner.FileMeta, out *enumerateOutcome) {
	defer close(metaCh)

	modTime, size, fpOK := scanner.GitIndexFingerprint(root)
	configFP := fingerprintConfig(scope.Exclude, assignments)

	var oldBlob []byte
	if ro != nil {
		oldBlob, _ = ro.enumBlob()
	}

	var cached enumCache
	cacheOK := false
	if fpOK && oldBlob != nil {
		if c, ok := decodeEnumCache(oldBlob); ok &&
			c.IndexModTime == modTime && c.IndexSize == size && c.Config == configFP {
			verified := existingMarkers(root, c.TrackedMarkers)
			if len(verified) == len(c.TrackedMarkers) {
				cached = c
				cached.TrackedMarkers = verified
				cacheOK = true
			}
			// else: a cached tracked marker no longer exists on disk —
			// `git ls-files -s` reports the index, not the working
			// tree, so a marker removed without `git rm` changes
			// neither GitIndexFingerprint nor a fresh `git ls-files
			// -s` output. Fall through to a full re-enumeration so
			// the corrected marker set is both returned and
			// persisted below.
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)

	var trackedMetas []scanner.FileMeta
	var trackedMarkers []string
	var trackedErr error
	go func() {
		defer wg.Done()
		if cacheOK {
			for _, m := range cached.TrackedMetas {
				metaCh <- m
			}
			trackedMetas = cached.TrackedMetas
			trackedMarkers = cached.TrackedMarkers
			return
		}
		var candidates []string
		trackedErr = scanner.StreamTracked(ctx, root, scope,
			func(m scanner.FileMeta) {
				metaCh <- m
				trackedMetas = append(trackedMetas, m)
			},
			func(p string) { candidates = append(candidates, p) },
		)
		// trackedMarkers comes from `git ls-files -s`, which reports
		// the index, not the working tree. Verifying by stat is what
		// makes marker freshness independent of staged-vs-unstaged,
		// the same way C-2 removed that distinction for everything
		// else: markers are a handful of files, so stat is cheap
		// regardless of corpus size.
		trackedMarkers = existingMarkers(root, candidates)
	}()

	var untrackedMarkers []string
	var untrackedErr error
	go func() {
		defer wg.Done()
		untrackedErr = scanner.StreamUntracked(ctx, root, scope,
			func(m scanner.FileMeta) { metaCh <- m },
			func(p string) { untrackedMarkers = append(untrackedMarkers, p) },
		)
	}()

	wg.Wait()

	if trackedErr != nil {
		out.err = trackedErr
		return
	}
	if untrackedErr != nil {
		out.err = untrackedErr
		return
	}

	out.markerPaths = append(append([]string{}, trackedMarkers...), untrackedMarkers...)

	if cacheOK {
		out.enumBlob = oldBlob
		return
	}
	if fpOK {
		// Best-effort: a failure to persist the cache changes nothing
		// about correctness, only whether the next call gets to skip
		// this same work. git's own index is already path-sorted, so
		// trackedMetas arrives in order in practice; sorted here
		// explicitly rather than trusted, since §3 C5 is a hard
		// constraint and the cache blob's own byte layout depends on
		// it directly.
		sortedMetas := append([]scanner.FileMeta(nil), trackedMetas...)
		sort.Slice(sortedMetas, func(i, j int) bool { return sortedMetas[i].Path < sortedMetas[j].Path })
		out.enumBlob = encodeEnumCache(modTime, size, configFP, sortedMetas, trackedMarkers)
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
// of its own, since it cannot change without a new binary.
//
// This alone does not make an assignment change retroactively
// re-extract a content-unchanged file — see runPipeline's own comment
// on enumChanged for why, and the completion report for this
// directive, which names the residual gap precisely rather than
// claiming this closes it.
func fingerprintConfig(patterns []scanner.Pattern, assignments map[string]string) string {
	parts := make([]string, len(patterns))
	for i, p := range patterns {
		parts[i] = string(p)
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
