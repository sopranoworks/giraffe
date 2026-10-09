package scanner

import (
	"path"
	"sort"
	"strings"
)

// markerNames are the root markers scanner recognises on its own
// (specifications/giraffe.md §20 C-3): every ecosystem giraffe has no
// registered language module for yet, so there is no module to move
// this knowledge into. go.mod is deliberately not here — it moved to
// golang.Marker (directive 017 §3: "recognising go.mod is Go's
// knowledge") — a caller merges it, and any other registered module's
// own marker, back in via Scope.Markers (see mergedMarkers in
// scanner.go); this is the one list with no other owner, not a second
// copy of a name a module already owns.
//
// Markers left out rather than guessed at: any marker whose declared-name
// field would need its own parser to be useful (Cargo.toml's [package]
// name, package.json's "name", pom.xml's <artifactId>) is still a valid
// root boundary here — it just does not feed a language position, since
// no language module for that ecosystem exists in this version.
var markerNames = []string{"Cargo.toml", "package.json", "pom.xml", "build.sbt", "*.csproj"}

func isMarker(relPath string, markers []string) bool {
	base := path.Base(relPath)
	for _, m := range markers {
		if ok, _ := path.Match(m, base); ok {
			return true
		}
	}
	return false
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
// contributes: the directory's path relative to the scan root, or
// rootBase for a marker at the scan root itself (path.Base(".") is not
// useful). A path is unique where a basename is not: gcp/observability
// and interop/observability, client/pkg and pkg are two projects each,
// as a multi-module repository has them (directive 077; 003 made two
// basenames alike an error, and no index was built at all). A caller
// still types the last element alone — --query 'project:util' selects
// every project whose name ends in util (query.Filter.Match).
//
// Chosen over the marker's own declared name (e.g. go.mod's module path)
// for uniformity: every marker type would otherwise need its own
// name-extraction rule. The Go module path is not lost — it still feeds
// the "module" position attribute, per file, exactly as before.
func ProjectNames(markerDirs []string, rootBase string) map[string]string {
	names := make(map[string]string, len(markerDirs))
	for _, d := range markerDirs {
		if d == "." {
			names[d] = rootBase
			continue
		}
		names[d] = d
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
