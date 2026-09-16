// Package index maintains giraffe's cache: one bbolt file per root
// holding extracted entries, the freshness fingerprint that avoids
// re-parsing unchanged files, and a name-trigram posting list that
// narrows substring search without visiting every entry
// (specifications/giraffe.md §30 C-46, C-47). The store is local and
// disposable, never committed and never shared: a clone arrives with
// none, the first `find` builds it, and it is discarded and rebuilt
// whole the moment its schema version does not match — see store.go's
// openStore.
package index

import (
	"strings"

	"giraffe/internal/entry"
)

// Dir is where giraffe's own files live, at the repository root: hand-
// written configuration directly under it (roots, exclude — authored,
// and the operator's choice whether to commit them), the derived cache
// under CacheDir (never committed — §23: "make the split obvious from
// the layout, a directory the operator can ignore wholesale").
const Dir = ".giraffe"

// CacheDir, under Dir, holds only derived, disposable state. Nothing
// under it is ever read as configuration and nothing under it is ever
// committed. Exported since directive 046: the session record
// (internal/session) lives there too, and main names the directory
// for both.
const CacheDir = "cache"

// Record is one file's extraction-cache entry (§20 C-2, unaffected by
// §30's storage change): its freshness fingerprint — mtime and size —
// and the ids of the entries extracted from it last time it was read,
// looked up in the store's entries bucket rather than carried inline.
type Record struct {
	ModTime  int64 // UnixNano
	Size     int64
	EntryIDs []uint64
}

func encodeBool(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// sanitize keeps a field to one line: a stored fact is one line, and a
// tab or newline inside a field would silently create or merge fields.
func sanitize(s string) string {
	s = strings.ReplaceAll(s, "\t", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

func encodePosition(pos []entry.Position) string {
	if len(pos) == 0 {
		return ""
	}
	parts := make([]string, len(pos))
	for i, p := range pos {
		parts[i] = sanitize(p.Name) + "=" + sanitize(p.Value)
	}
	return strings.Join(parts, ";")
}

func decodePosition(s string) []entry.Position {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ";")
	pos := make([]entry.Position, 0, len(parts))
	for _, p := range parts {
		if i := strings.IndexByte(p, '='); i >= 0 {
			pos = append(pos, entry.Position{Name: p[:i], Value: p[i+1:]})
		}
	}
	return pos
}
