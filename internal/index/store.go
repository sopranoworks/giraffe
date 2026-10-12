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
	"bytes"
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

	"github.com/sopranoworks/giraffe/internal/entry"
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
// A change in what an encoded field means is a change of shape too: an
// index that decoded fine but held ranges computed the old way would
// keep them for every file that has not changed since.
const schemaVersion = "13" // 3: EndLine joined the entry encoding (directive 042); 4: Line starts at the doc comment (043); 5: DocText joined it (065); 6: Test joined it (065, g42); 7: the calls and callers buckets (050, ported in 065 g47); 8: Call.Text and Call.In (065, g48); 9: callers keyed by name and path (067); 10: Call.Use (072); 12: uses kept only under constants and variables (073), its own store beside 10 and 11; 13: a nested project is named by its path (077)

var (
	bucketMeta         = []byte("meta")
	bucketEntries      = []byte("entries")
	bucketFiles        = []byte("files")
	bucketEnum         = []byte("enum")
	bucketNameTrigrams = []byte("name_trigrams")
	// bucketCalls and bucketCallers are the call-site index's two
	// directions (directive 050 §2): calls maps a file's path to every
	// Call in it — the record commit reads back to know which names a
	// changed file used to call — and callers holds, under the key
	// name+NUL+path (callersKey), the calls of that name in that file,
	// so every site of a name is one prefix scan in path then line
	// order, which Callers serves (directive 067: one key per name and
	// file, written once with the file, so a build never holds every
	// site of every name in memory to write a name's list once). A
	// call in file A is a fact about a name declared in B, so neither is
	// stored in an entry: a change to A rewrites A's calls record and
	// A's own keys under the names A called before and calls now, and
	// nothing of B's (see commitBatch).
	bucketCalls      = []byte("calls")
	bucketCallers    = []byte("callers")
	keySchemaVersion = []byte("schema_version")
	keyEnumBlob      = []byte("blob")
	keyWalkBlob      = []byte("walk")
	keyNextID        = []byte("next_id")
	// keyComplete is present exactly when every write the store has
	// seen ran to its end (directive 067): a batch's first transaction
	// removes it and its last puts it back, so a store whose build or
	// update was cut short — a timeout, a kill — opens as no store at
	// all (openStore) and is rebuilt whole, never read with its files
	// bucket ahead of its postings.
	keyComplete = []byte("complete")
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
// not exist, does not open as a valid bbolt file, was written by a
// different schema version (§23 C-14: never migrated, always rebuilt),
// or was left without its complete mark by a write that never finished
// (keyComplete).
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
		valid = string(b.Get(keySchemaVersion)) == schemaVersion && b.Get(keyComplete) != nil
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
		for _, name := range [][]byte{bucketMeta, bucketEntries, bucketFiles, bucketEnum, bucketNameTrigrams, bucketCalls, bucketCallers} {
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

// Callers returns every call site of name in this store, in path then
// line order — what a result line's callers: carries (directive 050).
// Nil when nothing calls the name. One prefix scan over the name's own
// keys (callersKey: one per file that calls it, in path order, each
// holding that file's sites in line order), decoded on demand: the
// list is attached to an entry only when a query reaches it
// (query.Search, query.Browse), never on the whole-index walks All and
// Candidates serve, so Summarize's pass over every entry pays nothing
// for it.
func (s *Store) Callers(name string) []entry.Call {
	if s == nil {
		return nil
	}
	var calls []entry.Call
	_ = s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketCallers)
		if b == nil {
			return nil
		}
		prefix := callersPrefix(name)
		c := b.Cursor()
		for k, v := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, v = c.Next() {
			calls = append(calls, decodeCalls(v)...)
		}
		return nil
	})
	return calls
}

// callersKey is the callers bucket's key for name's sites in path: the
// name, a NUL (which neither an identifier nor a path can hold), then
// the path — so one name's keys are contiguous and sort by path, the
// order Callers promises.
func callersKey(name, path string) []byte {
	return []byte(name + "\x00" + path)
}

func callersPrefix(name string) []byte {
	return []byte(name + "\x00")
}

// CallsIn is the call sites recorded in the file at path, in line
// order (query.Entries; directive 073): one key read, decoded on demand
// for the entries a page prints, the way Callers is.
func (s *Store) CallsIn(path string) []entry.Call {
	return s.callsOf(path)
}

// callsOf returns the call sites recorded for path — a file's own
// calls, carried forward with its entries when its content is unchanged
// but its record must be rewritten (carryForward), the way entriesByID
// carries the entries.
func (s *Store) callsOf(path string) []entry.Call {
	if s == nil {
		return nil
	}
	var calls []entry.Call
	_ = s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketCalls)
		if b == nil {
			return nil
		}
		calls = decodeCalls(b.Get([]byte(path)))
		return nil
	})
	return calls
}

// encodeCalls and decodeCalls render a list of call sites one per
// line, tab-separated path, name, line, via and text — the same one-
// fact-one-line, tab-between-fields shape encodeEntry uses, so a field
// is sanitized the same way (sanitize) and nothing in it can split the
// line — text included: a call's own source line can itself hold a
// literal tab (Go source commonly does), which sanitize folds to a
// space the same as it would in Doc or LineText. Both buckets hold the
// same shape: a file's calls carry their own path on every line
// (redundant there, one encoding), a name's callers carry the name on
// every line likewise. Empty in, nil out; a malformed line is skipped,
// since this file's own pair wrote it.
func encodeCalls(calls []entry.Call) []byte {
	var b strings.Builder
	for i, c := range calls {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(sanitize(c.Path))
		b.WriteByte('\t')
		b.WriteString(sanitize(c.Name))
		b.WriteByte('\t')
		b.WriteString(strconv.Itoa(c.Line))
		b.WriteByte('\t')
		b.WriteString(sanitize(c.Via))
		b.WriteByte('\t')
		b.WriteString(encodeBool(c.Test))
		b.WriteByte('\t')
		b.WriteString(sanitize(c.Text))
		b.WriteByte('\t')
		b.WriteString(sanitize(c.In))
		b.WriteByte('\t')
		b.WriteString(encodeBool(c.Use))
	}
	return []byte(b.String())
}

func decodeCalls(v []byte) []entry.Call {
	if len(v) == 0 {
		return nil
	}
	lines := strings.Split(string(v), "\n")
	calls := make([]entry.Call, 0, len(lines))
	for _, l := range lines {
		f := strings.Split(l, "\t")
		if len(f) != 8 {
			continue
		}
		line, err := strconv.Atoi(f[2])
		if err != nil {
			continue
		}
		calls = append(calls, entry.Call{Path: f[0], Name: f[1], Line: line, Via: f[3], Test: f[4] == "1", Text: f[5], In: f[6], Use: f[7] == "1"})
	}
	return calls
}

// sortCalls orders call sites by path, then line, then name — the
// stored order of a name's callers, and the order a file's calls are
// written in, so the same tree writes the same bytes (§3 C5).
func sortCalls(calls []entry.Call) {
	sort.Slice(calls, func(i, j int) bool {
		a, b := calls[i], calls[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Name < b.Name
	})
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
	// Calls is every call site in the file (directive 050), written to
	// the calls bucket under Path and merged into the callers bucket
	// under each called name — see commit.
	Calls []entry.Call
}

// cacheBlobs bundles a batch's two cache-blob parameters — see
// commitBatch.finish's own doc comment for why they are one value, not
// two parameter pairs.
type cacheBlobs struct {
	enum      []byte
	writeEnum bool
	walk      []byte
}

// commitMode selects how a batch fills bbolt's pages while writing —
// see commitBatch's own doc comment for why this is a named fork, not
// a bool.
type commitMode int

const (
	// commitDifferential is an update against a store that already has
	// content — bbolt's own default page-fill behaviour, chosen for
	// future scattered writes, not this one.
	commitDifferential commitMode = iota
	// commitBulk is a first build, or a full rebuild of a discarded
	// store — every key this batch writes is new and arrives in
	// ascending order, so packing pages fuller costs nothing later.
	commitBulk
)

// chunkBytes bounds how many bytes of values one of a batch's
// transactions holds before it is committed and the next begun
// (directive 067). bbolt keeps every page a transaction touches as a
// Go object until that transaction commits, and re-copies all of them
// each time the file grows under it, so one transaction the size of
// kubernetes' index (400 MB of values) held some 750 MB of heap under
// a 384 MiB ceiling: 3,100 collections, 72% of the build's CPU in the
// collector, and 29 of its 32 seconds in commit. Eight mebibytes keeps
// a transaction's own footprint a small multiple of this, well inside
// the ceiling, at some fifty commits for kubernetes — each a page
// write and an fsync, negligible next to what it saves.
const chunkBytes = 8 << 20

// commitBatch applies one Refresh's changes to the store, in bounded
// transactions (directive 067, restructuring directive 013's single
// differential transaction without changing what it writes). A caller
// removes every deleted path (remove), adds every changed file's fresh
// contribution in path order (add) — streaming them straight from
// extraction on a first build, so the whole corpus is never resident
// at once — and finishes (finish), which writes what only the whole
// batch can know: the trigram postings, the cache blobs, the id
// counter and the complete mark.
//
// What a batch writes is exactly what the one transaction wrote before
// it: each change replaces whatever entries its path owned with fresh
// ones — the old ones, and their trigram postings, are removed first,
// so the id space never carries a stale entry forward under a live id
// — and each deleted path is removed outright. A path the batch says
// nothing about is never opened: the writes are the size of the diff,
// not the corpus. Ids are assigned in the order add is called, so the
// caller's path order is the store's id order — the same order the one
// transaction assigned, sorting its changes by path first — and a
// first build's entries, files and calls all arrive in ascending key
// order, bbolt's own append-optimized path.
//
// The very first build of a store is not a separate code path: it is
// this same batch with add called for every scanned file, nothing to
// remove, and nothing yet in the files bucket for removeOwned to find
// (§23 C-14's discard-and-rebuild ethos — one mechanism, not two that
// must be kept in step by hand).
//
// The call-site index (directive 050 §2) follows the same rule with a
// second dimension: a changed or deleted path's old calls record names
// every callers key that held a site in that path (callersKey — one per
// name the file called), and its new calls write one key per name it
// calls now. A name nobody in the change set called before or after is
// never opened; a path outside the change set has its own calls record
// and its own callers keys left exactly as they were.
//
// Trigram postings are batched in memory across every add and remove
// in the batch, not read-modify-written per entry: a full build's
// common case is a gram shared by thousands of entries, and touching
// bbolt once per entry that happens to share it — decoding and
// re-encoding a growing list each time — makes that gram's own cost
// grow quadratically in how many entries share it. Collecting every
// add and remove first, then applying each distinct gram exactly once
// in finish, keeps the cost linear in how many (entry, gram) pairs
// exist — and the map is ids alone, a few tens of megabytes for
// kubernetes, not the entries.
//
// keysWritten counts distinct bbolt keys touched (Put or Delete) across
// entries, files, name_trigrams, calls, callers and meta — the number
// directive 013's own completion report is required to give for
// "nothing changed / 5 files changed / full build": a Put and a Delete
// on the same key within one transaction (a file's callers key dropped
// for its old sites and written for its new) count once, since that
// key is the thing that changed, not twice for how many operations it
// took. A key is touched in one transaction only: a path's keys are all
// written by its own add, and a gram's posting once, in finish.
//
// mode (directive 020 §1) is commitBulk exactly when the batch is
// writing into a store with nothing in it yet — a first run, or a
// stale/unreadable store being rebuilt from scratch (see Refresh) —
// never for a differential update against a store that already has
// content. Measured before changing anything (directive 020's report):
// on a cold kubernetes build the bbolt transaction's own internal
// commit (page spill, rebalance, write, fsync) was a third of commit;
// commitBulk addresses it: entries, files and name_trigrams are all
// written in ascending key order on a full build, which is exactly the
// append-only pattern bbolt's own Bucket.FillPercent doc names as worth
// raising — and the same value its own compact tool uses for a bulk
// copy (compact.go). Left at bbolt's 0.5 default for a differential
// update: those writes land among a live tree's already-filled pages,
// not appended past its end, where packing every touched page to 100%
// now would only move today's saving into tomorrow's extra split.
//
// An enumerated mode, not a bool (code-manners M5: "no boolean
// parameter that selects behavior... one enumerated mode") —
// bulk-vs-differential is a named behavior fork, not a data flag like
// caches.writeEnum, which says only whether a value is present to
// write, not how the write itself behaves.
type commitBatch struct {
	s    *Store
	mode commitMode

	tx      *bolt.Tx
	txBytes int
	touched map[string]struct{} // keys touched by the open transaction
	keys    int                 // keys touched by every transaction committed so far
	started bool                // the first transaction has begun: the complete mark is gone

	nextID   uint64
	assigned bool // nextID moved and must be persisted

	addIDs    map[string][]uint64
	removeIDs map[string][]uint64
}

// beginCommit starts a batch against s. Nothing is written until the
// first remove or add; finish must be called to complete it — a batch
// abandoned partway leaves the store without its complete mark, which
// the next open treats as no store (openStore).
func (s *Store) beginCommit(mode commitMode) *commitBatch {
	return &commitBatch{
		s:         s,
		mode:      mode,
		addIDs:    make(map[string][]uint64),
		removeIDs: make(map[string][]uint64),
	}
}

// begin opens the batch's next transaction if none is open. The first
// one also takes the complete mark away (keyComplete) and reads the id
// counter; every one sets the bulk fill factor when the mode asks.
func (w *commitBatch) begin() error {
	if w.tx != nil {
		return nil
	}
	tx, err := w.s.db.Begin(true)
	if err != nil {
		return err
	}
	w.tx = tx
	w.txBytes = 0
	w.touched = make(map[string]struct{})
	if w.mode == commitBulk {
		for _, name := range [][]byte{bucketEntries, bucketFiles, bucketNameTrigrams, bucketCalls, bucketCallers} {
			tx.Bucket(name).FillPercent = 1.0
		}
	}
	if !w.started {
		w.started = true
		mb := tx.Bucket(bucketMeta)
		w.nextID = readNextID(mb)
		if err := mb.Delete(keyComplete); err != nil {
			_ = tx.Rollback()
			w.tx = nil
			return err
		}
	}
	return nil
}

// flush commits the open transaction, if any, persisting the id
// counter with it so a later transaction — or a later batch — never
// hands out an id this one already did.
func (w *commitBatch) flush() error {
	if w.tx == nil {
		return nil
	}
	if w.assigned {
		if err := writeNextID(w.tx.Bucket(bucketMeta), w.nextID); err != nil {
			_ = w.tx.Rollback()
			w.tx = nil
			return err
		}
		w.touch('m', keyNextID)
		w.assigned = false
	}
	w.keys += len(w.touched)
	err := w.tx.Commit()
	w.tx = nil
	w.touched = nil
	return err
}

// abort rolls back whatever transaction is open. The complete mark
// stays gone: the store is rebuilt on its next open.
func (w *commitBatch) abort() {
	if w.tx != nil {
		_ = w.tx.Rollback()
		w.tx = nil
	}
}

func (w *commitBatch) touch(bucket byte, key []byte) {
	w.touched[string(bucket)+string(key)] = struct{}{}
}

// removeOwned takes away everything path owns in the open transaction:
// its entries and their trigram postings (batched into removeIDs), and
// its keys under every name its old calls record says it called. Its
// files and calls records are the caller's to replace or delete.
func (w *commitBatch) removeOwned(path string) error {
	eb := w.tx.Bucket(bucketEntries)
	fb := w.tx.Bucket(bucketFiles)
	cb := w.tx.Bucket(bucketCalls)
	rb := w.tx.Bucket(bucketCallers)
	names := make(map[string]bool)
	for _, c := range decodeCalls(cb.Get([]byte(path))) {
		names[c.Name] = true
	}
	for n := range names {
		key := callersKey(n, path)
		if rb.Get(key) == nil {
			continue
		}
		if err := rb.Delete(key); err != nil {
			return err
		}
		w.touch('r', key)
	}
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
				w.removeIDs[g] = append(w.removeIDs[g], id)
			}
		}
		if err := eb.Delete(idKey(id)); err != nil {
			return err
		}
		w.touch('e', idKey(id))
	}
	return nil
}

// remove takes a deleted path out of the store: its entries, postings
// and callers keys (removeOwned), then its files and calls records.
func (w *commitBatch) remove(path string) error {
	if err := w.begin(); err != nil {
		return err
	}
	if err := w.removeOwned(path); err != nil {
		w.abort()
		return err
	}
	fb := w.tx.Bucket(bucketFiles)
	cb := w.tx.Bucket(bucketCalls)
	if err := fb.Delete([]byte(path)); err != nil {
		w.abort()
		return err
	}
	w.touch('f', []byte(path))
	if cb.Get([]byte(path)) != nil {
		if err := cb.Delete([]byte(path)); err != nil {
			w.abort()
			return err
		}
		w.touch('c', []byte(path))
	}
	return nil
}

// add writes one changed file's fresh contribution: whatever its path
// owned before is removed (removeOwned), its entries get the next ids,
// its files record, its calls record and one callers key per name it
// calls are written, and its names' trigrams are queued for finish.
// Once the open transaction holds chunkBytes of values it is committed
// and the next add begins another.
func (w *commitBatch) add(c fileChange) error {
	if err := w.begin(); err != nil {
		return err
	}
	if err := w.addIn(c); err != nil {
		w.abort()
		return err
	}
	if w.txBytes >= chunkBytes {
		return w.flush()
	}
	return nil
}

func (w *commitBatch) addIn(c fileChange) error {
	if err := w.removeOwned(c.Path); err != nil {
		return err
	}
	eb := w.tx.Bucket(bucketEntries)
	fb := w.tx.Bucket(bucketFiles)
	cb := w.tx.Bucket(bucketCalls)
	rb := w.tx.Bucket(bucketCallers)

	ids := make([]uint64, 0, len(c.Entries))
	for _, e := range c.Entries {
		id := w.nextID
		w.nextID++
		w.assigned = true
		v := encodeEntry(e)
		if err := eb.Put(idKey(id), v); err != nil {
			return err
		}
		w.touch('e', idKey(id))
		w.txBytes += len(v)
		ids = append(ids, id)
		for _, g := range trigrams(strings.ToLower(e.Name)) {
			w.addIDs[g] = append(w.addIDs[g], id)
		}
	}
	rec := encodeFileRecord(Record{ModTime: c.ModTime, Size: c.Size, EntryIDs: ids})
	if err := fb.Put([]byte(c.Path), rec); err != nil {
		return err
	}
	w.touch('f', []byte(c.Path))
	w.txBytes += len(rec)

	n, err := putCalls(cb, c.Path, c.Calls, w.touch)
	if err != nil {
		return err
	}
	w.txBytes += n

	// One callers key per name this file calls, holding the file's
	// sites of that name in line order — Callers reads a name's keys
	// back in path order, so the whole list comes out sorted the way
	// sortCalls sorts, without any list ever being read to be extended.
	byName := make(map[string][]entry.Call)
	for _, call := range c.Calls {
		byName[call.Name] = append(byName[call.Name], call)
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		sites := byName[name]
		sortCalls(sites)
		key := callersKey(name, c.Path)
		v := encodeCalls(sites)
		if err := rb.Put(key, v); err != nil {
			return err
		}
		w.touch('r', key)
		w.txBytes += len(v)
	}
	return nil
}

// finish writes what only the whole batch can know — every touched
// gram's posting list, applied once (applyPosting); the enum cache
// blob, when writeEnum says it actually differs from what is stored;
// the walk cache blob; the id counter — puts the complete mark back,
// and commits. It returns the keys the whole batch touched.
//
// caches bundles the enum cache blob (directive 019) and the
// incremental walk cache blob (directive 021 item 2) — two parallel
// (blob []byte, write bool)-shaped facts that already had to change
// together (both come from the same enumerate call, in refresh.go) and
// so are one value, not two more parameters (code-manners M2;
// patterns.md §9). The walk blob has no write flag of its own: Walk
// always runs and always produces a fresh cache to persist, unlike the
// enum blob, whose cache-hit replay path can leave nothing new to
// write.
func (w *commitBatch) finish(caches cacheBlobs) (keysWritten int, err error) {
	if err := w.begin(); err != nil {
		return 0, err
	}
	if err := w.finishIn(caches); err != nil {
		w.abort()
		return 0, err
	}
	if err := w.flush(); err != nil {
		return 0, err
	}
	return w.keys, nil
}

func (w *commitBatch) finishIn(caches cacheBlobs) error {
	tx := w.tx
	tb := tx.Bucket(bucketNameTrigrams)
	grams := make(map[string]struct{}, len(w.addIDs)+len(w.removeIDs))
	for g := range w.addIDs {
		grams[g] = struct{}{}
	}
	for g := range w.removeIDs {
		grams[g] = struct{}{}
	}
	sortedGrams := make([]string, 0, len(grams))
	for g := range grams {
		sortedGrams = append(sortedGrams, g)
	}
	sort.Strings(sortedGrams)
	for _, g := range sortedGrams {
		if err := applyPosting(tb, g, w.addIDs[g], w.removeIDs[g]); err != nil {
			return err
		}
		w.touch('t', []byte(g))
	}

	if caches.writeEnum && caches.enum != nil {
		eb2, err := tx.CreateBucketIfNotExists(bucketEnum)
		if err != nil {
			return err
		}
		if err := eb2.Put(keyEnumBlob, caches.enum); err != nil {
			return err
		}
		w.touch('u', keyEnumBlob)
	}
	if caches.walk != nil {
		wb, err := tx.CreateBucketIfNotExists(bucketEnum)
		if err != nil {
			return err
		}
		if err := wb.Put(keyWalkBlob, caches.walk); err != nil {
			return err
		}
		w.touch('u', keyWalkBlob)
	}
	mb := tx.Bucket(bucketMeta)
	if err := mb.Put(keyComplete, []byte("1")); err != nil {
		return err
	}
	w.touch('m', keyComplete)
	return nil
}

// commit applies a differential set of changes as one batch (directive
// 013; commitBatch): each deleted path removed, each change added in
// path order, then finished. The form Refresh's differential update
// and the store's own tests use; a first build streams its changes into
// a batch directly instead (Refresh).
func (s *Store) commit(changes []fileChange, deletes []string, caches cacheBlobs, mode commitMode) (keysWritten int, err error) {
	w := s.beginCommit(mode)
	sortedDeletes := append([]string(nil), deletes...)
	sort.Strings(sortedDeletes)
	for _, path := range sortedDeletes {
		if err := w.remove(path); err != nil {
			return 0, err
		}
	}
	sortedChanges := append([]fileChange(nil), changes...)
	sort.Slice(sortedChanges, func(i, j int) bool { return sortedChanges[i].Path < sortedChanges[j].Path })
	for _, c := range sortedChanges {
		if err := w.add(c); err != nil {
			return 0, err
		}
	}
	return w.finish(caches)
}

// putCalls writes path's calls record, or deletes it when the file
// calls nothing — an absent key and an empty list are one fact, as for
// a trigram's postings. A file whose record was absent and stays absent
// touches no key. Returns the bytes written.
func putCalls(cb *bolt.Bucket, path string, calls []entry.Call, touch func(byte, []byte)) (int, error) {
	key := []byte(path)
	if len(calls) == 0 {
		if cb.Get(key) == nil {
			return 0, nil
		}
		touch('c', key)
		return 0, cb.Delete(key)
	}
	sorted := append([]entry.Call(nil), calls...)
	sortCalls(sorted)
	touch('c', key)
	v := encodeCalls(sorted)
	return len(v), cb.Put(key, v)
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
// (directive 009 item 3), Line/LineText (§31.4) and EndLine (directive
// 042), none of which the old cache persisted, because every caller
// used to get them from the same in-memory all slice Refresh had just
// patched them into. Here, every caller — including the fast,
// nothing-changed path, which never touches that computation again —
// reads entries back through this encoding, so these must round-trip
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
		strconv.Itoa(e.Line),
		sanitize(e.LineText),
		strconv.Itoa(e.EndLine),
		// DocText keeps its line breaks, as \x1f: sanitize would fold
		// them into spaces, and a result prints one of its lines.
		strings.ReplaceAll(strings.ReplaceAll(e.DocText, "\t", " "), "\n", "\x1f"),
		encodeBool(e.Test),
	}
	return []byte(strings.Join(fields, "\t"))
}

func decodeEntry(v []byte) (entry.Entry, error) {
	fields := strings.Split(string(v), "\t")
	if len(fields) != 17 {
		return entry.Entry{}, fmt.Errorf("expected 17 tab-separated fields, got %d", len(fields))
	}
	size, err1 := strconv.ParseInt(fields[10], 10, 64)
	declCount, err2 := strconv.Atoi(fields[11])
	line, err3 := strconv.Atoi(fields[12])
	endLine, err4 := strconv.Atoi(fields[14])
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
		return entry.Entry{}, fmt.Errorf("malformed Size/DeclCount/Line/EndLine fields: %q", v)
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
		EndLine:   endLine,
		DocText:   strings.ReplaceAll(fields[15], "\x1f", "\n"),
		Test:      fields[16] == "1",
	}, nil
}
