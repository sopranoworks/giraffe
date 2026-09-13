// Package scanner owns all file access (specifications/giraffe.md §18).
// List resolves the file set once — tracked paths and blob hashes from
// `git ls-files -s`, untracked paths from `git ls-files --others
// --exclude-standard` — without opening anything, so a caller can decide,
// from blob hashes alone, which files are already indexed and can be left
// unread (§9: "unchanged blob → file is not reopened"). Read then opens
// exactly the paths it is given, with a fixed number of workers, so peak
// open descriptors is computable in advance (§3, §18). Language modules
// never open a file themselves; they receive File values.
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
	"sync"
)

// workers is the fixed concurrency for reading file contents (§3 C4: fixed
// and small, so peak open descriptors is computable in advance).
const workers = 8

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
	Size int64
}

// File is one file's resolved identity and contents, as delivered to a
// language module.
type File struct {
	Path     string
	BlobHash string
	Content  []byte
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
// every other exclusion below, including the two coverage lists (§24
// C-19: "extendable and overridable through .giraffe/exclude" — a
// plain pattern extends either list by excluding more; a negated one
// overrides a list's default by including something it would have
// dropped).
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
	"vendor/",
	"node_modules/",
	"testdata/",
	"examples/",
	"*_test.go",
	"*.pb.go",
}

// knownExtensionless are the extensionless files worth indexing by name
// (§24 C-19): common build and doc files whose name alone says what
// they are. An extensionless file not on this list is skipped — node,
// configure have exactly the shape of a compiled binary, and defaulting
// to include is the dangerous direction. Checked by exact basename,
// before extension is considered at all, so CMakeLists.txt belongs here
// despite technically having a ".txt" extension: its name is what
// identifies it, the same as the others on this list.
//
// The directive's own third example, a.out, is not actually
// extensionless under path.Ext (it has ".out") — named in the
// completion report rather than silently reclassified; ".out" is
// covered by the deny list below instead, so the outcome the directive
// asked for still holds.
var knownExtensionless = []string{
	"Makefile", "Dockerfile", "CMakeLists.txt", "LICENSE", "README",
	"Rakefile", "Gemfile", "Procfile", "Vagrantfile",
}

// deniedExtensions are binary, compiled, and archive formats excluded
// from file-name indexing by default (§24 C-19). The extension already
// declares the type, so a deny list is enough: no content is ever read
// to decide, and everything not on this list is included.
var deniedExtensions = []string{
	".png", ".jpg", ".jpeg", ".gif", ".ico", ".bmp", ".webp",
	".o", ".out", ".so", ".a", ".dylib", ".dll", ".exe", ".class", ".pyc", ".wasm",
	".zip", ".tar", ".gz", ".bz2", ".xz", ".jar", ".war", ".pdf",
	".woff", ".woff2", ".ttf", ".eot", ".mp3", ".mp4", ".mov",
}

// Scope selects what List considers: the paths left out, on top of the
// coverage rules below. Extension no longer gates the candidate set
// itself (§24 C-18) — that gating produced an empty candidate list for
// any repository with no registered extractor, which threw away the
// one property that justified file-name indexing: it needs no parser,
// so it can cover every language today. Which files get an extractor
// run over them is decided downstream, per file, against the registry
// (giraffe/internal/lang), not here.
type Scope struct {
	Exclude []Pattern
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

// List resolves the current file set — tracked and untracked, filtered by
// scope and by the coverage rules above (§24 C-18/C-19) — without reading
// any of it. Order is deterministic (path order), independent of anything
// about how the files were produced.
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
	return filterMetas(root, tracked, untracked, scope), markersOf(tracked, untracked)
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
// and the coverage defaults (§24 C-18/C-19), and separately finds every
// marker file among them regardless of exclusion (§20 C-3: root
// discovery must see go.mod even where an exclude pattern would
// otherwise drop it — markersOf's own rule, reproduced here since a
// cached tracked half needs both without re-deriving one from the
// other).
//
// Exposed so a persistent enumeration cache (index.Refresh) can filter
// and mark the tracked half once and reuse both results across calls:
// measured on llvm-project (184181 tracked files), this — not the git
// subprocess calls themselves — is the larger remaining cost once
// enumeration is cached (§26 C-28/item 5).
func FilterTracked(root string, tracked map[string]string, scope Scope) (metas []FileMeta, markerPaths []string) {
	exclude := mergedExclude(scope)
	for relPath, hash := range tracked {
		if included(relPath, exclude) {
			metas = append(metas, FileMeta{Path: relPath, BlobHash: hash, Size: statSize(root, relPath)})
		}
		if isMarker(relPath) {
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
	for _, relPath := range untracked {
		if included(relPath, exclude) {
			metas = append(metas, FileMeta{Path: relPath, BlobHash: "", Size: statSize(root, relPath)})
		}
		if isMarker(relPath) {
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

// included reports whether relPath survives scanning: the exclusion
// patterns (default plus project-level, §19) first, then — for
// anything no pattern named — the coverage rules above (§24 C-19). A
// negated pattern is checked last and wins over everything above it,
// including the coverage rules, so an operator can pull a specific
// file back in without having to fight the built-in lists.
func included(relPath string, exclude []Pattern) bool {
	keep := coverage(relPath)
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

// coverage is the default answer before any exclude pattern is applied:
// a known extensionless file, or any extensioned file whose extension
// is not on the deny list (§24 C-19). Decided from the path string
// alone — no stat, no open — since a misclassification here costs one
// index line and no I/O, never a wrong read.
func coverage(relPath string) bool {
	base := path.Base(relPath)
	for _, n := range knownExtensionless {
		if base == n {
			return true
		}
	}
	ext := path.Ext(relPath)
	if ext == "" {
		return false // shape of a compiled binary; defaulting to include is the dangerous direction
	}
	for _, d := range deniedExtensions {
		if ext == d {
			return false
		}
	}
	return true
}

// Discover resolves, from one git-based enumeration, both the
// scope-filtered candidate set List alone would return and every marker
// file path MarkerFiles alone would return.
//
// Measured against a Go corpus two orders of magnitude past gekka (§23
// Step 1): Refresh calling List and MarkerFiles independently paid for
// `git ls-files` (tracked and untracked) twice, at roughly the combined
// cost of everything else Refresh does. Both need the same two lists —
// List filters them by extension, MarkerFiles filters them by marker
// name — so fetching once and deriving both removes the duplication
// structurally rather than by remembering to call one before the other.
func Discover(ctx context.Context, root string, scope Scope) (metas []FileMeta, markerPaths []string, err error) {
	tracked, untracked, err := enumerate(ctx, root)
	if err != nil {
		return nil, nil, err
	}
	metas, markerPaths = Combine(root, tracked, untracked, scope)
	return metas, markerPaths, nil
}

// Read opens exactly the given paths, once each, with a fixed pool of
// workers. Callers pass only the paths that actually need (re-)reading —
// typically a subset List returned, after comparing blob hashes against an
// existing index.
func Read(ctx context.Context, root string, paths []string) ([]File, error) {
	results := make([]File, len(paths))
	jobs := make(chan int)
	errc := make(chan error, len(paths))

	n := workers
	if n > len(paths) {
		n = len(paths)
	}
	var wg sync.WaitGroup
	for w := 0; w < n; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				content, err := os.ReadFile(filepath.Join(root, paths[i]))
				if err != nil {
					errc <- fmt.Errorf("read %s: %w", paths[i], err)
					continue
				}
				results[i] = File{Path: paths[i], Content: content}
			}
		}()
	}

	go func() {
		defer close(jobs)
		for i := range paths {
			select {
			case jobs <- i:
			case <-ctx.Done():
				return
			}
		}
	}()

	wg.Wait()
	close(errc)

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	for err := range errc {
		return nil, err // report the first read failure
	}
	return results, nil
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
