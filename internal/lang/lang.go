// Package lang holds the one place file extensions are mapped to a
// language module (specifications/giraffe.md §18): main and every other
// package consult this table; none of them contain language-specific
// logic themselves.
package lang

import (
	"sort"

	"giraffe/internal/entry"
	"giraffe/internal/lang/golang"
	"giraffe/internal/scanner"
)

// Extractor is what a language module is: a function that turns one
// scanned file into the entries found in it. Declared here, in the
// package that consumes it, not beside either implementation
// (language-policy G2).
type Extractor func(f scanner.File, project string) ([]entry.Entry, error)

// module pairs a language's own display name — the lang attribute's
// value (§24 C-20), unmapped per §6 — with the function that extracts
// from it.
type module struct {
	name    string
	extract Extractor
}

// registry maps a file extension to the module that extracts from it.
//
// Go is the only entry this version registers. The table is nonetheless
// the extension point languages are added through later (§10: "add a
// language when it is actually used, driven by observed need") — a data
// addition here, not a restructuring of main or the scanner.
var registry = map[string]module{
	".go": {name: "go", extract: golang.Extract},
}

// For returns the extractor registered for ext, if any.
func For(ext string) (Extractor, bool) {
	m, ok := registry[ext]
	if !ok {
		return nil, false
	}
	return m.extract, true
}

// NameFor returns ext's lang value: a registered extractor's own name
// if one exists, otherwise the operator's own assignment for ext, if
// one was written in .giraffe/exclude (§25 C-26). Neither existing is
// not an error — most extensions have no assigned language — so the
// result is "", not a placeholder (§25 C-24: "none" was withdrawn
// because it delivered neither visibility nor a narrowing axis).
// assignments comes from index.LoadExclude; nil is a valid empty set.
//
// An extractor's own registration always wins over an operator
// assignment for the same extension: the assignment exists to label
// what giraffe cannot otherwise parse (C-26), not to relabel what it
// already extracts correctly.
func NameFor(ext string, assignments map[string]string) string {
	if m, ok := registry[ext]; ok {
		return m.name
	}
	if name, ok := assignments[ext]; ok {
		return name
	}
	return ""
}

// RegisteredNames returns every distinct language name a registered
// extractor answers to, sorted. Used to give ## index's language
// breakdown (§25 C-24) visibility even at zero: "go 0, unknown 181099"
// for llvm-project is an accurate statement precisely because Go *is*
// registered and genuinely found nothing, distinct from a language
// giraffe has no extractor for at all — a distinction the breakdown
// loses if an unrepresented registered language is simply omitted.
// Returns names, not extensions, deliberately: a caller outside this
// package (output.Summarize, via main) treats them as opaque strings,
// the same way it already treats Kind and Scope values (§21 C-12) —
// never a reason to import this package into common structures.
func RegisteredNames() []string {
	seen := make(map[string]bool, len(registry))
	for _, m := range registry {
		seen[m.name] = true
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
