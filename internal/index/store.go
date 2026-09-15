// store.go holds the bbolt-backed persistent form (specifications/giraffe.md
// §30 C-46, C-47): entries read through mmap rather than decoded into a
// process-resident structure on every invocation, a name-trigram posting
// list so Search's name-based tiers narrow without visiting every entry,
// and the extraction cache (files) and enumeration cache (enum) kept as
// buckets in the same file rather than separate text files. Schema version
// lives in one bucket; any mismatch — including a file that does not open
// as a valid bbolt database at all — means discard and rebuild, never
// migrate (§23 C-14, unchanged by this directive).
//
// commit, below, is the one place anything is written: directive 013
// made it differential — a write transaction touches only the paths a
// caller names as changed or deleted, never the whole store — with the
// very first build of a store expressed as the same call, not a
// separate path (see commit's own comment).
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
const schemaVersion = "2"

var (
	bucketMeta         = []byte("meta")
	bucketEntries      = []byte("entries")
	bucketFiles        = []byte("files")
	bucketEnum         = []byte("enum")
	bucketNameTrigrams = []byte("name_trigrams")
	keySchemaVersion   = []byte("schema_version")
	keyEnumBlob        = []byte("blob")
	keyWalkBlob        = []byte("walk")
	keyNextID          = []byte("next_id")
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

// openForWrite reopens root's existing store read-write, for a
// differential update (directive 013): unlike createStore, it neither
// removes nor recreates the file — the whole point is to modify what is
// already there in place, so the entries and postings a change does not
// touch are never even read. Only called once Refresh already knows,
// from a prior read-only open, that the file exists and opens cleanly;
// a failure here would mean it changed shape between the two opens,
// which callers treat as any other I/O error.
func openForWrite(root string) (*Store, error) {
	path := filepath.Join(root, Dir, cacheDir, storeFilename)
	db, err := bolt.Open(path, 0o644, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, fmt.Errorf("reopen store for writing: %w", err)
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

// walkBlob returns the raw incremental-walk cache blob (directive 021
// item 2), or nil if none has been written yet — the same "no cache
// yet" contract enumBlob has, in the same bucket, under its own key.
func (s *Store) walkBlob() ([]byte, error) {
	var v []byte
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketEnum)
		if b == nil {
			return nil
		}
		if raw := b.Get(keyWalkBlob); raw != nil {
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

// filePaths returns every path the files bucket currently knows about —
// a key-only cursor walk, no value decoded — so Refresh can compute
// exactly which paths from a previous run are gone now (directive 013:
// "entries belonging to deleted files: removed" needs to name them, not
// just notice the count changed).
func (s *Store) filePaths() []string {
	if s == nil {
		return nil
	}
	var paths []string
	_ = s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketFiles)
		if b == nil {
			return nil
		}
		c := b.Cursor()
		for k, _ := c.First(); k != nil; k, _ = c.Next() {
			paths = append(paths, string(k))
		}
		return nil
	})
	return paths
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

// fileChange is one file's fresh contribution to the store, as Refresh
// prepares it: its freshness fingerprint and every entry it now owns —
// its own file entry, plus declarations if it is extractable. commit
// assigns fresh ids to Entries after first removing whatever that path
// owned before, so a file's identity as "the set of ids it owns" never
// straddles a stale id and a new one.
type fileChange struct {
	Path    string
	ModTime int64
	Size    int64
	Entries []entry.Entry
}

// cacheBlobs bundles commit's two cache-blob parameters — see commit's
// own doc comment for why they are one value, not two parameter pairs.
type cacheBlobs struct {
	enum      []byte
	writeEnum bool
	walk      []byte
}

// commitMode selects how commit fills bbolt's pages while writing —
// see commit's own doc comment for why this is a named fork, not a
// bool.
type commitMode int

const (
	// commitDifferential is an update against a store that already has
	// content — bbolt's own default page-fill behaviour, chosen for
	// future scattered writes, not this one.
	commitDifferential commitMode = iota
	// commitBulk is a first build, or a full rebuild of a discarded
	// store — every key this call writes is new and arrives in
	// ascending order, so packing pages fuller costs nothing later.
	commitBulk
)

// commit applies a differential set of changes in one write transaction
// (directive 013): each change in changes replaces whatever entries its
// path owned before with fresh ones — the old ones, and their trigram
// postings, are removed first, so the id space never carries a stale
// entry forward under a live id — and each path in deletes is removed
// outright. enumBlob is written only when writeEnum says it actually
// differs from what is stored. A path this call says nothing about is
// never opened: the write transaction's size is the size of the diff,
// not the corpus.
//
// The very first build of a store is not a separate code path: it is
// this same function with changes covering every scanned file, deletes
// empty, and nothing yet in the files bucket for remove to find (§23
// C-14's discard-and-rebuild ethos — one mechanism, not two that must be
// kept in step by hand).
//
// keysWritten counts distinct bbolt keys touched (Put or Delete) across
// entries, files, name_trigrams and meta — the number directive 013's
// own completion report is required to give for "nothing changed / 5
// files changed / full build": a Put and a Delete on the same key
// within one call (an entry's trigram gram losing an old id and
// gaining a new one) count once, since that gram is the thing that
// changed, not twice for how many operations it took.
//
// mode (directive 020 §1) is commitBulk exactly when this transaction is
// writing into a store with nothing in it yet — a first run, or a
// stale/unreadable store being rebuilt from scratch (see Refresh's own
// !roOK) — never for a differential update against a store that already
// has content. Measured before changing anything (this directive's
// completion report has the breakdown): on a cold kubernetes build,
// commit was ~70% of total wall time, and within commit itself,
// entries+files (encoding and inserting) and the bbolt transaction's
// own internal commit (page spill, rebalance, write, fsync) were each
// roughly a third — with 013's own trigram-posting batching (below)
// confirmed still doing its job (applying every gram once, not
// read-modify-written per entry that shares it). commitBulk addresses
// the bbolt-internal third: entries, files and name_trigrams are all
// written in ascending key order on a full build (nextID is sequential;
// sortedChanges and sortedGrams are both sorted), which is exactly the
// append-only pattern bbolt's own Bucket.FillPercent doc names as worth
// raising — and the same value its own compact tool uses for a bulk
// copy (compact.go). Left at bbolt's 0.5 default for a differential
// update: those writes land among a live tree's already-filled pages,
// not appended past its end, where packing every touched page to 100%
// now would only move today's saving into tomorrow's extra split.
//
// An enumerated mode, not a second bool parameter (code-manners M5:
// "no boolean parameter that selects behavior... one enumerated mode")
// — bulk-vs-differential is a named behavior fork this call makes, not
// a data flag like caches.writeEnum, which says only whether a value is
// present to write, not how the write itself behaves.
//
// caches bundles the enum cache blob (directive 019) and the
// incremental walk cache blob (directive 021 item 2) — two parallel
// (blob []byte, write bool)-shaped facts that already had to change
// together (both come from the same enumerate call, in refresh.go) and
// so are one value, not two more parameters on top of the four already
// here (code-manners M2; patterns.md §9). The walk blob has no write
// flag of its own: Walk always runs and always produces a fresh cache
// to persist, unlike the enum blob, whose cache-hit replay path can
// leave nothing new to write.
func (s *Store) commit(changes []fileChange, deletes []string, caches cacheBlobs, mode commitMode) (keysWritten int, err error) {
	err = s.db.Update(func(tx *bolt.Tx) error {
		eb := tx.Bucket(bucketEntries)
		fb := tx.Bucket(bucketFiles)
		tb := tx.Bucket(bucketNameTrigrams)
		mb := tx.Bucket(bucketMeta)
		if mode == commitBulk {
			eb.FillPercent = 1.0
			fb.FillPercent = 1.0
			tb.FillPercent = 1.0
		}

		touched := make(map[string]struct{})
		touch := func(bucket byte, key []byte) {
			touched[string(bucket)+string(key)] = struct{}{}
		}

		nextID := readNextID(mb)

		// Trigram postings are batched in memory across every change and
		// delete in this call, not read-modify-written per entry: a
		// full build's common case is a gram shared by thousands of
		// entries (kubernetes: 172,397 of them), and touching bbolt once
		// per entry that happens to share it — decoding and re-encoding
		// a growing list each time — makes that gram's own cost grow
		// quadratically in how many entries share it. Collecting every
		// add and remove here first, then applying each distinct gram
		// exactly once below, keeps the cost linear in how many
		// (entry, gram) pairs exist, the same bound the old full-rewrite
		// build had via its own in-memory map.
		addIDs := make(map[string][]uint64)
		removeIDs := make(map[string][]uint64)

		removeOwned := func(path string) error {
			v := fb.Get([]byte(path))
			if v == nil {
				return nil
			}
			rec, err := decodeFileRecord(v)
			if err != nil {
				return nil
			}
			for _, id := range rec.EntryIDs {
				ev := eb.Get(idKey(id))
				if ev == nil {
					continue
				}
				if e, decErr := decodeEntry(ev); decErr == nil {
					for _, g := range trigrams(strings.ToLower(e.Name)) {
						removeIDs[g] = append(removeIDs[g], id)
					}
				}
				if err := eb.Delete(idKey(id)); err != nil {
					return err
				}
				touch('e', idKey(id))
			}
			return nil
		}

		sortedDeletes := append([]string(nil), deletes...)
		sort.Strings(sortedDeletes)
		for _, path := range sortedDeletes {
			if err := removeOwned(path); err != nil {
				return err
			}
			if err := fb.Delete([]byte(path)); err != nil {
				return err
			}
			touch('f', []byte(path))
		}

		sortedChanges := append([]fileChange(nil), changes...)
		sort.Slice(sortedChanges, func(i, j int) bool { return sortedChanges[i].Path < sortedChanges[j].Path })
		for _, c := range sortedChanges {
			if err := removeOwned(c.Path); err != nil {
				return err
			}
			ids := make([]uint64, 0, len(c.Entries))
			for _, e := range c.Entries {
				id := nextID
				nextID++
				if err := eb.Put(idKey(id), encodeEntry(e)); err != nil {
					return err
				}
				touch('e', idKey(id))
				ids = append(ids, id)
				for _, g := range trigrams(strings.ToLower(e.Name)) {
					addIDs[g] = append(addIDs[g], id)
				}
			}
			rec := Record{ModTime: c.ModTime, Size: c.Size, EntryIDs: ids}
			if err := fb.Put([]byte(c.Path), encodeFileRecord(rec)); err != nil {
				return err
			}
			touch('f', []byte(c.Path))
		}

		grams := make(map[string]struct{}, len(addIDs)+len(removeIDs))
		for g := range addIDs {
			grams[g] = struct{}{}
		}
		for g := range removeIDs {
			grams[g] = struct{}{}
		}
		sortedGrams := make([]string, 0, len(grams))
		for g := range grams {
			sortedGrams = append(sortedGrams, g)
		}
		sort.Strings(sortedGrams)
		for _, g := range sortedGrams {
			if err := applyPosting(tb, g, addIDs[g], removeIDs[g]); err != nil {
				return err
			}
			touch('t', []byte(g))
		}

		if caches.writeEnum && caches.enum != nil {
			eb2, err := tx.CreateBucketIfNotExists(bucketEnum)
			if err != nil {
				return err
			}
			if err := eb2.Put(keyEnumBlob, caches.enum); err != nil {
				return err
			}
			touch('u', keyEnumBlob)
		}
		if caches.walk != nil {
			wb, err := tx.CreateBucketIfNotExists(bucketEnum)
			if err != nil {
				return err
			}
			if err := wb.Put(keyWalkBlob, caches.walk); err != nil {
				return err
			}
			touch('u', keyWalkBlob)
		}

		if len(sortedChanges) > 0 {
			if err := writeNextID(mb, nextID); err != nil {
				return err
			}
			touch('m', keyNextID)
		}

		keysWritten = len(touched)
		return nil
	})
	return keysWritten, err
}

// applyPosting reads gram's current posting list once, removes every id
// in remove and appends every id in add, and writes the result back
// once — deleting the key entirely if the result is empty, so an absent
// key and an empty list are never two representations of the same fact
// (intersectPostings' "no postings at all" short-circuit depends on
// that). ids are assigned sequentially and never reused, so every id in
// add is larger than anything already stored or being removed —
// appending after filtering keeps the list sorted without a re-sort.
//
// One call handles everything gram loses and gains in this commit, no
// matter how many entries that is — commit's own caller batches every
// add and remove across the whole transaction into add/remove first,
// specifically so a gram shared by thousands of entries costs one read
// and one write here, not thousands.
func applyPosting(tb *bolt.Bucket, gram string, add, remove []uint64) error {
	ids := decodeIDList(tb.Get([]byte(gram)))
	if len(remove) > 0 {
		skip := make(map[uint64]bool, len(remove))
		for _, id := range remove {
			skip[id] = true
		}
		kept := ids[:0]
		for _, id := range ids {
			if !skip[id] {
				kept = append(kept, id)
			}
		}
		ids = kept
	}
	ids = append(ids, add...)
	if len(ids) == 0 {
		return tb.Delete([]byte(gram))
	}
	return tb.Put([]byte(gram), encodeIDList(ids))
}

// readNextID and writeNextID persist the entry-id counter across calls
// (meta bucket, key "next_id") — ids must never restart at 1 on a
// differential update, or a fresh id could collide with one still live
// from an earlier call. A missing key (a store just created by
// createStore) means "nothing assigned yet": start at 1.
func readNextID(b *bolt.Bucket) uint64 {
	v := b.Get(keyNextID)
	if v == nil {
		return 1
	}
	id, err := strconv.ParseUint(string(v), 10, 64)
	if err != nil {
		return 1
	}
	return id
}

func writeNextID(b *bolt.Bucket, id uint64) error {
	return b.Put(keyNextID, []byte(strconv.FormatUint(id, 10)))
}

// encodeEntry and decodeEntry are the store's per-entry shape: the ten
// fields specifications/giraffe.md §18 defines, tab-separated — reusing
// the encoding directive 001-011's cache already proved, minus the two
// freshness fields (ModTime, Size — the file's own freshness fingerprint
// used by Refresh, which now live in the files bucket instead of
// alongside every entry, §30 C-47's division of labour: entries and
// freshness are different concerns) — plus entry.Size and DeclCount
// (directive 009 item 3) and Line/LineText (§31.4), none of which the
// old cache persisted, because every caller used to get them from the
// same in-memory all slice Refresh had just patched them into. Here,
// every caller — including the fast, nothing-changed path, which never
// touches that computation again — reads entries back through this
// encoding, so these must round-trip through it too or the fast path
// would silently serve them as zero.
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
		strconv.Itoa(e.Line),
		sanitize(e.LineText),
	}
	return []byte(strings.Join(fields, "\t"))
}

func decodeEntry(v []byte) (entry.Entry, error) {
	fields := strings.Split(string(v), "\t")
	if len(fields) != 14 {
		return entry.Entry{}, fmt.Errorf("expected 14 tab-separated fields, got %d", len(fields))
	}
	size, err1 := strconv.ParseInt(fields[10], 10, 64)
	declCount, err2 := strconv.Atoi(fields[11])
	line, err3 := strconv.Atoi(fields[12])
	if err1 != nil || err2 != nil || err3 != nil {
		return entry.Entry{}, fmt.Errorf("malformed Size/DeclCount/Line fields: %q", v)
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
		Line:      line,
		LineText:  fields[13],
	}, nil
}
