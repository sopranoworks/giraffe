// Package scanner owns all file access (specifications/giraffe.md §18).
// The file set is resolved once — StreamTracked's tracked paths and
// blob hashes from `git ls-files -s` where a root has its own .git
// (HasGitDir), Walk's own incremental directory read for everything
// else (directive 021: no git at all, or the untracked remainder of an
// ordinary repository) — without opening anything, so a caller can
// decide, from blob hashes alone, which files are already indexed and
// can be left unread (§9: "unchanged blob → file is not reopened").
// Which route a call takes is this package's own decision, invisible to
// a caller: enumeration returns a set of paths, and how it obtained
// them is an implementation choice. Open then returns a File whose
// content is read from disk only when a caller actually asks for it
// (directive 017 §1). Language modules never open a file themselves;
// they receive File values.
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
	"strings"
)

// FileMeta is one candidate file's identity, without its contents.
type FileMeta struct {
	Path     string // repo-relative, normalised
	BlobHash string // git blob hash; "" for untracked or walked files

	// Size is the file's byte size, stat'd once when this FileMeta is
	// produced (StreamTracked/FilterWalked, below) — a declaration
	// search never needed it, but a filename result on a corpus with no
	// extractor has nothing else to tell a caller deciding whether to
	// open the file directly or narrow it first (directive 009 item 3).
	// 0 if the stat failed (gone between enumeration and filtering);
	// callers already tolerate that race elsewhere (index.Refresh's own
	// extraction stat).
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

// defaultExclude is the baseline every project starts with: dependency and
// build-output trees a reuse search gets nothing from. It is the same set
// specifications/giraffe.md §11 excludes from its sizing measurement, kept
// here so a real run stays comparable to that evidence. Per-project
// configuration adds to this; it does not replace it. Written in
// gitignore syntax (directive 023 item 1: one syntax, one parser) —
// parsed by defaultExcludeRules below exactly as any .gitignore or
// .giraffe/exclude line is.
var defaultExclude = []string{
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

// GitignoreRule is one exclusion rule discovered from a .gitignore
// file, scoped to the directory that declared it (directive 022 item
// 1: nested .gitignore files apply to their own subtree). Opaque
// outside this package — index/enum.go holds the slice Walk returns
// only to hand it back to FilterWalked, never inspecting it — the same
// shape defaultExclude and Scope.Exclude are normalised into
// internally (dir "."), so pruning and filtering share one algorithm
// (included, below) regardless of where a rule came from.
type GitignoreRule struct {
	dir     string
	pattern Pattern
}

// scopedTo reports the part of relPath below dir, and whether relPath
// is under dir at all — "." matches everything (defaultExclude and
// Scope.Exclude are always scan-root-relative), any other dir requires
// relPath to be dir itself or begin with "dir/". A gitignore rule only
// ever applies within the subtree of the .gitignore file that declared
// it; this is the one place that boundary is enforced.
func scopedTo(relPath, dir string) (suffix string, ok bool) {
	if dir == "." {
		return relPath, true
	}
	if relPath == dir {
		return "", true
	}
	if strings.HasPrefix(relPath, dir+"/") {
		return relPath[len(dir)+1:], true
	}
	return "", false
}

func defaultExcludeRules() []GitignoreRule {
	rules := make([]GitignoreRule, 0, len(defaultExclude))
	for _, raw := range defaultExclude {
		p, ok := ParsePattern(raw)
		if !ok {
			// Every entry in defaultExclude is a fixed literal, known
			// valid gitignore syntax — this can only fire if a future
			// edit to that list breaks it, which a test catches
			// (TestDefaultExclude_AllPatternsParse) well before this
			// would ever run against a real tree.
			panic("scanner: defaultExclude entry does not parse: " + raw)
		}
		rules = append(rules, GitignoreRule{dir: ".", pattern: p})
	}
	return rules
}

func scopeExcludeRules(scope Scope) []GitignoreRule {
	rules := make([]GitignoreRule, 0, len(scope.Exclude))
	for _, p := range scope.Exclude {
		rules = append(rules, GitignoreRule{dir: ".", pattern: p})
	}
	return rules
}

// baseRules is defaultExclude plus scope.Exclude, both scan-root-
// relative — the tracked half's whole rule set (StreamTracked below):
// .gitignore is deliberately not part of it. Real git does not
// retroactively drop a file from `ls-files -s` because it was later
// added to .gitignore (a common, intentional case — a file tracked
// before a build-output pattern existed), so the tracked half must
// not either; only the walked half's rule set (walkedRules) adds
// .gitignore on top of this same base.
func baseRules(scope Scope) []GitignoreRule {
	rules := defaultExcludeRules()
	return append(rules, scopeExcludeRules(scope)...)
}

// walkedRules is the walked half's full rule set, in override order:
// defaultExclude first, then whatever .gitignore contributed (item
// 1) — Walk discovers deeper .gitignore files later, so a nested rule
// naturally overrides a shallower one the same way real git layers
// them — and scope.Exclude last, so an operator's own
// .giraffe/exclude keeps the final say it already had before this
// directive.
func walkedRules(scope Scope, gitignore []GitignoreRule) []GitignoreRule {
	rules := defaultExcludeRules()
	rules = append(rules, gitignore...)
	return append(rules, scopeExcludeRules(scope)...)
}

// Scope selects what enumeration considers: the paths left out.
// Extension — or extensionless shape — no longer gates the candidate
// set itself (§24 C-18, extended by directive 017 §2): every path
// exclusion leaves standing is a candidate, full stop. Which of those a
// language module actually wants, including whether to exclude it from
// the index entirely, is decided downstream, per file, by the
// registered chain (giraffe/internal/lang) — the scanner names no
// extension and no filename shape anywhere in this package.
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

// GitIndexFingerprint reports the mtime and size of root's .git/index —
// git's own record of the tracked file set and staged changes. Any
// operation that can change what StreamTracked returns (add, rm,
// checkout, merge, reset, commit, stash) rewrites this file, so it is a
// correct freshness signal for the tracked half of enumeration and,
// unlike HEAD, catches a staged-but-uncommitted add or remove too.
//
// It says nothing about anything Walk resolves: a file created or
// deleted in the working tree without ever being staged changes neither
// this fingerprint nor HEAD (§26 C-28/item 5 — "a working tree that has
// moved without HEAD moving"). Walk has its own, different freshness
// mechanism (WalkCache, directive 021 item 2) — directory mtimes, not
// this file at all.
//
// ok is false if .git/index cannot be stat'd at all (a linked worktree
// or submodule whose .git is a file pointing elsewhere, a repository
// with no commits and nothing ever staged, or no .git at all) — the
// caller's only correct response is to skip the cache, not to treat
// this as an error: enumeration that always re-runs git is still
// correct, only slower.
func GitIndexFingerprint(root string) (modTime, size int64, ok bool) {
	info, err := os.Stat(filepath.Join(root, ".git", "index"))
	if err != nil {
		return 0, 0, false
	}
	return info.ModTime().UnixNano(), info.Size(), true
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

// included reports whether relPath survives scanning: everything is a
// candidate by default (directive 017 §2 — the scanner no longer has a
// coverage default keyed on extension or filename shape; that decision
// moved to the exclusion module, first in giraffe/internal/lang's
// chain) except what an exclusion rule (default, project-level, or —
// for the walked half only — .gitignore, §19, directive 022 item 1)
// removes. A negated pattern wins over a plain one, so an operator
// can pull a specific file back in without having to fight the
// built-in patterns. rules is checked in order, each scoped to its own
// dir (GitignoreRule's own doc comment) — this is the one algorithm
// both file-level filtering and directory-level pruning (walk.go's
// prunes) share. isDir is relPath's own kind — true only from prunes,
// which is only ever asked about a directory it is deciding whether to
// descend into; false from StreamTracked and FilterWalked, which only
// ever hand this a file (Pattern.excludes' own doc comment on why a
// dirOnly rule needs to know).
func included(relPath string, rules []GitignoreRule, isDir bool) bool {
	keep := true
	for _, r := range rules {
		suffix, ok := scopedTo(relPath, r.dir)
		if !ok {
			continue
		}
		if neg, plain := r.pattern.negation(); neg {
			if plain.excludes(suffix, isDir) {
				keep = true
			}
			continue
		}
		if r.pattern.excludes(suffix, isDir) {
			keep = false
		}
	}
	return keep
}

// streamGitLines runs git with args under root and calls onLine for
// each line of stdout as it is read from the pipe, rather than
// waiting for the whole process to exit before anything is available
// — the streaming counterpart to a buffered git call (directive 019
// §2: "a subprocess's stdout is a pipe and can be read incrementally").
// onLine returning a non-nil error (a malformed line) stops the read
// and kills the subprocess, rather than leaving it blocked writing
// into a pipe nothing is draining anymore.
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

// StreamTracked runs `git ls-files -s` under root, filtering and marking
// each path against scope as it is read from git's own output, rather
// than waiting for the whole tracked set before filtering anything
// (directive 019 §2: filtering needs nothing but the one path in front
// of it, so there is nothing to wait for). Only meaningful where
// HasGitDir(root) — a caller checks that first, since a root with no
// .git has no tracked half at all.
//
// prefix is prepended to every path this call reports — "" for a
// top-level call, a submodule's own path (as reported by an outer call's
// onGitlink) for a call made inside it (directive 023 item 3: "a
// submodule is enumerated by git, inside itself" — the same function,
// pointed at the submodule's own working tree, with its results folded
// back under the path that names it). Every callback receives the
// prefixed path; only the internal stat (statSize) uses root and the
// unprefixed one, since root is the submodule's own filesystem directory
// when prefix is not empty.
//
// onMeta is called for every path scope keeps; onMarker for every path
// that is also a marker basename, filtered or not (§20 C-3); onGitlink
// for a submodule boundary (gitlinkMode below) — never onMeta, since
// there is nothing here to read (attempting to would try to open the
// submodule's own directory as if it were a file's content), and no
// blob hash comparable to any other tracked file's. Recursing into it,
// if a caller chooses to, is index/enum.go's own decision (runTrackedHalf),
// not this function's — scanner stays one file-access layer, unaware of
// enumeration's own caching or concurrency shape. None of the three
// callbacks runs concurrently with another — all run on the goroutine
// that calls this function, in the order git prints its own output.
const gitlinkMode = "160000"

func StreamTracked(ctx context.Context, root, prefix string, scope Scope, onMeta func(FileMeta), onMarker func(path string), onGitlink func(path string)) error {
	rules := baseRules(scope)
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
		mode, hash, relPath := fields[0], fields[1], line[tab+1:]
		fullPath := path.Join(prefix, relPath)
		if mode == gitlinkMode {
			onGitlink(fullPath)
			return nil
		}
		if included(fullPath, rules, false) {
			onMeta(FileMeta{Path: fullPath, BlobHash: hash, Size: statSize(root, relPath)})
		}
		if isMarker(fullPath, markers) {
			onMarker(fullPath)
		}
		return nil
	}, "ls-files", "-s")
}

// FilterWalked applies scope's exclusion rules, whatever .gitignore
// contributed (gitignore — Walk's own return value, directive 022
// item 1), and marker detection to paths Walk found — the filtering
// step "one walker, two callers" shares (directive 021), the same
// per-path decision StreamTracked makes for its own git-sourced paths,
// over Walk's output instead of git's. skip is checked first: the
// tracked path set a git repository's own StreamTracked half already
// covered — at any depth, including every submodule it recursed into
// (directive 023 item 3) — so a path already reported there is never
// reconsidered here (a git repository's tracked and walked halves must
// never emit the same path — nothing downstream de-duplicates). skip
// is nil or empty for a root with no tracked half at all (item 1's
// no-git case, where Walk covers everything).
func FilterWalked(root string, paths []string, skip map[string]bool, scope Scope, gitignore []GitignoreRule, onMeta func(FileMeta), onMarker func(path string)) {
	rules := walkedRules(scope, gitignore)
	markers := mergedMarkers(scope)
	for _, p := range paths {
		if skip[p] {
			continue
		}
		if included(p, rules, false) {
			onMeta(FileMeta{Path: p, BlobHash: "", Size: statSize(root, p)})
		}
		if isMarker(p, markers) {
			onMarker(p)
		}
	}
}
