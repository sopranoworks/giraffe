// Package query parses --query and searches and ranks entries against a
// search word (specifications/giraffe.md §5, §7).
package query

import (
	"fmt"
	"iter"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"giraffe/internal/entry"
)

// Entries is what Search, Suggest, Browse, and ValidAttributes need from
// a corpus (specifications/giraffe.md §30 C-46, C-47): the full set, via
// All, and — for Search's name-based tiers — a narrowed set of entries
// whose Name could satisfy word, via Candidates, from whatever posting
// list the source keeps. This package declares the interface it needs
// rather than importing internal/index directly (Go's implicit
// interface satisfaction: *index.Store already has both methods), so
// neither package depends on the other and query stays free of any
// storage-format dependency, including bbolt's. A source with no
// narrowing structure at all can simply have Candidates return All,
// unfiltered — Search's own exact tier check is unaffected either way,
// only how much work it does to reach it.
//
// Callers is the call-site index's read side (directive 050): every
// call of name the source holds, in path then line order. Nothing
// here attaches it to every entry it visits — a list is one store read
// and a decode per entry, spent on thousands of entries a broad word
// matches and eight are printed — so it is attached where it is
// looked at (WithCallers): by Search and Browse before the filter,
// when the filter has a callers: constraint, and by the caller for
// the results it prints.
type Entries interface {
	All() iter.Seq[entry.Entry]
	Candidates(word string) iter.Seq[entry.Entry]
	Callers(name string) []entry.Call
}

// Slice wraps a plain []entry.Entry as an Entries source with no
// narrowing structure of its own — Candidates simply returns All,
// unfiltered. Used by tests that construct a literal entry set directly,
// and by any caller with no store to back Search/Suggest/Browse.
func Slice(entries []entry.Entry) Entries {
	return sliceEntries(entries)
}

type sliceEntries []entry.Entry

func (s sliceEntries) All() iter.Seq[entry.Entry] {
	return func(yield func(entry.Entry) bool) {
		for _, e := range s {
			if !yield(e) {
				return
			}
		}
	}
}

func (s sliceEntries) Candidates(word string) iter.Seq[entry.Entry] {
	return s.All()
}

// Callers of a literal entry set are whatever its entries were built
// carrying: a test that wants a name called constructs the entry with
// its Callers set, and this hands them back the way a store would.
func (s sliceEntries) Callers(name string) []entry.Call {
	for _, e := range s {
		if e.Name == name && len(e.Callers) > 0 {
			return e.Callers
		}
	}
	return nil
}

// Concat presents several Entries sources as one — the shape a multi-
// root search needs (specifications/giraffe.md §20 §2: one store per
// root, merged only after each is independently refreshed).
func Concat(sources ...Entries) Entries {
	return concatEntries(sources)
}

type concatEntries []Entries

func (c concatEntries) All() iter.Seq[entry.Entry] {
	return func(yield func(entry.Entry) bool) {
		for _, src := range c {
			for e := range src.All() {
				if !yield(e) {
					return
				}
			}
		}
	}
}

// Callers merges every source's list, in path then line order across
// sources — a name is one name across roots, matched by name alone.
func (c concatEntries) Callers(name string) []entry.Call {
	var out []entry.Call
	for _, src := range c {
		out = append(out, src.Callers(name)...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Line < out[j].Line
	})
	return out
}

func (c concatEntries) Candidates(word string) iter.Seq[entry.Entry] {
	return func(yield func(entry.Entry) bool) {
		for _, src := range c {
			for e := range src.Candidates(word) {
				if !yield(e) {
					return
				}
			}
		}
	}
}

// Filter is a parsed --query value: attribute name -> the OR'd values
// accepted for it. Different attribute names are AND'd together (§5).
type Filter map[string][]string

// Every valid attribute is filterable by construction (§6: "attribute
// names appearing in output are exactly the names --query accepts") —
// there is deliberately no second, separately-maintained list of
// filterable names: a language module that adds a position attribute
// (module, receiver, type, or a future language's own) becomes
// filterable the moment it is valid, via matchesAny's default case,
// with nothing else to register (directive 015 — the split between
// ValidAttributes and a hand-maintained filterable set is what left
// this false for fourteen directives).
//
// name was the one exception, from §22 C-10 through directive 017: this
// invariant was restored three times (C-10 for kind/scope, 015 for the
// positions, 017 for size/declarations/line) and each time left one
// name still unfilterable, most recently this one. Directive 018 §3
// closes it rather than repeating the pattern: name is filterable too
// now (matchesAny, below), as an exact match — deliberately distinct
// from find's own positional word, which still does a case-insensitive
// substring match against name (see tier). The two coexist because they
// answer different questions: the positional word is "what looks like
// this", --query 'name:...' is "exactly this, as one more AND'd
// condition alongside path, kind, project, ...". Browsing a place (no
// search word, only --query, §31.3) can now also narrow to one
// declaration by its exact name, which it could not before.

// ValidAttributes returns the common attribute names plus every
// language-position name actually present in entries: "the set of valid
// attribute names is derived from index contents, not maintained
// separately" (§18). scope joined the common set in §21 C-6; lang in
// §24 C-20; ext in §25 C-24; size, declarations and line in directive
// 017 §4; callers in directive 050 — every one of them a common Entry
// field, present (if often zero) on every entry, the same standing
// path/name/kind/project already have, not something derived per
// repository like a language position.
func ValidAttributes(src Entries) map[string]bool {
	valid := map[string]bool{
		"path": true, "name": true, "kind": true, "project": true,
		"scope": true, "lang": true, "ext": true,
		"size": true, "declarations": true, "line": true,
		"callers": true,
	}
	for e := range src.All() {
		for _, p := range e.Position {
			valid[p.Name] = true
		}
	}
	return valid
}

// Parse tokenizes a --query value into a Filter. The argument is one
// shell word; giraffe does its own tokenisation within it (§5): attributes
// are whitespace-separated, each is name:value with no space after the
// colon, and a value containing whitespace is wrapped in single quotes.
func Parse(raw string, validAttributes map[string]bool) (Filter, error) {
	tokens, err := tokenize(raw)
	if err != nil {
		return nil, err
	}
	f := make(Filter)
	for _, tok := range tokens {
		i := strings.IndexByte(tok, ':')
		if i <= 0 || i == len(tok)-1 {
			return nil, fmt.Errorf("malformed attribute %q: expected name:value", tok)
		}
		name, value := tok[:i], tok[i+1:]
		if !validAttributes[name] {
			return nil, fmt.Errorf("unknown attribute %q; valid attributes for this repository: %s", name, strings.Join(sortedKeys(validAttributes), ", "))
		}
		f[name] = append(f[name], value)
	}
	return f, nil
}

func tokenize(s string) ([]string, error) {
	var tokens []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'':
			inQuote = !inQuote
		case c == ' ' && !inQuote:
			if cur.Len() > 0 {
				tokens = append(tokens, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteByte(c)
		}
	}
	if inQuote {
		return nil, fmt.Errorf("unterminated quote in query: %q", s)
	}
	if cur.Len() > 0 {
		tokens = append(tokens, cur.String())
	}
	return tokens, nil
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Match reports whether e satisfies every attribute in f. path matches by
// substring (037: not a prefix — an agent writing path:kubectl or
// path:logs means "somewhere in the path", and neither directive 033's
// nor 036's real failing query named a repository-root prefix; a value
// carrying "/" still behaves like a prefix in practice, since that is
// what a substring of a path with a directory separator in it is);
// project matches exactly.
func (f Filter) Match(e entry.Entry) bool {
	for name, values := range f {
		if !matchesAny(name, e, values) {
			return false
		}
	}
	return true
}

func matchesAny(name string, e entry.Entry, values []string) bool {
	for _, v := range values {
		switch name {
		case "path":
			if strings.Contains(e.Path, v) {
				return true
			}
		case "project":
			if e.Project == v {
				return true
			}
		case "kind":
			if e.Kind == v {
				return true
			}
		case "scope":
			if e.Scope == v {
				return true
			}
		case "lang":
			if e.Lang == v {
				return true
			}
		case "ext":
			if e.Ext == v {
				return true
			}
		case "name":
			// Exact match (directive 018 §3), deliberately distinct from
			// find's own positional word, which substring-matches Name
			// case-insensitively (see tier) — the two are different
			// questions, not two spellings of the same one.
			if e.Name == v {
				return true
			}
		case "size":
			if strconv.FormatInt(e.Size, 10) == v {
				return true
			}
		case "declarations":
			if strconv.Itoa(e.DeclCount) == v {
				return true
			}
		case "line":
			// The value a result line prints (entry.Entry.LineRange —
			// "19-62", or "19" for a one-liner) matches as pasted (§7);
			// the bare start line matches too, so a caller who read
			// line:19-62 and typed line:19 is not told the declaration
			// is not there. A file entry has no line and matches neither.
			if e.Line > 0 && (e.LineRange() == v || strconv.Itoa(e.Line) == v) {
				return true
			}
		case "callers":
			// A site as the callers: line prints it (entry.Call.Place,
			// path:line), matched by substring the way path: is: the
			// whole site as pasted, or a file — callers:monitoring.go
			// keeps every declaration that file calls, which is the
			// call-site index's other direction (directive 050 §2)
			// asked from the query side.
			for _, c := range e.Callers {
				if strings.Contains(c.Place(), v) {
					return true
				}
			}
		default:
			// A language position (module, receiver, type, or a future
			// language module's own) — not a fixed case here, since
			// entry.Entry holds these as named pairs precisely so a
			// module adds one without changing this type (§18). Any
			// name ValidAttributes reports as present is reachable
			// here by construction: it came from some entry's own
			// Position slice, so matching against Position is always
			// the right fallback, never a name this switch needs its
			// own case for.
			for _, p := range e.Position {
				if p.Name == name && p.Value == v {
					return true
				}
			}
		}
	}
	return false
}

// tier ranks how an entry matched word: 0 is an exact name match on a
// top-level declaration, 5 a path-only match, 6 no match at all. Lower
// is better.
//
// §29 C-42: a method or field's name match is split into its own pair
// of tiers, below both of a top-level declaration's — not merely
// tie-broken within a shared tier (memberRank, §28 C-38, did that and
// it was not enough: it orders entries within a tier but cannot decide
// which fill the response budget once a tier holds more than
// maxResults entries on its own). This is evidential weight, not
// capacity: that a type is named "Cluster" says a great deal; that an
// unrelated type has a field named "cluster" says almost nothing, and
// the two must not compete for the same slots as equals. A member's
// exact match still outranks any doc or path match, top-level or not —
// it remains the strongest evidence available once no top-level name
// match exists, which is what keeps a query like "GetMailboxLengths",
// answered only by a method, working exactly as before.
func tier(e entry.Entry, lowerWord string) int {
	if t, ok := nameTier(e, lowerWord); ok {
		return t
	}
	if t, ok := docPathTier(e, lowerWord); ok {
		return t
	}
	return noMatch
}

const noMatch = 6

// nameTier is tier's name-based half (tiers 0-3): an exact or substring
// match on Name, split by member-vs-top-level (§29 C-42). Factored out
// from tier so Search can run it only against the candidate set a
// posting list narrows to (§30 C-47), rather than every entry — the
// check itself is unchanged, only when it runs.
func nameTier(e entry.Entry, lowerWord string) (int, bool) {
	member := isMember(e)
	switch {
	case strings.EqualFold(e.Name, lowerWord) && !member:
		return 0, true
	case strings.Contains(strings.ToLower(e.Name), lowerWord) && !member:
		return 1, true
	case strings.EqualFold(e.Name, lowerWord) && member:
		return 2, true
	case strings.Contains(strings.ToLower(e.Name), lowerWord) && member:
		return 3, true
	}
	return 0, false
}

// ExactName reports whether e is a declaration named exactly word,
// case-insensitively — the match nameTier ranks first for a top-level
// declaration (tier 0) and first among members (tier 2), and the one a
// caller who typed a name they already had is making. It is what
// decides which results print their text under ## source (directive
// 044). A file entry is never one: its name is a path's basename, and
// a file has no declaration text of its own to print.
func ExactName(e entry.Entry, word string) bool {
	return e.Kind != entry.KindFile && strings.EqualFold(e.Name, word)
}

// docPathTier is tier's remaining half (tiers 4-5): a substring match on
// Doc or Path, checked only for an entry nameTier already ruled out —
// tier's original switch never reaches these cases once a name match is
// found, and Search preserves that by construction (see Search).
func docPathTier(e entry.Entry, lowerWord string) (int, bool) {
	switch {
	case strings.Contains(strings.ToLower(e.Doc), lowerWord):
		return 4, true
	case strings.Contains(strings.ToLower(e.Path), lowerWord):
		return 5, true
	}
	return 0, false
}

// isMember reports whether e is a method or field rather than a
// top-level declaration or a file (§28 C-38's two new Kind values).
func isMember(e entry.Entry) bool {
	return e.Kind == "method" || e.Kind == "field"
}

// declFileRank orders a declaration above a file entry within the same
// match tier (§24 C-20): a func or type named "retry" answering "does
// this exist" outranks the file retry.go merely being named that. This
// is a difference in how structured the entry is, not in which
// language it came from — lang is not read here, by design.
func declFileRank(e entry.Entry) int {
	if e.Kind == entry.KindFile {
		return 1
	}
	return 0
}

// scopeRank orders exported above unexported within the same match tier
// (§21 C-6: "exported ranks above unexported by default"). Driven by
// Exported, not by comparing Scope's displayed value — §21 C-12: the
// language module's boolean judgement, not a common-code guess at which
// of a language's own words means "exported".
func scopeRank(e entry.Entry) int {
	if e.Exported {
		return 0
	}
	return 1
}

// visible reports whether e may appear in search results from an
// invocation in invocationProject. A declaration not referenceable
// outside its own module cannot be reused elsewhere, so it is hidden
// there — surfacing it would be noise the response budget cannot afford
// (§21 C-6, §7). Exported entries are always visible.
func visible(e entry.Entry, invocationProject string) bool {
	return e.Exported || e.Project == invocationProject
}

// Search returns the entries matching word and every attribute in
// filter, ranked by tier first (§29 C-42: exact top-level name match,
// then substring top-level name match, then exact member name match,
// then substring member name match, then doc first-line match, then
// path match — a member's name match now sits below both of a
// top-level declaration's, not merely tied and tie-broken against it,
// since it is weaker evidence of what the caller is looking for), then
// within a tier declaration-before-file-entry, then
// exported-before-unexported (§21 C-6 added the scope tie-break, §24
// C-20 the declaration-versus-file one), then shorter-name-first
// (directive 042), then name, then path. §7's "do not tune further" was
// written when the index held only top-level declarations; §29 treats
// it as a calibration to recheck, not a permanent rule, now that
// members have changed the population it was calibrated against.
//
// Shorter name first, within a tier: a substring tier holds every name
// the word appears in, and the name the word covers more of is the
// closer match — "NodeMetrics" and "MetricsSnapshot" answer "metrics"
// more directly than "DefaultMetricsExporterConfig" does. Before this
// the tie-break was the name itself, alphabetically, so a page of
// eight showed whichever substring matches happened to start with A
// through G: on gekka, `find metrics` (the first query in every one of
// 041's aided runs) filled its page with ClusterMetrics*,
// DefaultMetrics* and GossipMetrics* and never showed NodeMetrics or
// MetricsSnapshot — the two types the task turned on — at all. §12
// item 1: the ranking is tuned against real queries, and these are the
// real queries. 041 built and measured this, and reverted it under its
// own token-median rule; 042 takes it on this ground instead.
//
// invocationProject is the project the caller is searching from;
// package-scope entries outside it are excluded before ranking, not
// merely ranked low.
//
// §30 C-46, C-47: src supplies two views rather than one materialized
// slice — All, and Candidates(word), narrowed by whatever posting list
// the source keeps (a name-trigram index, for the store this ships
// with). Name-based tiers (0-3) are found via Candidates; Doc/Path
// tiers (4-5) still visit every entry via All, since neither is
// posting-indexed (a scope decision recorded in the report, not an
// oversight) — but every entry visited there is decoded lazily from the
// store's mmap, not out of a slice already fully materialized in
// memory, so nothing not actually returned as a match is ever GC-owned.
func Search(src Entries, word string, filter Filter, invocationProject string) []entry.Entry {
	lw := strings.ToLower(word)
	var ranked []rankedEntry
	// nameMatched records every entry nameTier already accounted for
	// (matched or not visible/filtered), keyed by identity, so the
	// second loop below — which checks Doc/Path — never re-examines an
	// entry the name-based pass already resolved. tier's original
	// switch never falls through to a Doc/Path check once a name match
	// is found; this preserves that without re-running nameTier over
	// every entry a second time (§30 C-47: Candidates narrows the first
	// loop to a small set via the name-trigram posting list, so this
	// map stays small too — it is not the full-corpus structure it
	// would be if built from src.All()).
	nameMatched := make(map[string]bool)
	for e := range src.Candidates(word) {
		if !visible(e, invocationProject) {
			continue
		}
		t, ok := nameTier(e, lw)
		if !ok {
			continue
		}
		nameMatched[dedupKey(e)] = true
		if filter.constrainsCallers() {
			e = WithCallers(e, src)
		}
		if !filter.Match(e) {
			continue
		}
		ranked = append(ranked, rankedEntry{e: e, tier: t, declFile: declFileRank(e), scope: scopeRank(e)})
	}
	for e := range src.All() {
		if nameMatched[dedupKey(e)] {
			continue
		}
		if !visible(e, invocationProject) {
			continue
		}
		t, ok := docPathTier(e, lw)
		if !ok {
			continue
		}
		if filter.constrainsCallers() {
			e = WithCallers(e, src)
		}
		if !filter.Match(e) {
			continue
		}
		// §29 C-43: tier, declFileRank and scopeRank are computed once
		// per matched entry here, not recomputed on every pairwise
		// comparison below. A word matching thousands of entries (a
		// common short name, now with members in the mix) drives
		// sort.SliceStable's comparator into the tens of thousands of
		// calls; tier() alone allocates via strings.ToLower on every
		// call, so recomputing it per comparison rather than per entry
		// turned an O(n) cost into an O(n log n) one for no reason —
		// measured (report) to grow faster than entry count alone
		// predicts, unlike every other stage of the miss and hit paths.
		// The rank and the entry travel together in one slice so
		// sort.SliceStable's swaps cannot desynchronise them, which a
		// second, separately-swapped slice would risk.
		ranked = append(ranked, rankedEntry{e: e, tier: t, declFile: declFileRank(e), scope: scopeRank(e)})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		ri, rj := ranked[i], ranked[j]
		if ri.tier != rj.tier {
			return ri.tier < rj.tier
		}
		if ri.declFile != rj.declFile {
			return ri.declFile < rj.declFile
		}
		if ri.scope != rj.scope {
			return ri.scope < rj.scope
		}
		if len(ri.e.Name) != len(rj.e.Name) {
			return len(ri.e.Name) < len(rj.e.Name)
		}
		if ri.e.Name != rj.e.Name {
			return ri.e.Name < rj.e.Name
		}
		return ri.e.Path < rj.e.Path
	})
	matches := make([]entry.Entry, len(ranked))
	for i, r := range ranked {
		matches[i] = r.e
	}
	return matches
}

// rankedEntry pairs a matched entry with its ranking, computed once
// (see Search) rather than recomputed on every pairwise comparison
// during sorting.
type rankedEntry struct {
	e        entry.Entry
	tier     int
	declFile int
	scope    int
}

// dedupKey identifies an entry for Search's nameMatched set. Path+Name+
// Kind is unique in practice: two declarations at the same path with
// the same name and kind would already be a compile error in any
// extractable language, so no language module can produce two entries
// this key does not distinguish. entry.Entry itself is not comparable
// (Position is a slice), so a string key stands in for identity here.
func dedupKey(e entry.Entry) string {
	return e.Path + "\x00" + e.Name + "\x00" + e.Kind
}

// Browse returns every entry matching filter and visible from
// invocationProject, ordered by path, then line, then name (§31.3:
// browsing a place — find with no search word and a --query — lists
// what is there; there is no search word to rank matches against, so
// file order stands in for relevance order). Within one file the order
// is the file's own (directive 043, from 041's branch): a declaration's
// line is the order a reader of that file meets it in, and the file
// entry (line 0) comes first, ahead of everything declared inside it.
// Before 043 it was name order, which scattered a struct's fields
// alphabetically away from the struct and, once every line carried a
// range (042), printed line:117-134 above line:19-60 above line:32.
// filter must be non-empty; the caller (main.runFind) rejects an empty
// one, since browsing always names a place rather than dumping the
// whole index.
func Browse(src Entries, filter Filter, invocationProject string) []entry.Entry {
	var out []entry.Entry
	for e := range src.All() {
		if !visible(e, invocationProject) {
			continue
		}
		if filter.constrainsCallers() {
			e = WithCallers(e, src)
		}
		if !filter.Match(e) {
			continue
		}
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		if out[i].Line != out[j].Line {
			return out[i].Line < out[j].Line
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// constrainsCallers reports whether f has a callers: constraint — the
// one attribute whose value is not on an entry as the store decodes it
// (see Entries.Callers), so Search and Browse attach it before matching
// only when the filter will look at it.
func (f Filter) constrainsCallers() bool {
	_, ok := f["callers"]
	return ok
}

// WithCallers returns e carrying where its name is called (directive
// 050), ordered for a reader who will see a bounded prefix of the list
// (orderCallers). One key read; the caller decides which entries are
// worth it — a filter with a callers: constraint (Search, Browse), or
// the page and ## source entries about to be printed (main). A file
// entry is not called and carries none.
func WithCallers(e entry.Entry, src Entries) entry.Entry {
	if e.Kind == entry.KindFile {
		return e
	}
	e.Callers = orderCallers(e, src.Callers(e.Name))
	return e
}

// orderCallers ranks a name's call sites for the entry they are shown
// under: a site whose qualifier names one of the entry's own position
// values — c.metrics.Snapshot(...) and ms.provider.Metrics().Snapshot(
// ...) under the Snapshot method whose receiver is NodeMetrics — comes
// before one whose qualifier does not (rb.Snapshot() under the same
// entry), and within each group the stored path-then-line order
// stands. "Names" is: the value equals the qualifier or ends with it,
// case-insensitively (namesValue) — a field or accessor is named for
// its type, in whole or by the type name's last word, and a one-letter
// receiver (g, rb) names nothing. Matching is by name, so the list
// holds every Snapshot in the corpus; the qualifier is the one fact a
// call site carries about which one it means, and it puts the sites
// most likely to be this declaration's within the bounded prefix a
// result line prints, without resolving a type. Sites that match are
// no more certain than the rest — a place to open, as ## source is.
func orderCallers(e entry.Entry, calls []entry.Call) []entry.Call {
	if len(calls) == 0 {
		return nil
	}
	named := func(c entry.Call) bool {
		for _, p := range e.Position {
			if namesValue(c.Via, p.Value) {
				return true
			}
		}
		return false
	}
	out := make([]entry.Call, len(calls))
	copy(out, calls)
	sort.SliceStable(out, func(i, j int) bool {
		return named(out[i]) && !named(out[j])
	})
	return out
}

// namesValue reports whether a call's qualifier via names value: the
// whole of it, or its last word — a tail that starts at a capital,
// "metrics" in NodeMetrics, "provider" in ClusterProvider — compared
// case-insensitively. A tail that starts mid-word ("s" in NodeMetrics,
// "trics") is not a name for it, and an empty via (a bare call) names
// nothing.
func namesValue(via, value string) bool {
	if via == "" || len(via) > len(value) {
		return false
	}
	at := len(value) - len(via)
	if !strings.EqualFold(value[at:], via) {
		return false
	}
	r, _ := utf8.DecodeRuneInString(value[at:])
	return at == 0 || unicode.IsUpper(r)
}

// DiagnoseFilter explains a zero-result answer that carried a non-empty
// filter (directive 034, revised by 035): an unknown attribute *name* is
// already a parse-time error (§5, Parse) — an unknown *value* was not,
// and read identically to a query whose constraints are all real but
// whose intersection happens to be empty. The index holds every value of
// every attribute, so which of those two happened is not a guess.
//
// Every attr:value pair in filter is checked against the whole index,
// independent of word and independent of the filter's other
// constraints — "is this a real value" is a fact about the corpus, not
// about this particular query, the same principle Suggest already
// applies to a missed word. A constraint whose value matches nothing
// anywhere, under any attribute, is reported as such; one whose value
// exists under a *different* attribute names every attribute it is real
// under, not the alphabetically-first one (035: 034 shipped a single
// first-match report — "path:kubectl" is not a path — it is a name" —
// and 035 found this hid an equally-true, more-useful answer:
// "project:kubectl" is exactly as real, in the one case with evidence.
// Listing the attributes a value actually appears under is not a guess
// and not a ranking — §23 C-16's "no fuzzy matching, no spelling
// correction, no ranked candidates" governs invention, not enumeration
// of facts the index already holds). If every constraint turns out to be
// individually real, that is reported too, distinct from either — a
// query that legitimately intersected to nothing.
//
// Only meaningful once the caller already knows word+filter together
// matched nothing; DiagnoseFilter does not itself decide whether to
// call it, and returns nil for an empty filter (nothing to diagnose).
func DiagnoseFilter(src Entries, filter Filter) []string {
	if len(filter) == 0 {
		return nil
	}
	valid := ValidAttributes(src)
	attrs := make([]string, 0, len(filter))
	for attr := range filter {
		attrs = append(attrs, attr)
	}
	sort.Strings(attrs)

	var out []string
	for _, attr := range attrs {
		for _, v := range filter[attr] {
			if valueExists(src, attr, v) {
				continue
			}
			if others := findOtherAttrs(src, valid, attr, v); len(others) > 0 {
				out = append(out, fmt.Sprintf("%q is wrong - it is in [%s]", attr+":"+v, strings.Join(others, ", ")))
			} else {
				out = append(out, fmt.Sprintf("no such %s: %q", attr, v))
			}
		}
	}
	if len(out) == 0 {
		out = append(out, "every constraint in this query matches something in the index on its own; their combination does not")
	}
	return out
}

// valueExists reports whether any entry in src satisfies attr:value —
// the same matchesAny a real filter would use, checked in isolation
// rather than as one AND'd condition among several, so the answer is
// about the corpus, not about the rest of the query it appeared in.
func valueExists(src Entries, attr, value string) bool {
	for e := range src.All() {
		if matchesAny(attr, e, []string{value}) {
			return true
		}
	}
	return false
}

// findOtherAttrs looks for value under every valid attribute except
// exclude, one pass over src rather than one pass per candidate
// attribute — each entry is checked against every not-yet-found
// attribute name before moving to the next entry. Unlike a first-match
// search, this cannot stop once one attribute is confirmed (035: a value
// can be genuinely real under more than one attribute, and reporting
// only the alphabetically-first one hid the more useful answer in the
// one case with evidence — see DiagnoseFilter); an already-found
// attribute is skipped on later entries so no attribute is confirmed
// twice, but every entry is still visited until every valid attribute
// is accounted for or the scan ends. Returns nil when value exists under
// no attribute at all, the same as exhausting every possibility rather
// than a guess. The returned slice is alphabetical, carrying no implied
// preference among the attributes found (035: an order by anything else
// — match count, an exactness heuristic — would be a ranking, and the
// one case with evidence does not even discriminate between its two
// real answers).
func findOtherAttrs(src Entries, valid map[string]bool, exclude, value string) []string {
	others := sortedKeys(valid)
	found := make(map[string]bool, len(others))
	remaining := len(others)
	if valid[exclude] {
		remaining--
	}
	for e := range src.All() {
		if remaining == 0 {
			break
		}
		for _, attr := range others {
			if attr == exclude || found[attr] {
				continue
			}
			if matchesAny(attr, e, []string{value}) {
				found[attr] = true
				remaining--
			}
		}
	}
	if len(found) == 0 {
		return nil
	}
	names := make([]string, 0, len(found))
	for attr := range found {
		names = append(names, attr)
	}
	sort.Strings(names)
	return names
}

// prefixLen picks how many leading characters of word two names must
// share to count as a near miss below: short enough to catch a small
// typo or an unfinished word, long enough that a one- or two-letter
// word does not match half the index.
func prefixLen(word string) int {
	n := len(word)
	if n > 4 {
		return 4
	}
	return n
}

// identifierTokens splits an identifier into its lowercase parts on
// camelCase, PascalCase, and snake_case/kebab-case boundaries (§23
// C-16). "runShow" -> ["run", "show"]; "showCommand" -> ["show",
// "command"]; "HTTPServer" -> ["http", "server"] (a run of uppercase
// letters breaks before the last one when it starts a new word, so an
// acronym does not swallow the word that follows it).
func identifierTokens(name string) []string {
	runes := []rune(name)
	var tokens []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			tokens = append(tokens, strings.ToLower(cur.String()))
			cur.Reset()
		}
	}
	for i, r := range runes {
		switch {
		case r == '_' || r == '-':
			flush()
		case unicode.IsUpper(r) && i > 0 && (unicode.IsLower(runes[i-1]) || unicode.IsDigit(runes[i-1])):
			flush() // lower/digit -> upper: "runShow" splits before "S"
			cur.WriteRune(r)
		case unicode.IsUpper(r) && i > 0 && i+1 < len(runes) && unicode.IsLower(runes[i+1]) && unicode.IsUpper(runes[i-1]):
			flush() // last of an uppercase run followed by lowercase: "HTTPServer" splits before "Server"
			cur.WriteRune(r)
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return tokens
}

// couldShareToken is a cheap necessary condition for sharesToken: a
// token equal to one of wordTokens must appear, verbatim, somewhere in
// name — a shared token is a substring by construction — so if none of
// wordTokens even appears as a case-insensitive substring of name,
// sharesToken is guaranteed false and the full boundary-aware split can
// be skipped entirely.
func couldShareToken(name string, wordTokens []string) bool {
	lower := strings.ToLower(name)
	for _, t := range wordTokens {
		if strings.Contains(lower, t) {
			return true
		}
	}
	return false
}

// sharesToken reports whether name has any identifier token in common
// with wordTokens (already split once by the caller, since Suggest
// checks it against every entry in the index).
func sharesToken(name string, wordTokens map[string]bool) bool {
	for _, t := range identifierTokens(name) {
		if wordTokens[t] {
			return true
		}
	}
	return false
}

// Suggest offers evidence-backed directions to try after word matched
// nothing (§22: `## truncated` already tells a caller how to narrow the
// same question; nothing told it how to switch to a different one).
// Three things are cheap to check and are backed by the index rather
// than invented:
//
//   - a match exists but is hidden from invocationProject because it is
//     not exported elsewhere — the caller is looking from the wrong
//     project, not at a name that does not exist
//   - a name elsewhere in the index shares word's leading characters —
//     worth trying if the word was a small typo
//   - a name elsewhere shares an identifier token with word once both
//     are split on camelCase/snake_case boundaries (§23 C-16) — worth
//     trying if the word guessed the wrong *shape* of a compound name
//     ("showCommand" for what was actually named "runShow": no shared
//     prefix, but both contain "show")
//
// The two near-name checks catch different misses and neither subsumes
// the other — a single-token typo ("Retri" for "Retry") shares a
// prefix but no whole token; a restructured compound name shares a
// token but often no prefix — so both run.
//
// Returns nil, not a fixed list of generic advice, when none holds: "do
// not fabricate a hint that is not backed by the index" (§22).
//
// couldShareToken gates the token-overlap check (§26 C-28/item 4:
// "reduce what reaches the tokeniser with a cheap pre-filter"). 007's
// CPU profile pinned identifierTokens — specifically strings.ToLower,
// called once per flushed token — as Suggest's largest cost, run over
// every entry that reaches this point. Two other directions were tried
// and measured first: folding case per rune instead of once per token
// (roughly a wash — Go's strings.ToLower already has an ASCII fast
// path a per-rune loop does not get) and memoising identifierTokens by
// Name across one Suggest call (a clear win on kubernetes, where
// declaration names repeat, but a net loss on llvm-project: at that
// corpus's lower name-repetition ratio the map's own overhead
// exceeded what it saved). Neither shipped. The pre-filter measured a
// 53-54% reduction in wall time on both corpora instead (see the
// report), because it is a correct, cheap, universal filter rather
// than betting on how much a particular corpus's names repeat:
// sharesToken can only be true if some wordToken appears, verbatim, as
// a substring of name (a token is literally carved out of name), so
// checking that first is a true necessary condition — it never
// produces a false negative, only an occasional false positive
// (TestCouldShareToken_NecessaryButNotSufficient) that the real,
// boundary-aware check below still filters out correctly.
func Suggest(src Entries, word, invocationProject string) []string {
	lw := strings.ToLower(word)
	pl := prefixLen(lw)
	wordTokenList := identifierTokens(word)
	wordTokens := make(map[string]bool, len(wordTokenList))
	for _, t := range wordTokenList {
		wordTokens[t] = true
	}

	hiddenIn := make(map[string]int) // project -> count of matches hidden there
	nearPrefix := make(map[string]bool)
	nearToken := make(map[string]bool)
	// Suggest only runs once Search has already found nothing (§22), so
	// every entry is visited here regardless — there is no candidate set
	// to narrow to when the answer is already "no name match anywhere."
	// §30's posting list speeds up confirming that (Search's own
	// Candidates lookup), not this pass, which is unaffected — directive
	// 011 already measured Suggest was never the miss-path bottleneck.
	for e := range src.All() {
		if tier(e, lw) != noMatch {
			if !visible(e, invocationProject) {
				hiddenIn[e.Project]++
			}
			continue
		}
		if pl >= 2 && len(e.Name) >= pl && strings.EqualFold(e.Name[:pl], lw[:pl]) {
			nearPrefix[e.Name] = true
			continue
		}
		if !couldShareToken(e.Name, wordTokenList) {
			continue
		}
		if sharesToken(e.Name, wordTokens) {
			nearToken[e.Name] = true
		}
	}

	var out []string
	if len(hiddenIn) > 0 {
		projects := make([]string, 0, len(hiddenIn))
		for p := range hiddenIn {
			projects = append(projects, p)
		}
		sort.Strings(projects)
		for _, p := range projects {
			out = append(out, fmt.Sprintf(
				"%d entries match %q in project %s but are not exported there — search again from within %s",
				hiddenIn[p], word, p, p))
		}
	}
	if len(nearPrefix) > 0 {
		out = append(out, "names in the index starting the same way: "+joinCapped(nearPrefix, 5))
	}
	if len(nearToken) > 0 {
		out = append(out, "names in the index sharing part of the word: "+joinCapped(nearToken, 5))
	}
	return out
}

// joinCapped sorts the keys of names and joins at most max of them,
// comma-separated — the response budget (§7) applies to a suggestion
// line the same as to a result line.
func joinCapped(names map[string]bool, max int) string {
	list := make([]string, 0, len(names))
	for n := range names {
		list = append(list, n)
	}
	sort.Strings(list)
	if len(list) > max {
		list = list[:max]
	}
	return strings.Join(list, ", ")
}
