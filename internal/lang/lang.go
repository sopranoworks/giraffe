// Package lang holds the one route from a scanned file to the entries
// it produces (specifications/giraffe.md §18; directive 016 §1): every
// registered module is tried, in order, until one handles the file.
// main and index consult only Extract and RegisteredNames; neither
// contains language-specific logic itself, and no file extension is
// named anywhere in this package — only inside the module that claims
// it.
package lang

import (
	"sort"

	"giraffe/internal/entry"
	"giraffe/internal/lang/file"
	"giraffe/internal/lang/golang"
	"giraffe/internal/scanner"
)

// Extractor is what a language module is: given a scanned file, it
// either handles the file and returns the entries it produced
// (handled=true), or it does not, and the caller tries the next
// module in the registration order. A module that recognises the
// file's extension but fails to parse its content still answers
// handled=false — "not handled" covers both "this is not mine" and
// "this was mine and its content did not parse" (directive 016 §1:
// same-extension-different-language falls out of this without a
// special case). err, when non-nil, is a diagnostic worth reporting
// even when a later module went on to handle the file anyway.
//
// Declared here, in the package that consumes it, not beside any one
// implementation (language-policy G2).
type Extractor func(f scanner.File, project string, assignments map[string]string) (entries []entry.Entry, handled bool, err error)

// module pairs a language's own display name — the lang attribute's
// value (§24 C-20), unmapped per §6 — with its Extractor, for
// RegisteredNames' use only. Extract itself never branches on name;
// dispatch is entirely "ask the next module."
type module struct {
	name    string
	extract Extractor
}

// registered is every language module, tried in the order below. No
// extension is named here: this is a registration order and nothing
// else (§18; directive 016 §1). file.Extract is last and always
// handles, so Extract never falls through with nothing produced.
var registered = []module{
	{name: "go", extract: golang.Extract},
	{name: "", extract: file.Extract},
}

// Extract hands f to each registered module in turn and returns the
// first one's entries once it answers handled=true. declined carries
// forward whatever diagnostic a module that declined attached on its
// way past — e.g. a parse error after a module's own extension
// matched — for the caller to report (in ## warnings) alongside the
// entries a later module still produced; it is not itself a failure of
// this call.
func Extract(f scanner.File, project string, assignments map[string]string) (entries []entry.Entry, declined error) {
	for _, m := range registered {
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

// RegisteredNames returns every distinct language name a registered
// extractor answers to, sorted — the generic module's own empty name
// excluded, since it is not a language. Used to give ## index's
// language breakdown (§25 C-24) visibility even at zero: "go 0,
// unknown 181099" for llvm-project is an accurate statement precisely
// because Go is registered and genuinely found nothing, distinct from
// a language giraffe has no extractor for at all — a distinction the
// breakdown loses if an unrepresented registered language is simply
// omitted. Returns names, not extensions, deliberately: a caller
// outside this package (output.Summarize, via main) treats them as
// opaque strings, the same way it already treats Kind and Scope values
// (§21 C-12) — never a reason to import this package into common
// structures.
func RegisteredNames() []string {
	seen := make(map[string]bool, len(registered))
	for _, m := range registered {
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
