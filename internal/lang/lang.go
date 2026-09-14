// Package lang holds the one route from a scanned file to the entries
// it produces (specifications/giraffe.md §18; directive 016 §1): every
// registered module is tried, in order, until one handles the file.
// main and index consult only Extract, RegisteredNames and
// RegisteredMarkers; neither contains language-specific logic itself,
// and no file extension or marker name is named anywhere in this
// package — only inside the module that claims it.
package lang

import (
	"sort"

	"giraffe/internal/entry"
	"giraffe/internal/lang/exclude"
	"giraffe/internal/lang/file"
	"giraffe/internal/lang/golang"
	"giraffe/internal/scanner"
)

// Extractor is what a language module is: given a scanned file, it
// either handles the file and returns the entries it produced
// (handled=true), or it does not, and the caller tries the next
// module in the dispatch order. A module that recognises the file's
// extension but fails to parse its content still answers
// handled=false — "not handled" covers both "this is not mine" and
// "this was mine and its content did not parse" (directive 016 §1:
// same-extension-different-language falls out of this without a
// special case). err, when non-nil, is a diagnostic worth reporting
// even when a later module went on to handle the file anyway.
//
// A module reads f.Content() only if it needs to (directive 017 §1):
// f's content is not read from disk until something actually calls
// Content, so a module that declines by extension alone — this
// package's exclusion module, and golang.Extract's own extension
// check — never causes a read.
//
// Declared here, in the package that consumes it, not beside any one
// implementation (language-policy G2).
type Extractor func(f scanner.File, project string, assignments map[string]string) (entries []entry.Entry, handled bool, err error)

// module pairs a language's own display name — the lang attribute's
// value (§24 C-20), unmapped per §6 — with its Extractor, for
// RegisteredNames' use only, plus the root-marker basename (directive
// 017 §3) that moves it to the front of the chain for a directory that
// has one. Extract itself never branches on name; dispatch is entirely
// "ask the next module."
type module struct {
	name    string
	extract Extractor
	marker  string // root-marker basename this module claims; "" if none
}

// exclusionModule always runs first and is never reordered by a
// directory claim (directive 017 §2, §3): every other module's
// laziness (§1) depends on this one having already had the chance to
// claim — and decline to read — a file before anything downstream
// gets it.
var exclusionModule = module{name: "", extract: exclude.Extract}

// extractionModules is the reorderable middle of the chain: every
// module that can actually produce declarations from a file's
// content. Registration order here is also the default dispatch order
// for a directory no module's own marker claims (directive 017 §3).
var extractionModules = []module{
	{name: "go", extract: golang.Extract, marker: golang.Marker},
}

// genericModule always runs last and, like exclusionModule, is never
// reordered: every file must still reach it once every extraction
// module has declined (directive 016 §1).
var genericModule = module{name: "", extract: file.Extract}

// Extract hands f to each module, in dispatchOrder(dirMarkers)'s
// order, and returns the first one's entries once it answers
// handled=true. dirMarkers is the set of root-marker basenames present
// in f's own owning directory (scanner.OwningDir over scanner.MarkerDirs)
// — nil or empty when none apply, which dispatches in plain
// registration order. declined carries forward whatever diagnostic a
// module that declined attached on its way past — e.g. a parse error
// after a module's own extension matched — for the caller to report
// (in ## warnings) alongside the entries a later module still
// produced; it is not itself a failure of this call.
func Extract(f scanner.File, project string, assignments map[string]string, dirMarkers map[string]bool) (entries []entry.Entry, declined error) {
	for _, m := range dispatchOrder(dirMarkers) {
		es, handled, err := m.extract(f, project, assignments)
		if err != nil {
			declined = err
		}
		if handled {
			return es, declined
		}
	}
	return nil, declined // unreachable: file.Extract always handles
}

// dispatchOrder is exclusionModule, then extractionModules reordered
// so the module whose own marker is present in dirMarkers comes first
// (directive 017 §3), then genericModule.
func dispatchOrder(dirMarkers map[string]bool) []module {
	ordered := orderExtractionModules(extractionModules, dirMarkers)
	out := make([]module, 0, len(ordered)+2)
	out = append(out, exclusionModule)
	out = append(out, ordered...)
	out = append(out, genericModule)
	return out
}

// orderExtractionModules moves every module in mods whose own marker
// is present in dirMarkers to the front, preserving mods' own relative
// order within each of the two groups (directive 017 §3: "not
// exclusion, not a filter, an ordering change" — ordering alone
// decides which module sees an ambiguous file first, and registration
// order is arbitrary with respect to any one directory). A module with
// no marker (marker == "") never moves. Factored out from
// dispatchOrder as a pure function of its arguments, rather than
// reading the package-level extractionModules directly, so it is
// testable against a synthetic module set independent of how many real
// language modules are registered today.
func orderExtractionModules(mods []module, dirMarkers map[string]bool) []module {
	front := make([]module, 0, len(mods))
	back := make([]module, 0, len(mods))
	for _, m := range mods {
		if m.marker != "" && dirMarkers[m.marker] {
			front = append(front, m)
		} else {
			back = append(back, m)
		}
	}
	return append(front, back...)
}

// RegisteredNames returns every distinct language name a registered
// extraction module answers to, sorted — the exclusion and generic
// modules' own empty name excluded, since neither is a language. Used
// to give ## index's language breakdown (§25 C-24) visibility even at
// zero: "go 0, unknown 181099" for llvm-project is an accurate
// statement precisely because Go is registered and genuinely found
// nothing, distinct from a language giraffe has no extractor for at
// all — a distinction the breakdown loses if an unrepresented
// registered language is simply omitted. Returns names, not
// extensions, deliberately: a caller outside this package
// (output.Summarize, via main) treats them as opaque strings, the same
// way it already treats Kind and Scope values (§21 C-12) — never a
// reason to import this package into common structures.
func RegisteredNames() []string {
	seen := make(map[string]bool, len(extractionModules))
	for _, m := range extractionModules {
		if m.name != "" {
			seen[m.name] = true
		}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// RegisteredMarkers returns every extraction module's own root-marker
// basename — go.mod today — sorted. scanner's project-root discovery
// merges this into its own marker list (scanner.Scope.Markers), so a
// name a module owns is named exactly once, here, rather than kept as
// a second, separately maintained copy in the scanner (directive 017
// §3: "one authority over those names").
func RegisteredMarkers() []string {
	var markers []string
	for _, m := range extractionModules {
		if m.marker != "" {
			markers = append(markers, m.marker)
		}
	}
	sort.Strings(markers)
	return markers
}
