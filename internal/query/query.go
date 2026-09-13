// Package query parses --query and searches and ranks entries against a
// search word (specifications/giraffe.md §5, §7).
package query

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	"giraffe/internal/entry"
)

// Filter is a parsed --query value: attribute name -> the OR'd values
// accepted for it. Different attribute names are AND'd together (§5).
type Filter map[string][]string

// filterable is the set of attributes --query may narrow by.
//
// §22 C-10: before this directive, kind and scope were valid but not
// filterable — rejected with an explicit "not supported" error (see
// Parse), never silently ignored, so §5's rule against a silently
// ignored filter was not violated. But §6's stronger promise ("every
// attribute printed can be pasted into --query and work") was false as
// written, since 001 restricted filtering to path and project as a
// minimal-version convenience, not a design decision. kind and scope
// join here to restore it.
//
// name stays out, deliberately, not by oversight: find's positional
// word argument already does a case-insensitive substring match against
// name (see tier), so --query 'name:...' would either duplicate that or
// silently mean something different (exact match) for the same
// attribute depending on how it was spelled — worse than the gap it
// would close. Revisit if a caller ever needs to narrow by name
// alongside a *different* search word than the one being matched on.
var filterable = map[string]bool{"path": true, "project": true, "kind": true, "scope": true, "lang": true, "ext": true}

// ValidAttributes returns the common attribute names plus every
// language-position name actually present in entries: "the set of valid
// attribute names is derived from index contents, not maintained
// separately" (§18). scope joined the common set in §21 C-6; lang in
// §24 C-20; ext in §25 C-24.
func ValidAttributes(entries []entry.Entry) map[string]bool {
	valid := map[string]bool{"path": true, "name": true, "kind": true, "project": true, "scope": true, "lang": true, "ext": true}
	for _, e := range entries {
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
		if !filterable[name] {
			return nil, fmt.Errorf("attribute %q is valid but not supported by --query in this version (filtering is limited to: path, project, kind, scope, lang, ext)", name)
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
// exact value or directory prefix (so path:internal/retry narrows to
// everything under that directory); project matches exactly.
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
			if e.Path == v || strings.HasPrefix(e.Path, v+"/") {
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
		}
	}
	return false
}

// tier ranks how an entry matched word: 0 is an exact name match, 3 a
// path-only match, 4 no match at all. Lower is better.
func tier(e entry.Entry, lowerWord string) int {
	switch {
	case strings.EqualFold(e.Name, lowerWord):
		return 0
	case strings.Contains(strings.ToLower(e.Name), lowerWord):
		return 1
	case strings.Contains(strings.ToLower(e.Doc), lowerWord):
		return 2
	case strings.Contains(strings.ToLower(e.Path), lowerWord):
		return 3
	default:
		return 4
	}
}

const noMatch = 4

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

// Search returns the entries matching word — in name, doc, or path — and
// every attribute in filter, ranked symbol-name match first, then
// declaration-before-file-entry, then exported-before-unexported, then
// doc first-line match, then path match (the directive's fixed ranking
// for this version: "do not tune further"; §21 C-6 added the scope
// tie-break, §24 C-20 the declaration-versus-file one). invocationProject
// is the project the caller is searching from; package-scope entries
// outside it are excluded before ranking, not merely ranked low.
func Search(entries []entry.Entry, word string, filter Filter, invocationProject string) []entry.Entry {
	lw := strings.ToLower(word)
	var matches []entry.Entry
	for _, e := range entries {
		if !visible(e, invocationProject) {
			continue
		}
		if tier(e, lw) == noMatch {
			continue
		}
		if !filter.Match(e) {
			continue
		}
		matches = append(matches, e)
	}
	sort.SliceStable(matches, func(i, j int) bool {
		ti, tj := tier(matches[i], lw), tier(matches[j], lw)
		if ti != tj {
			return ti < tj
		}
		di, dj := declFileRank(matches[i]), declFileRank(matches[j])
		if di != dj {
			return di < dj
		}
		si, sj := scopeRank(matches[i]), scopeRank(matches[j])
		if si != sj {
			return si < sj
		}
		if matches[i].Name != matches[j].Name {
			return matches[i].Name < matches[j].Name
		}
		return matches[i].Path < matches[j].Path
	})
	return matches
}

// Browse returns every entry matching filter and visible from
// invocationProject, ordered by path then name (§22 item 5: `show`
// lists what is in a place — there is no search word to rank matches
// against, so declaration order stands in for relevance order). filter
// must be non-empty; the caller (main.runShow) rejects an empty one,
// since "show" always names a place rather than browsing the whole
// index.
func Browse(entries []entry.Entry, filter Filter, invocationProject string) []entry.Entry {
	var out []entry.Entry
	for _, e := range entries {
		if !visible(e, invocationProject) {
			continue
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
		return out[i].Name < out[j].Name
	})
	return out
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
func Suggest(entries []entry.Entry, word, invocationProject string) []string {
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
	for _, e := range entries {
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
