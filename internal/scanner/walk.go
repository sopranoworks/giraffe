package scanner

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// HasGitDir reports whether root has its own .git entry — a directory
// for an ordinary checkout, a file for a linked worktree or submodule
// (both make git commands meaningful here). A root with neither has no
// git repository of its own (directive 021 item 1): enumeration falls
// back to Walk for everything, rather than asking git a question it can
// only answer with "fatal: not a git repository". Checked structurally,
// once, rather than by running git and classifying its failure —
// cheaper (no subprocess at all) and does not depend on parsing git's
// own error text. Does not walk upward looking for an ancestor .git the
// way git's own repository discovery does: a giraffe scan root is
// always given explicitly, never inferred.
func HasGitDir(root string) bool {
	_, err := os.Lstat(filepath.Join(root, ".git"))
	return err == nil
}

// WalkCache is Walk's own incremental state, opaque to every caller but
// this package — persisted across invocations by index.Refresh (§3 C1:
// no resident process, so a directory's mtime is only useful to the
// next call if it is written down). The zero value is a valid "nothing
// known yet" cache (Go policy G20): Walk treats every directory as
// unseen and reads it fresh.
type WalkCache struct {
	Dirs map[string]WalkDirEntry // key: root-relative directory path, "." for root itself
}

// WalkDirEntry is one directory's own state as of the call that found
// it: its mtime, and the file and subdirectory basenames it held. Read
// again, unread, on a later call whose fresh stat of the same directory
// still matches ModTime — see Walk's own doc comment for why that is a
// sound thing to trust rather than merely a fast one.
type WalkDirEntry struct {
	ModTime int64
	Files   []string
	Subdirs []string
}

// walkPruneDirs are the top-level directory names Walk skips outright —
// neither their own mtime nor their contents ever enter WalkCache or
// the returned file list — derived from defaultExclude's own
// directory-shaped entries (the trailing-"/" ones, which Pattern.excludes
// already only matches at the root: "vendor/" matches relPath=="vendor",
// never "sub/vendor"), so this pruning is always a strict subset of what
// filtering would have removed anyway, never a second, independent
// opinion about what to exclude.
//
// Not scope.Exclude — a project's own .giraffe/exclude — even though
// pruning it too would save more: a negated pattern there
// (Pattern.negation) can reach back into an otherwise-excluded
// directory to re-include one specific path (§25 C-26 sits beside the
// same mechanism), and pruning during the walk itself would make that
// path unreachable regardless — a silent "does not exist" for exactly
// the case an operator wrote the negation to prevent, which §3 rules
// out everywhere. defaultExclude is a fixed literal list with no
// negation of its own, so pruning by it alone is unconditionally safe.
// ".git" is pruned at every depth, not just the root, unconditionally:
// no legitimate content ever lives in a directory of that name.
var walkPruneDirs = func() map[string]bool {
	m := make(map[string]bool)
	for _, p := range defaultExclude {
		s := string(p)
		if trimmed, ok := strings.CutSuffix(s, "/"); ok && !strings.Contains(trimmed, "/") {
			m[trimmed] = true
		}
	}
	return m
}()

// Walk resolves every regular file (and symlink — never followed, so a
// symlink to a directory cannot create a cycle) under root by reading
// the directory tree directly, rather than asking git. Item 1's own
// mechanism for a root with no .git of its own, reused by item 2 for
// the untracked half of an ordinary git repository via the same
// recursion: "one walker, two callers".
//
// prev is the previous call's own returned WalkCache — the zero value
// on a first call, or whenever no prior cache is available. next is
// what the caller should persist for the call after this one.
//
// A directory whose fresh stat mtime still matches prev's own record
// for it is not re-read (os.ReadDir) — its cached file and subdirectory
// names are reused directly — but is still visited and stat'd, and
// every one of its subdirectories is still walked the same way:
// recursion does not stop at an unchanged parent, because a change
// three levels down only moves that directory's own mtime, never an
// ancestor's (item 2's own worked example). This is what makes the
// cache sound rather than merely fast: it is keyed on exactly the fact
// that changes when a directory's own entries do (POSIX updates a
// directory's mtime on any add, remove, or rename within it), not on a
// proxy that can lag behind that the way .git/index can lag the working
// tree (§27 C-34) — a directory the cache confirms unchanged genuinely
// still holds the files and subdirectories it held last call, so
// nothing here needs to re-verify a cache hit against disk the way
// index/enum.go's existingMarkers must for the tracked half's own
// HEAD-adjacent cache.
//
// What this misses, named per item 2's own requirement:
//   - mtime granularity: two changes inside one directory closer
//     together than the filesystem clock can distinguish look like one
//     write. Nothing is lost — every file either change touched is
//     still among the directory's freshly read children — only the
//     distinction between them.
//   - a file modified in place, which never moves its own directory's
//     mtime at all. Not this cache's job: classify's own per-file stat
//     comparison (directive 013, §20 C-2) runs over every path this
//     function returns exactly as it would over any other, and catches
//     it there.
//   - a filesystem that does not update a directory's mtime on
//     add/remove/rename inside it at all (some FAT variants, some
//     network mounts). Degrades silently to a full read of that subtree
//     on every call — slower, never wrong: nothing here trusts a cache
//     entry for a directory whose own mtime it cannot compare.
//
// ctx bounds this the same way it bounds a git subprocess elsewhere in
// this package (§3 C3) — checked once per directory visited, so a
// pathological tree is interrupted rather than run to completion
// regardless of the caller's own timeout.
func Walk(ctx context.Context, root string, prev WalkCache) (files []string, next WalkCache, err error) {
	next = WalkCache{Dirs: make(map[string]WalkDirEntry)}
	err = walkDir(ctx, root, ".", prev, &next, &files)
	sort.Strings(files) // deterministic regardless of directory read order (§3 C5)
	return files, next, err
}

func walkDir(ctx context.Context, absRoot, relDir string, prev WalkCache, next *WalkCache, files *[]string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	absDir := filepath.Join(absRoot, relDir)
	info, err := os.Stat(absDir)
	if err != nil {
		if os.IsNotExist(err) && relDir != "." {
			// Vanished between the parent's read and this visit (e.g. a
			// race with an editor's save-as-rename) — the same race
			// statSize already tolerates for a file, here for a
			// directory: left out of the result, not fatal.
			return nil
		}
		return err
	}
	mtime := info.ModTime().UnixNano()

	var fileNames, subdirNames []string
	if cached, ok := prev.Dirs[relDir]; ok && cached.ModTime == mtime {
		fileNames, subdirNames = cached.Files, cached.Subdirs
	} else {
		entries, err := os.ReadDir(absDir)
		if err != nil {
			return err
		}
		for _, de := range entries {
			name := de.Name()
			switch {
			case de.IsDir():
				if name == ".git" {
					continue
				}
				if relDir == "." && walkPruneDirs[name] {
					continue
				}
				subdirNames = append(subdirNames, name)
			case de.Type().IsRegular(), de.Type()&os.ModeSymlink != 0:
				fileNames = append(fileNames, name)
			}
		}
	}
	next.Dirs[relDir] = WalkDirEntry{ModTime: mtime, Files: fileNames, Subdirs: subdirNames}

	for _, f := range fileNames {
		*files = append(*files, path.Join(relDir, f))
	}
	for _, d := range subdirNames {
		if err := walkDir(ctx, absRoot, path.Join(relDir, d), prev, next, files); err != nil {
			return err
		}
	}
	return nil
}
