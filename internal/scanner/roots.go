package scanner

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
)

// markerNames are the marker files that identify a project root
// (specifications/giraffe.md §20 C-3). A directory containing one of
// these is a root; roots are discovered from the same file list `git
// ls-files` already produced, not configured.
//
// Markers left out rather than guessed at: any marker whose declared-name
// field would need its own parser to be useful (Cargo.toml's [package]
// name, package.json's "name", pom.xml's <artifactId>) is still a valid
// root boundary here — it just does not feed a language position, since
// no language module for that ecosystem exists in this version.
var markerNames = []string{"go.mod", "Cargo.toml", "package.json", "pom.xml", "build.sbt", "*.csproj"}

func isMarker(relPath string) bool {
	base := path.Base(relPath)
	for _, m := range markerNames {
		if ok, _ := path.Match(m, base); ok {
			return true
		}
	}
	return false
}

// MarkerFiles returns every marker file's path in the repository,
// regardless of scope. Root discovery must see go.mod even though an
// exclude pattern could otherwise drop it — a marker is not itself an
// extraction input (§20 C-3), so it needs its own, unfiltered
// enumeration rather than reusing List's scoped result.
//
// Called on its own, this pays for `git ls-files` (tracked and
// untracked) a second time on top of whatever already called List — see
// Discover for the single-enumeration path a caller needing both should
// use instead.
func MarkerFiles(ctx context.Context, root string) ([]string, error) {
	tracked, untracked, err := enumerate(ctx, root)
	if err != nil {
		return nil, err
	}
	return markersOf(tracked, untracked), nil
}

func markersOf(tracked map[string]string, untracked []string) []string {
	var markers []string
	for p := range tracked {
		if isMarker(p) {
			markers = append(markers, p)
		}
	}
	for _, p := range untracked {
		if isMarker(p) {
			markers = append(markers, p)
		}
	}
	return markers
}

// MarkerDirs returns every directory that contains a marker file —
// repo-relative, "." for a marker at the scan root itself. Ordered
// longest-path-first, so the nearest (most specific) enclosing marker is
// found first by OwningDir.
func MarkerDirs(markerPaths []string) []string {
	seen := make(map[string]bool)
	for _, p := range markerPaths {
		seen[path.Dir(p)] = true
	}
	dirs := make([]string, 0, len(seen))
	for d := range seen {
		dirs = append(dirs, d)
	}
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	return dirs
}

// OwningDir returns the nearest marker directory enclosing relPath, or ""
// if no discovered root owns it (§20 C-3, "a file under no root at all").
func OwningDir(relPath string, markerDirs []string) string {
	for _, d := range markerDirs {
		if d == "." {
			return d
		}
		if relPath == d || strings.HasPrefix(relPath, d+"/") {
			return d
		}
	}
	return ""
}

// ProjectNames maps each marker directory to the project name it
// contributes: the directory's own basename, or rootBase for a marker at
// the scan root itself (path.Base(".") is not useful).
//
// Chosen over the marker's own declared name (e.g. go.mod's module path)
// for uniformity: every marker type would otherwise need its own
// name-extraction rule, and the directory basename is already what a
// caller types into --query 'project:...' naturally. The Go module path
// is not lost — it still feeds the "module" position attribute, per file,
// exactly as before.
func ProjectNames(markerDirs []string, rootBase string) map[string]string {
	names := make(map[string]string, len(markerDirs))
	for _, d := range markerDirs {
		if d == "." {
			names[d] = rootBase
			continue
		}
		names[d] = path.Base(d)
	}
	return names
}

// ProjectFor resolves the project name for one file: the name of its
// nearest enclosing marker directory, or fallback if no marker owns it.
func ProjectFor(relPath string, markerDirs []string, names map[string]string, fallback string) string {
	dir := OwningDir(relPath, markerDirs)
	if dir == "" {
		return fallback
	}
	return names[dir]
}

// CollisionError is returned by CheckNameCollisions. It is its own type,
// not a plain error, so a caller can tell a name collision — a
// configuration problem with one clear fix — apart from a runtime
// failure (I/O, git) and exit accordingly (§21 C-8: consistent
// treatment means the same exit behaviour, not just the same message
// shape).
type CollisionError struct {
	Name         string // the project name both paths would produce
	PathA, PathB string
}

func (e *CollisionError) Error() string {
	return fmt.Sprintf("two project roots share the name %q: %s and %s", e.Name, e.PathA, e.PathB)
}

// CheckNameCollisions errors, naming both paths, if two different
// directories in names would produce the same project name (§21 C-8).
//
// A project-name collision is a hard error wherever it occurs: two
// nested marker directories within one tree (names here is
// ProjectNames's output) or two configured roots across repositories
// (names here is one root path -> its own project name). Before §21
// C-8 only the cross-root case was checked; a same-name intra-tree
// collision merged silently under one project: value, which is the
// same "filter applied without being stated" failure §5 rules out —
// just reached a different way. One detection now covers both.
func CheckNameCollisions(names map[string]string) error {
	dirs := make([]string, 0, len(names))
	for d := range names {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs) // deterministic regardless of map iteration order (§3 C5)

	byName := make(map[string]string, len(names))
	for _, d := range dirs {
		name := names[d]
		if prev, ok := byName[name]; ok {
			return &CollisionError{Name: name, PathA: prev, PathB: d}
		}
		byName[name] = d
	}
	return nil
}
