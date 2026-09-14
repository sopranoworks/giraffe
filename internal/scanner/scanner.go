// Package scanner owns all file access (specifications/giraffe.md §18).
// List resolves the file set once — tracked paths and blob hashes from
// `git ls-files -s`, untracked paths from `git ls-files --others
// --exclude-standard` — without opening anything, so a caller can decide,
// from blob hashes alone, which files are already indexed and can be left
// unread (§9: "unchanged blob → file is not reopened"). Open then returns
// a File whose content is read from disk only when a caller actually asks
// for it (directive 017 §1). Language modules never open a file
// themselves; they receive File values.
package scanner

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// FileMeta is one candidate file's identity, without its contents.
type FileMeta struct {
	Path     string // repo-relative, normalised
	BlobHash string // git blob hash; "" for untracked files

	// Size is the file's byte size, stat'd once when this FileMeta is
	// produced (FilterTracked/FilterUntracked/filterMetas, below) — a
	// declaration search never needed it, but a filename result on a
	// corpus with no extractor has nothing else to tell a caller
	// deciding whether to open the file directly or narrow it first
	// (directive 009 item 3). 0 if the stat failed (gone between
	// enumeration and filtering); callers already tolerate that race
	// elsewhere (index.Refresh's own extraction stat).
	//
	// Not a freshness signal: on an enum-cache hit (index.discover),
	// the tracked half's metas — this field included — come back from
	// whenever the cache was written, not freshly stat'd this call.
	// index.Refresh's own freshness comparison stats again, on purpose.
	Size int64
}

// File is one file's resolved identity, as delivered to a language
// module. Content is not carried on the value itself (directive 017
// §1): a module gets it only by calling Content(), so a module that
// never needs content — the exclusion module, the generic module —
// never causes a read. Language modules still never touch the
// filesystem themselves (§18): the read behind Content is scanner's
// own, via Open below.
type File struct {
	Path     string
	BlobHash string
	content  func() ([]byte, error)
}

// Content reads f's contents, the first time a caller actually asks
// for them (directive 017 §1). A File with nothing behind it (the zero
// value) reports no content and no error — never reached by a real
// caller, since every File in this package is built by Open or
// WithContent, both of which always set it.
func (f File) Content() ([]byte, error) {
	if f.content == nil {
		return nil, nil
	}
	return f.content()
}

// Open returns a File for m, under root, whose content is read from
// disk only when a caller calls Content() — scanner's one filesystem
// read implementation (§18), deferred rather than run eagerly for
// every candidate file (directive 017 §1: most never need it, and the
// modules that do not read now never trigger the read at all).
func Open(root string, m FileMeta) File {
	p := m.Path
	return File{
		Path:     p,
		BlobHash: m.BlobHash,
		content: func() ([]byte, error) {
			b, err := os.ReadFile(filepath.Join(root, p))
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", p, err)
			}
			return b, nil
		},
	}
}

// WithContent returns a File whose content is already known — fixed
// in-memory bytes, not read from disk. Used by tests, which have no
// file on disk to defer a read against.
func WithContent(path string, content []byte) File {
	return File{Path: path, content: func() ([]byte, error) { return content, nil }}
}

// Pattern is one exclusion rule. A trailing "/" matches a directory and
// everything under it ("vendor/"). Otherwise it is a glob (path.Match)
// matched against the full relative path, and — if the pattern contains no
// "/" — against the file's base name too, so "*.pb.go" excludes regardless
// of directory. This is the common subset of .gitignore patterns, chosen
// because it is already the mental model anyone using git has.
type Pattern string

func (p Pattern) excludes(relPath string) bool {
	s := string(p)
	if strings.HasSuffix(s, "/") {
		prefix := strings.TrimSuffix(s, "/")
		return relPath == prefix || strings.HasPrefix(relPath, prefix+"/")
	}
	if ok, _ := path.Match(s, relPath); ok {
		return true
	}
	if !strings.Contains(s, "/") {
		if ok, _ := path.Match(s, path.Base(relPath)); ok {
			return true
		}
	}
	return false
}

// negation reports whether p is a negated pattern (a leading "!", the
// same mental model .gitignore already uses — Pattern's own doc
// comment) and returns the pattern with that prefix stripped. A
// negated pattern forces inclusion of whatever it matches, overriding
// every other exclusion pattern below it in the merged list.
//
// It no longer overrides the exclusion module's deny list the way it
// once overrode scanner's own coverage default (§24 C-19): that
// default moved out of this package entirely (directive 017 §2), to a
// module dispatch stage a scanner Pattern cannot reach. A denied
// extension (logo.png) can no longer be pulled back in with
// `!logo.png` in .giraffe/exclude — named here, and in the completion
// report, as a real behaviour change this directive did not ask for
// and does not restore.
func (p Pattern) negation() (bool, Pattern) {
	if strings.HasPrefix(string(p), "!") {
		return true, Pattern(strings.TrimPrefix(string(p), "!"))
	}
	return false, p
}

// defaultExclude is the baseline every project starts with: dependency and
// build-output trees a reuse search gets nothing from. It is the same set
// specifications/giraffe.md §11 excludes from its sizing measurement, kept
// here so a real run stays comparable to that evidence. Per-project
// configuration adds to this; it does not replace it.
var defaultExclude = []Pattern{
	// giraffe's own local cache and configuration (index.Dir — repeated
	// as a literal here, not imported, since index already imports this
	// package). A repository with no .gitignore entry for it — every
	// test repo in this codebase, and plausibly some real ones — would
	// otherwise have its own store file picked up by the untracked-file
	// scan the moment the store is created, indexing giraffe's cache as
	// a search target of itself. Directive 013 found this: its own
	// fix — a newly-appeared untracked file is no longer silently
	// dropped by the fast path (§20 C-2, extended) — is what turned this
	// from a latent gap into an observed one.
	".giraffe/",
	"vendor/",
	"node_modules/",
	"testdata/",
	"examples/",
	"*_test.go",
	"*.pb.go",
}

// Scope selects what List considers: the paths left out. Extension —
// or extensionless shape — no longer gates the candidate set itself
// (§24 C-18, extended by directive 017 §2): List returns every path
// exclusion leaves standing, full stop. Which of those a language
// module actually wants, including whether to exclude it from the
// index entirely, is decided downstream, per file, by the registered
// chain (giraffe/internal/lang) — the scanner names no extension and
// no filename shape anywhere in this package.
type Scope struct {
	Exclude []Pattern

	// Markers are extra root-marker basenames to recognise, on top of
	// markerNames below — a registered language module's own root
	// marker (go.mod, via lang.RegisteredMarkers()), threaded in by
	// the caller rather than imported here: scanner must not depend on
	// lang, which already depends on scanner for scanner.File
	// (directive 017 §3 — "one authority over those names": go.mod is
	// named once, in the golang module, and scanner only ever receives
	// it).
	Markers []string
}

// Root resolves and canonicalises a scan root (symlinks followed once, up
// front), the way every other function in this package expects it.
func Root(root string) (string, error) {
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve scan root %q: %w", root, err)
	}
	return resolved, nil
}

// List resolves the current file set — tracked and untracked, filtered
// only by scope's exclusion patterns (§19; directive 017 §2 removed
// the coverage-by-extension default this doc comment used to also
// describe) — without reading any of it. Order is deterministic (path
// order), independent of anything about how the files were produced.
func List(ctx context.Context, root string, scope Scope) ([]FileMeta, error) {
	tracked, untracked, err := enumerate(ctx, root)
	if err != nil {
		return nil, err
	}
	return filterMetas(root, tracked, untracked, scope), nil
}

// enumerate is git's two-call file enumeration (tracked with blob hashes,
// then untracked), factored out so a caller needing both List's
// scope-filtered result and MarkerFiles' unfiltered one in the same
// invocation pays for it once — see Discover, below.
func enumerate(ctx context.Context, root string) (tracked map[string]string, untracked []string, err error) {
	tracked, err = trackedFiles(ctx, root)
	if err != nil {
		return nil, nil, err
	}
	untracked, err = untrackedFiles(ctx, root)
	if err != nil {
		return nil, nil, err
	}
	return tracked, untracked, nil
}

// EnumerateTracked runs only the tracked half of enumerate: `git
// ls-files -s`. Exposed so a caller can cache this half across calls
// (index.Refresh, keyed on GitIndexFingerprint below) while always
// re-running the untracked half fresh, which has no comparably cheap
// freshness signal (§26 C-28/item 5).
func EnumerateTracked(ctx context.Context, root string) (map[string]string, error) {
	return trackedFiles(ctx, root)
}

// EnumerateUntracked runs only the untracked half of enumerate: `git
// ls-files --others --exclude-standard`.
func EnumerateUntracked(ctx context.Context, root string) ([]string, error) {
	return untrackedFiles(ctx, root)
}

// Combine builds the same (metas, markerPaths) result Discover would,
// from an already-known tracked and untracked file set. This is the
// seam a persistent enumeration cache uses (index.Refresh) to skip
// re-running git for the tracked half without duplicating filterMetas
// or markersOf.
func Combine(root string, tracked map[string]string, untracked []string, scope Scope) (metas []FileMeta, markerPaths []string) {
	return filterMetas(root, tracked, untracked, scope), markersOf(tracked, untracked, mergedMarkers(scope))
}

// GitIndexFingerprint reports the mtime and size of root's .git/index —
// git's own record of the tracked file set and staged changes. Any
// operation that can change what EnumerateTracked returns (add, rm,
// checkout, merge, reset, commit, stash) rewrites this file, so it is a
// correct freshness signal for the tracked half of enumeration and,
// unlike HEAD, catches a staged-but-uncommitted add or remove too.
//
// It says nothing about untracked files: a file created or deleted in
// the working tree without ever being staged changes neither this
// fingerprint nor HEAD (§26 C-28/item 5 — "a working tree that has
// moved without HEAD moving"). There is no comparably cheap fingerprint
// for that half; EnumerateUntracked must always be re-run, never cached
// against this or any other signal.
//
// ok is false if .git/index cannot be stat'd at all (a linked worktree
// or submodule whose .git is a file pointing elsewhere, or a repository
// state with no index yet) — the caller's only correct response is to
// skip the cache, not to treat this as an error: an enumeration that
// always re-runs git is still correct, only slower.
func GitIndexFingerprint(root string) (modTime, size int64, ok bool) {
	info, err := os.Stat(filepath.Join(root, ".git", "index"))
	if err != nil {
		return 0, 0, false
	}
	return info.ModTime().UnixNano(), info.Size(), true
}

// mergedExclude is the one place default and project-level exclusion
// patterns are combined (§19). Factored out so FilterTracked and
// FilterUntracked, below, build the same list filterMetas always has,
// without restating the append twice.
func mergedExclude(scope Scope) []Pattern {
	exclude := make([]Pattern, 0, len(defaultExclude)+len(scope.Exclude))
	exclude = append(exclude, defaultExclude...)
	exclude = append(exclude, scope.Exclude...)
	return exclude
}

// mergedMarkers is markerNames — the root markers scanner recognises on
// its own, because no registered language module claims them yet
// (directive 017 §3) — plus scope.Markers, a registered module's own
// marker (go.mod, from lang.RegisteredMarkers()). Factored out so every
// caller below combines the two the same way, without restating the
// append.
func mergedMarkers(scope Scope) []string {
	markers := make([]string, 0, len(markerNames)+len(scope.Markers))
	markers = append(markers, markerNames...)
	markers = append(markers, scope.Markers...)
	return markers
}

func filterMetas(root string, tracked map[string]string, untracked []string, scope Scope) []FileMeta {
	exclude := mergedExclude(scope)

	files := make(map[string]string, len(tracked)+len(untracked)) // path -> blob hash ("" if untracked)
	for relPath, hash := range tracked {
		if included(relPath, exclude) {
			files[relPath] = hash
		}
	}
	for _, relPath := range untracked {
		if included(relPath, exclude) {
			if _, seen := files[relPath]; !seen {
				files[relPath] = ""
			}
		}
	}

	metas := make([]FileMeta, 0, len(files))
	for p, hash := range files {
		metas = append(metas, FileMeta{Path: p, BlobHash: hash, Size: statSize(root, p)})
	}
	sort.Slice(metas, func(i, j int) bool { return metas[i].Path < metas[j].Path })
	return metas
}

// statSize is FileMeta.Size's one source: a best-effort stat, never an
// error — a file gone between enumeration and filtering (e.g. a race
// with an editor's save-as-rename) just gets Size 0, the same
// tolerance index.Refresh's own extraction stat already has for the
// same race.
func statSize(root, relPath string) int64 {
	info, err := os.Stat(filepath.Join(root, relPath))
	if err != nil {
		return 0
	}
	return info.Size()
}

// FilterTracked filters tracked's paths through scope's exclusion rules
// (§19), and separately finds every marker file among them regardless
// of exclusion (§20 C-3: root discovery must see go.mod even where an
// exclude pattern would otherwise drop it — markersOf's own rule,
// reproduced here since a cached tracked half needs both without
// re-deriving one from the other).
//
// Exposed so a persistent enumeration cache (index.Refresh) can filter
// and mark the tracked half once and reuse both results across calls:
// measured on llvm-project (184181 tracked files), this — not the git
// subprocess calls themselves — is the larger remaining cost once
// enumeration is cached (§26 C-28/item 5).
func FilterTracked(root string, tracked map[string]string, scope Scope) (metas []FileMeta, markerPaths []string) {
	exclude := mergedExclude(scope)
	markers := mergedMarkers(scope)
	for relPath, hash := range tracked {
		if included(relPath, exclude) {
			metas = append(metas, FileMeta{Path: relPath, BlobHash: hash, Size: statSize(root, relPath)})
		}
		if isMarker(relPath, markers) {
			markerPaths = append(markerPaths, relPath)
		}
	}
	sort.Slice(metas, func(i, j int) bool { return metas[i].Path < metas[j].Path })
	return metas, markerPaths
}

// FilterUntracked is FilterTracked's counterpart for the untracked
// half, which is never cached (§26 C-28/item 5) but still needs
// filtering and marker detection on every call.
func FilterUntracked(root string, untracked []string, scope Scope) (metas []FileMeta, markerPaths []string) {
	exclude := mergedExclude(scope)
	markers := mergedMarkers(scope)
	for _, relPath := range untracked {
		if included(relPath, exclude) {
			metas = append(metas, FileMeta{Path: relPath, BlobHash: "", Size: statSize(root, relPath)})
		}
		if isMarker(relPath, markers) {
			markerPaths = append(markerPaths, relPath)
		}
	}
	sort.Slice(metas, func(i, j int) bool { return metas[i].Path < metas[j].Path })
	return metas, markerPaths
}

// MergeSortedMetas merges a and b — each already sorted by Path, which
// FilterTracked and FilterUntracked both guarantee — into one sorted
// slice in O(n+m), rather than concatenating and re-sorting the
// combined result. Tracked and untracked paths never overlap (git lists
// a path as one or the other, never both), so this is a plain merge,
// not a dedup. This matters once the cached half alone is in the
// hundreds of thousands of entries (§26 C-28/item 5): re-sorting it on
// every call it is reused would give back most of what caching it saved.
func MergeSortedMetas(a, b []FileMeta) []FileMeta {
	out := make([]FileMeta, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if a[i].Path <= b[j].Path {
			out = append(out, a[i])
			i++
		} else {
			out = append(out, b[j])
			j++
		}
	}
	out = append(out, a[i:]...)
	out = append(out, b[j:]...)
	return out
}

// included reports whether relPath survives scanning: everything is a
// candidate by default (directive 017 §2 — the scanner no longer has a
// coverage default keyed on extension or filename shape; that decision
// moved to the exclusion module, first in giraffe/internal/lang's
// chain) except what an exclusion pattern (default plus project-level,
// §19) removes. A negated pattern wins over a plain one, so an operator
// can pull a specific file back in without having to fight the
// built-in patterns.
func included(relPath string, exclude []Pattern) bool {
	keep := true
	for _, p := range exclude {
		if neg, plain := p.negation(); neg {
			if plain.excludes(relPath) {
				keep = true
			}
			continue
		}
		if p.excludes(relPath) {
			keep = false
		}
	}
	return keep
}

// Discover resolves, from one git-based enumeration, both the
// scope-filtered candidate set List alone would return and every marker
// file path MarkerFiles alone would return.
//
// Measured against a Go corpus two orders of magnitude past gekka (§23
// Step 1): Refresh calling List and MarkerFiles independently paid for
// `git ls-files` (tracked and untracked) twice, at roughly the combined
// cost of everything else Refresh does. Both need the same two lists —
// List filters them by exclusion pattern, MarkerFiles filters them by
// marker name — so fetching once and deriving both removes the duplication
// structurally rather than by remembering to call one before the other.
func Discover(ctx context.Context, root string, scope Scope) (metas []FileMeta, markerPaths []string, err error) {
	tracked, untracked, err := enumerate(ctx, root)
	if err != nil {
		return nil, nil, err
	}
	metas, markerPaths = Combine(root, tracked, untracked, scope)
	return metas, markerPaths, nil
}

func trackedFiles(ctx context.Context, root string) (map[string]string, error) {
	out, err := runGit(ctx, root, "ls-files", "-s")
	if err != nil {
		return nil, err
	}
	files := make(map[string]string)
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		// "<mode> <blob> <stage>\t<path>"
		tab := strings.IndexByte(line, '\t')
		if tab < 0 {
			return nil, fmt.Errorf("unexpected `git ls-files -s` line: %q", line)
		}
		fields := strings.Fields(line[:tab])
		if len(fields) != 3 {
			return nil, fmt.Errorf("unexpected `git ls-files -s` line: %q", line)
		}
		files[line[tab+1:]] = fields[1]
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read `git ls-files -s` output: %w", err)
	}
	return files, nil
}

func untrackedFiles(ctx context.Context, root string) ([]string, error) {
	out, err := runGit(ctx, root, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	var files []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if line := sc.Text(); line != "" {
			files = append(files, line)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read `git ls-files --others` output: %w", err)
	}
	return files, nil
}

// streamGitLines runs git with args under root and calls onLine for
// each line of stdout as it is read from the pipe, rather than
// waiting for the whole process to exit before anything is available
// — the streaming counterpart to runGit (directive 019 §2: "a
// subprocess's stdout is a pipe and can be read incrementally").
// onLine returning a non-nil error (a malformed line, the same
// condition trackedFiles/untrackedFiles already treat as fatal) stops
// the read and kills the subprocess, rather than leaving it blocked
// writing into a pipe nothing is draining anymore.
func streamGitLines(ctx context.Context, root string, onLine func(line string) error, args ...string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = root
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}

	sc := bufio.NewScanner(stdout)
	var lineErr error
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		if err := onLine(line); err != nil {
			lineErr = err
			cancel()
			break
		}
	}
	scanErr := sc.Err()
	waitErr := cmd.Wait()

	if lineErr != nil {
		return lineErr
	}
	if scanErr != nil {
		return fmt.Errorf("read git %s output: %w", strings.Join(args, " "), scanErr)
	}
	if waitErr != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), waitErr, bytes.TrimSpace(stderr.Bytes()))
	}
	return nil
}

// StreamTracked runs `git ls-files -s`, filtering and marking each
// path against scope as it is read from git's own output — the same
// per-path decision FilterTracked makes over an already-complete map
// — rather than waiting for the whole tracked set before filtering
// anything (directive 019 §2: filtering needs nothing but the one
// path in front of it, so there is nothing to wait for). onMeta is
// called for every path scope keeps, in whatever order git prints
// them (git's own index order, not a guarantee of this function's);
// onMarker for every path that is also a marker basename, filtered or
// not (§20 C-3, unaffected by this directive). Neither callback runs
// concurrently with the other — both run on the goroutine that calls
// this function.
func StreamTracked(ctx context.Context, root string, scope Scope, onMeta func(FileMeta), onMarker func(path string)) error {
	exclude := mergedExclude(scope)
	markers := mergedMarkers(scope)
	return streamGitLines(ctx, root, func(line string) error {
		tab := strings.IndexByte(line, '\t')
		if tab < 0 {
			return fmt.Errorf("unexpected `git ls-files -s` line: %q", line)
		}
		fields := strings.Fields(line[:tab])
		if len(fields) != 3 {
			return fmt.Errorf("unexpected `git ls-files -s` line: %q", line)
		}
		relPath, hash := line[tab+1:], fields[1]
		if included(relPath, exclude) {
			onMeta(FileMeta{Path: relPath, BlobHash: hash, Size: statSize(root, relPath)})
		}
		if isMarker(relPath, markers) {
			onMarker(relPath)
		}
		return nil
	}, "ls-files", "-s")
}

// StreamUntracked is StreamTracked's counterpart for `git ls-files
// --others --exclude-standard` — the untracked half, which has no
// comparably cheap freshness signal and so is never cached (§26
// C-28/item 5, §27 C-34), meaning it streams on every call, cache hit
// or miss, not only a cold one.
func StreamUntracked(ctx context.Context, root string, scope Scope, onMeta func(FileMeta), onMarker func(path string)) error {
	exclude := mergedExclude(scope)
	markers := mergedMarkers(scope)
	return streamGitLines(ctx, root, func(line string) error {
		relPath := line
		if included(relPath, exclude) {
			onMeta(FileMeta{Path: relPath, BlobHash: "", Size: statSize(root, relPath)})
		}
		if isMarker(relPath, markers) {
			onMarker(relPath)
		}
		return nil
	}, "ls-files", "--others", "--exclude-standard")
}

func runGit(ctx context.Context, root string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, bytes.TrimSpace(stderr.Bytes()))
	}
	return out, nil
}
