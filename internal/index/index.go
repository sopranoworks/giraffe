// Package index maintains giraffe's cache: one fact per file — its
// freshness fingerprint and the entries extracted from it last time it
// was read (specifications/giraffe.md §23 C-14). The cache is local and
// disposable, never committed and never shared: a clone arrives with
// none, the first `find` builds it, and it is thrown away and rebuilt
// whole the moment anything about its shape does not parse — see Load.
package index

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"giraffe/internal/entry"
)

// Dir is where giraffe's own files live, at the repository root: hand-
// written configuration directly under it (roots, exclude — authored,
// and the operator's choice whether to commit them), the derived cache
// under cacheDir (never committed — §23: "make the split obvious from
// the layout, a directory the operator can ignore wholesale").
const Dir = ".giraffe"

// cacheDir holds only derived, disposable state. Nothing under it is
// ever read as configuration and nothing under it is ever committed.
const cacheDir = "cache"

// cacheFilename is the cache file itself.
const cacheFilename = "entries"

// Record is one file's cached state: its freshness fingerprint (§20
// C-2 — mtime and size, unaffected by this decommit; a committed,
// blob-hash-keyed identity was the part that assumed sharing, and that
// assumption is what §23 C-14 withdraws) and the entries extracted from
// it the last time it was read.
type Record struct {
	ModTime int64 // UnixNano
	Size    int64
	Entries []entry.Entry
}

// Load reads the local cache, grouped by path. A missing cache is not
// an error: nothing has been generated yet, and the caller rebuilds
// everything — the same self-healing §9 already promised for a missing
// index extends unchanged to a missing cache.
//
// §23 C-14 changes what happens when the cache exists but does not
// parse. Directive 004 made a stale-shaped *line* self-healing (skip
// it, re-extract that one file) because the index was shared and a
// format bump could not be allowed to hard-break every clone's
// committed copy on upgrade. Nothing depends on this cache but the
// invocation that wrote it, so that graduated repair is no longer
// buying anything: the first line that fails to decode discards the
// whole cache and every file is re-extracted, which is simpler and no
// more expensive than the version it replaces once nobody downstream
// is relying on the parts that did decode.
func Load(root string) (map[string]Record, error) {
	data, err := os.ReadFile(filepath.Join(root, Dir, cacheDir, cacheFilename))
	if os.IsNotExist(err) {
		return map[string]Record{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read cache: %w", err)
	}

	records := make(map[string]Record)
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		path, mtime, size, e, err := decodeLine(line)
		if err != nil {
			// A shape this build does not recognise — a previous
			// giraffe version's cache, or on-disk corruption. Neither
			// is worth diagnosing line by line: discard the whole
			// cache and let a normal Refresh rebuild it (§23 C-14).
			return map[string]Record{}, nil
		}
		rec := records[path]
		rec.ModTime, rec.Size = mtime, size
		if e.Name != "" { // "" marks a presence line (see Save): a file with no entries
			rec.Entries = append(rec.Entries, e)
		}
		records[path] = rec
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read cache: %w", err)
	}
	return records, nil
}

// Save writes records as the local cache: one line per entry, sorted by
// path then name then kind. Sorting no longer serves a merge (§23
// C-14 — nothing merges a file nobody else reads), only reproducible
// output for the same tree, which is what tells a caller a corrupt
// cache apart from a real change (§3 C5).
//
// A file with zero entries (every declaration unexported, or none at
// all) still gets exactly one line, carrying its path and freshness
// fingerprint but no name — otherwise it would have nothing to anchor
// its freshness to and would be re-read on every single run even
// though nothing about it ever changes.
func Save(root string, records map[string]Record) error {
	type row struct {
		entry.Entry
		ModTime int64
		Size    int64
	}
	var rows []row
	for path, rec := range records {
		if len(rec.Entries) == 0 {
			rows = append(rows, row{entry.Entry{Path: path}, rec.ModTime, rec.Size})
			continue
		}
		for _, e := range rec.Entries {
			rows = append(rows, row{e, rec.ModTime, rec.Size})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Path != rows[j].Path {
			return rows[i].Path < rows[j].Path
		}
		if rows[i].Name != rows[j].Name {
			return rows[i].Name < rows[j].Name
		}
		return rows[i].Kind < rows[j].Kind
	})

	var buf strings.Builder
	for _, r := range rows {
		buf.WriteString(encodeLine(r.Entry, r.ModTime, r.Size))
		buf.WriteByte('\n')
	}

	dir := filepath.Join(root, Dir, cacheDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s/%s: %w", Dir, cacheDir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, cacheFilename), []byte(buf.String()), 0o644); err != nil {
		return fmt.Errorf("write cache: %w", err)
	}
	return nil
}

// encodeLine and decodeLine are the cache's one fact-per-line shape:
// path, mtime, size, kind, name, project, scope, exported, position,
// doc, lang, ext — tab-separated. Position holds every language-position
// pair as "name=value", joined by ";". A presence line (see Save)
// carries no name, scope, or exported, since it describes the file,
// not a declaration; only extractable files ever reach here, so lang
// and ext are never "" in a cache line — a file is only ever stat'd,
// cached and read once its extension is one a registered extractor
// recognises (§24 C-18, §25 C-24).
//
// No version field (§23 C-14: "no format version, no backward
// compatibility" — a shape mismatch is Load's job to discard, not this
// function's job to migrate). Adding ext as a 12th field is exactly the
// kind of shape change that relies on that rule: a pre-007 cache (11
// fields) now fails decodeLine's count check and is discarded wholesale,
// not migrated.
func encodeLine(e entry.Entry, mtime, size int64) string {
	fields := []string{
		sanitize(e.Path),
		strconv.FormatInt(mtime, 10),
		strconv.FormatInt(size, 10),
		sanitize(e.Kind),
		sanitize(e.Name),
		sanitize(e.Project),
		sanitize(e.Scope),
		encodeBool(e.Exported),
		encodePosition(e.Position),
		sanitize(e.Doc),
		sanitize(e.Lang),
		sanitize(e.Ext),
	}
	return strings.Join(fields, "\t")
}

func decodeLine(line string) (path string, mtime, size int64, e entry.Entry, err error) {
	fields := strings.Split(line, "\t")
	if len(fields) != 12 {
		return "", 0, 0, entry.Entry{}, fmt.Errorf("expected 12 tab-separated fields, got %d: %q", len(fields), line)
	}
	mtime, err1 := strconv.ParseInt(fields[1], 10, 64)
	size, err2 := strconv.ParseInt(fields[2], 10, 64)
	if err1 != nil || err2 != nil {
		return "", 0, 0, entry.Entry{}, fmt.Errorf("malformed freshness fields: %q", line)
	}
	e = entry.Entry{
		Path:     fields[0],
		Kind:     fields[3],
		Name:     fields[4],
		Project:  fields[5],
		Scope:    fields[6],
		Exported: fields[7] == "1",
		Position: decodePosition(fields[8]),
		Doc:      fields[9],
		Lang:     fields[10],
		Ext:      fields[11],
	}
	return fields[0], mtime, size, e, nil
}

func encodeBool(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// sanitize keeps a field to one line: a cache line is one fact, and a
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
