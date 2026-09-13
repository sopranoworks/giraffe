// store.go holds the bbolt-backed persistent form (specifications/giraffe.md
// §30 C-46, C-47): entries read through mmap rather than decoded into a
// process-resident structure on every invocation, a name-trigram posting
// list so Search's name-based tiers narrow without visiting every entry,
// and the extraction cache (files) and enumeration cache (enum) kept as
// buckets in the same file rather than separate text files. Schema version
// lives in one bucket; any mismatch — including a file that does not open
// as a valid bbolt database at all — means discard and rebuild, never
// migrate (§23 C-14, unchanged by this directive).
package index

import (
	"encoding/binary"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"

	"giraffe/internal/entry"
)

// storeFilename replaces the two text files (cache/entries, cache/enum)
// directive 011 and earlier left behind; one file, one schema-version
// check, one discard-and-rebuild path.
const storeFilename = "store.bolt"

// schemaVersion changes whenever the bucket layout or entry encoding
// changes shape. No migration is ever written for it (§23 C-14): a
// mismatch means Open discards the file and the caller rebuilds from
// scratch, the same convention the old line-oriented cache used for an
// undecodable line, just cheaper to detect — one key, not a parse
// failure partway through 37MB of text.
const schemaVersion = "1"

var (
	bucketMeta         = []byte("meta")
	bucketEntries      = []byte("entries")
	bucketFiles        = []byte("files")
	bucketEnum         = []byte("enum")
	bucketNameTrigrams = []byte("name_trigrams")
	keySchemaVersion   = []byte("schema_version")
	keyEnumBlob        = []byte("blob")
)

// Store is one root's persistent state: extracted entries, the
// extraction cache that avoids re-parsing unchanged files, the
// enumeration cache, and the name-trigram posting list that narrows
// Search's name-based tiers. Backed by one mmap'd bbolt file; nothing
// here is decoded into a Go object until a caller actually asks for a
// specific entry.
type Store struct {
	db   *bolt.DB
	path string
}

// openStore opens root's store, discarding and recreating it if it does
// not exist, does not open as a valid bbolt file, or was written by a
// different schema version (§23 C-14: never migrated, always rebuilt).
// readOnly=true refuses to create a missing file (returns ok=false
// instead) — used by the fast "nothing changed" path, which must never
// be the reason a store gets created.
func openStore(root string, readOnly bool) (s *Store, ok bool, err error) {
	dir := filepath.Join(root, Dir, cacheDir)
	path := filepath.Join(dir, storeFilename)

	if readOnly {
		if _, statErr := os.Stat(path); statErr != nil {
			return nil, false, nil
		}
	}

	db, openErr := bolt.Open(path, 0o644, &bolt.Options{ReadOnly: readOnly, Timeout: time.Second})
	if openErr != nil {
		if readOnly {
			return nil, false, nil
		}
		// Not a bbolt file at all (or otherwise unreadable): the same
		// "discard and rebuild" the old cache gave any undecodable
		// content (§23 C-14). Remove it and let the caller create fresh.
		_ = os.Remove(path)
		return nil, false, nil
	}

	valid := false
	_ = db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketMeta)
		if b == nil {
			return nil
		}
		valid = string(b.Get(keySchemaVersion)) == schemaVersion
		return nil
	})
	if !valid {
		_ = db.Close()
		if readOnly {
			return nil, false, nil
		}
		_ = os.Remove(path)
		db, openErr = bolt.Open(path, 0o644, &bolt.Options{Timeout: time.Second})
		if openErr != nil {
			return nil, false, openErr
		}
	}
	return &Store{db: db, path: path}, true, nil
}

// createStore makes a fresh store at root, first removing anything
// already there — used when a full rebuild is needed (something
// changed, or the existing store failed to open). mkdir is separate
// from bolt.Open's own file creation because cacheDir may not exist yet
// on a first run.
func createStore(root string) (*Store, error) {
	dir := filepath.Join(root, Dir, cacheDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create %s/%s: %w", Dir, cacheDir, err)
	}
	path := filepath.Join(dir, storeFilename)
	_ = os.Remove(path)
	db, err := bolt.Open(path, 0o644, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, fmt.Errorf("create store: %w", err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{bucketMeta, bucketEntries, bucketFiles, bucketEnum, bucketNameTrigrams} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return tx.Bucket(bucketMeta).Put(keySchemaVersion, []byte(schemaVersion))
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initialise store: %w", err)
	}
	return &Store{db: db, path: path}, nil
}

// Close releases the store's mmap. Safe to call once, after every read
// and iteration the caller intends to do is finished.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// All iterates every entry in the store, decoding each one lazily as
// the caller ranges over it — nothing beyond the entry currently being
// yielded is ever resident as a Go object (§30 C-46). A single read
// transaction spans the whole iteration; the caller must finish (or
// abandon) ranging before Close.
func (s *Store) All() iter.Seq[entry.Entry] {
	return func(yield func(entry.Entry) bool) {
		_ = s.db.View(func(tx *bolt.Tx) error {
			b := tx.Bucket(bucketEntries)
			if b == nil {
				return nil
			}
			c := b.Cursor()
			for k, v := c.First(); k != nil; k, v = c.Next() {
				e, err := decodeEntry(v)
				if err != nil {
					continue // a defensive skip; encodeEntry/decodeEntry are this file's own pair
				}
				if !yield(e) {
					return nil
				}
			}
			return nil
		})
	}
}

// Candidates narrows to entries whose Name could contain word as a
// case-insensitive substring, via the name-trigram posting list (§30
// C-47) — a necessary-but-not-sufficient filter, same principle as
// query.couldShareToken: every genuine substring match is guaranteed to
// appear in the intersection, so the caller's own exact check still
// runs on this (small) candidate set, and matching semantics do not
// change, only how candidates are found. Words under three characters
// cannot be trigrammed at all; Candidates falls back to All() for them,
// correct but with no narrowing benefit — short words are rare for this
// tool's purpose and already produce large, low-precision result sets
// by the nature of the word itself.
func (s *Store) Candidates(word string) iter.Seq[entry.Entry] {
	lw := strings.ToLower(word)
	grams := trigrams(lw)
	if len(grams) == 0 {
		return s.All()
	}
	return func(yield func(entry.Entry) bool) {
		_ = s.db.View(func(tx *bolt.Tx) error {
			tb := tx.Bucket(bucketNameTrigrams)
			eb := tx.Bucket(bucketEntries)
			if tb == nil || eb == nil {
				return nil
			}
			ids, ok := intersectPostings(tb, grams)
			if !ok {
				return nil
			}
			for _, id := range ids {
				v := eb.Get(idKey(id))
				if v == nil {
					continue
				}
				e, err := decodeEntry(v)
				if err != nil {
					continue
				}
				if !yield(e) {
					return nil
				}
			}
			return nil
		})
	}
}

// intersectPostings reads every posting list in grams and returns the
// sorted intersection of ids, or ok=false the instant any one gram has
// no postings at all (an empty intersection — no need to read the rest).
func intersectPostings(b *bolt.Bucket, grams []string) (ids []uint64, ok bool) {
	var sets [][]uint64
	for _, g := range grams {
		v := b.Get([]byte(g))
		if v == nil {
			return nil, false
		}
		sets = append(sets, decodeIDList(v))
	}
	sort.Slice(sets, func(i, j int) bool { return len(sets[i]) < len(sets[j]) })
	result := sets[0]
	for _, s := range sets[1:] {
		result = intersectSorted(result, s)
		if len(result) == 0 {
			return nil, false
		}
	}
	return result, true
}

func intersectSorted(a, b []uint64) []uint64 {
	var out []uint64
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			out = append(out, a[i])
			i++
			j++
		case a[i] < b[j]:
			i++
		default:
			j++
		}
	}
	return out
}

// trigrams splits lowered into overlapping 3-rune windows. Fewer than 3
// runes yields none — the caller falls back to a full scan.
func trigrams(lowered string) []string {
	runes := []rune(lowered)
	if len(runes) < 3 {
		return nil
	}
	seen := make(map[string]bool, len(runes)-2)
	var out []string
	for i := 0; i+3 <= len(runes); i++ {
		g := string(runes[i : i+3])
		if !seen[g] {
			seen[g] = true
			out = append(out, g)
		}
	}
	return out
}

// idKey renders id as an 8-byte big-endian key, not a decimal string.
// bbolt's B-tree is fastest when keys arrive in ascending byte order —
// its own documented append-optimized path — and build assigns ids
// sequentially while inserting; big-endian is the encoding where
// numeric and byte order agree ("1","10","11",...,"2" is not sequential
// in decimal-string byte order, so that encoding turned every insert
// into a mid-tree one). Measured against kubernetes (172,397 entries):
// this dropped a cold `find` from 37s to the same order of magnitude as
// the old text cache, entirely from this one change.
func idKey(id uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], id)
	return b[:]
}

// encodeIDList and decodeIDList render a posting list as decimal ids
// joined by ",": simple, and postings lists are short enough (bounded by
// how many entries share one trigram) that a denser binary encoding is
// not worth the added complexity here.
func encodeIDList(ids []uint64) []byte {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatUint(id, 10)
	}
	return []byte(strings.Join(parts, ","))
}

func decodeIDList(v []byte) []uint64 {
	if len(v) == 0 {
		return nil
	}
	parts := strings.Split(string(v), ",")
	ids := make([]uint64, 0, len(parts))
	for _, p := range parts {
		id, err := strconv.ParseUint(p, 10, 64)
		if err != nil {
			continue
		}
		ids = append(ids, id)
	}
	return ids
}

// enumBlob returns the raw enum-cache blob, or nil if none has been
// written yet.
func (s *Store) enumBlob() ([]byte, error) {
	var v []byte
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketEnum)
		if b == nil {
			return nil
		}
		if raw := b.Get(keyEnumBlob); raw != nil {
			v = append([]byte(nil), raw...)
		}
		return nil
	})
	return v, err
}

// fileRecord looks up path's extraction-cache entry: its freshness
// fingerprint and the ids of the entries extracted from it last time.
// ok is false when path has no record at all (never seen, or the store
// itself doesn't exist).
func (s *Store) fileRecord(path string) (rec Record, ok bool) {
	if s == nil {
		return Record{}, false
	}
	_ = s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketFiles)
		if b == nil {
			return nil
		}
		v := b.Get([]byte(path))
		if v == nil {
			return nil
		}
		r, decErr := decodeFileRecord(v)
		if decErr != nil {
			return nil
		}
		rec, ok = r, true
		return nil
	})
	return rec, ok
}

// fileCount reports how many extraction-cache records the store holds —
// used only to detect "a file present before is gone now" (deletion or
// a newly added exclusion pattern), which must force a rebuild since a
// Refresh call only ever adds to the files map it builds, never deletes
// from an existing store.
func (s *Store) fileCount() int {
	if s == nil {
		return 0
	}
	n := 0
	_ = s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketFiles)
		if b == nil {
			return nil
		}
		n = b.Stats().KeyN
		return nil
	})
	return n
}

// entriesByID reads exactly the entries named by ids — a targeted
// lookup, not a scan — used to carry a file's entries forward into a
// rebuilt store without re-extracting it.
func (s *Store) entriesByID(ids []uint64) []entry.Entry {
	if s == nil || len(ids) == 0 {
		return nil
	}
	out := make([]entry.Entry, 0, len(ids))
	_ = s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketEntries)
		if b == nil {
			return nil
		}
		for _, id := range ids {
			v := b.Get(idKey(id))
			if v == nil {
				continue
			}
			if e, err := decodeEntry(v); err == nil {
				out = append(out, e)
			}
		}
		return nil
	})
	return out
}

func encodeFileRecord(r Record) []byte {
	ids := make([]string, len(r.EntryIDs))
	for i, id := range r.EntryIDs {
		ids[i] = strconv.FormatUint(id, 10)
	}
	return []byte(fmt.Sprintf("%d\t%d\t%s", r.ModTime, r.Size, strings.Join(ids, ",")))
}

func decodeFileRecord(v []byte) (Record, error) {
	fields := strings.SplitN(string(v), "\t", 3)
	if len(fields) != 3 {
		return Record{}, fmt.Errorf("expected 3 fields, got %d", len(fields))
	}
	modTime, err1 := strconv.ParseInt(fields[0], 10, 64)
	size, err2 := strconv.ParseInt(fields[1], 10, 64)
	if err1 != nil || err2 != nil {
		return Record{}, fmt.Errorf("malformed freshness fields: %q", v)
	}
	return Record{ModTime: modTime, Size: size, EntryIDs: decodeIDList([]byte(fields[2]))}, nil
}

// build writes a complete store in one transaction: every entry (fresh
// id assigned to each), the files freshness records, the enum blob, and
// the name-trigram posting list built alongside entry assignment. This
// is always a full rewrite, never an incremental update (§23 C-14's
// "discard and rebuild" ethos, extended here: a full rebuild is exactly
// what the old text cache did on every Save too, just against a
// database file instead of a line-oriented one).
func (s *Store) build(files map[string]Record, allEntries []entry.Entry, enumBlob []byte) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		eb := tx.Bucket(bucketEntries)
		fb := tx.Bucket(bucketFiles)
		tb := tx.Bucket(bucketNameTrigrams)

		trigramPostings := make(map[string][]uint64)
		nextID := uint64(1)
		fileIDs := make(map[string][]uint64, len(files))
		for path := range files {
			fileIDs[path] = nil
		}
		for _, e := range allEntries {
			id := nextID
			nextID++
			if err := eb.Put(idKey(id), encodeEntry(e)); err != nil {
				return err
			}
			// A file's own fileEntry (Kind==KindFile) shares its Path
			// with every declaration extracted from it, but is never
			// part of the extraction cache's carried-forward set — it
			// is recomputed fresh from the scan every single call
			// (Refresh's own top loop), never read back from a store.
			// Including it here too would have an unchanged file's
			// carried-over EntryIDs return its own file entry a second
			// time on every subsequent rebuild, once alongside the
			// freshly recomputed one.
			if e.Kind != entry.KindFile {
				fileIDs[e.Path] = append(fileIDs[e.Path], id)
			}
			for _, g := range trigrams(strings.ToLower(e.Name)) {
				trigramPostings[g] = append(trigramPostings[g], id)
			}
		}
		// §3 C5: sorted rather than ranged over directly — a Go map's
		// iteration order is randomised, and Put's insertion order can
		// affect a B-tree's resulting page layout. Sorting keeps this
		// deterministic in the same sense §3 always meant (the same
		// tree produces the same output), even though it is a bbolt
		// file now, not text.
		paths := make([]string, 0, len(files))
		for path := range files {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		for _, path := range paths {
			rec := files[path]
			rec.EntryIDs = fileIDs[path]
			if err := fb.Put([]byte(path), encodeFileRecord(rec)); err != nil {
				return err
			}
		}
		grams := make([]string, 0, len(trigramPostings))
		for g := range trigramPostings {
			grams = append(grams, g)
		}
		sort.Strings(grams)
		for _, g := range grams {
			if err := tb.Put([]byte(g), encodeIDList(trigramPostings[g])); err != nil {
				return err
			}
		}
		if enumBlob != nil {
			eb2, err := tx.CreateBucketIfNotExists(bucketEnum)
			if err != nil {
				return err
			}
			if err := eb2.Put(keyEnumBlob, enumBlob); err != nil {
				return err
			}
		}
		return nil
	})
}

// encodeEntry and decodeEntry are the store's per-entry shape: the ten
// fields specifications/giraffe.md §18 defines, tab-separated — reusing
// the encoding directive 001-011's cache already proved, minus the two
// freshness fields (ModTime, Size — the file's own freshness fingerprint
// used by Refresh, which now live in the files bucket instead of
// alongside every entry, §30 C-47's division of labour: entries and
// freshness are different concerns) — plus entry.Size and DeclCount
// (directive 009 item 3), which the old cache never persisted because
// every caller got them from the same in-memory all slice Refresh had
// just patched them into. Here, every caller — including the fast,
// nothing-changed path, which never touches that computation again —
// reads entries back through this encoding, so these two must round-trip
// through it too or the fast path would silently serve them as zero.
func encodeEntry(e entry.Entry) []byte {
	fields := []string{
		sanitize(e.Path),
		sanitize(e.Kind),
		sanitize(e.Name),
		sanitize(e.Project),
		sanitize(e.Scope),
		encodeBool(e.Exported),
		encodePosition(e.Position),
		sanitize(e.Doc),
		sanitize(e.Lang),
		sanitize(e.Ext),
		strconv.FormatInt(e.Size, 10),
		strconv.Itoa(e.DeclCount),
	}
	return []byte(strings.Join(fields, "\t"))
}

func decodeEntry(v []byte) (entry.Entry, error) {
	fields := strings.Split(string(v), "\t")
	if len(fields) != 12 {
		return entry.Entry{}, fmt.Errorf("expected 12 tab-separated fields, got %d", len(fields))
	}
	size, err1 := strconv.ParseInt(fields[10], 10, 64)
	declCount, err2 := strconv.Atoi(fields[11])
	if err1 != nil || err2 != nil {
		return entry.Entry{}, fmt.Errorf("malformed Size/DeclCount fields: %q", v)
	}
	return entry.Entry{
		Path:      fields[0],
		Kind:      fields[1],
		Name:      fields[2],
		Project:   fields[3],
		Scope:     fields[4],
		Exported:  fields[5] == "1",
		Position:  decodePosition(fields[6]),
		Doc:       fields[7],
		Lang:      fields[8],
		Ext:       fields[9],
		Size:      size,
		DeclCount: declCount,
	}, nil
}
