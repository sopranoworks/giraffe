// Package exclude is the chain's first module (directive 017 §2): every
// file is offered to it before anything else, and it claims — answers
// handled=true, produces no entries — every extension and extensionless
// shape that should never be indexed at all. Nothing downstream, not
// even the generic module, ever sees a file this module claims — unless
// the operator has said otherwise (directive 018 §2, below).
//
// This is the same chain-of-responsibility structure directive 016 §1
// already established and justified against design-review's
// patterns.md §5 (order is load-bearing: this module must run first,
// and a module can genuinely decline). Adding this module does not
// reopen that choice; it is one more member of an already-justified
// chain.
package exclude

import (
	"path"

	"giraffe/internal/entry"
	"giraffe/internal/scanner"
)

// deniedExtensions are binary, compiled and archive formats excluded
// from indexing by default — moved here from scanner.deniedExtensions
// (directive 017 §2: "the scanner stops knowing about extensions
// entirely"). The extension already declares the type, so a deny list
// is enough: no content is ever read to decide.
var deniedExtensions = []string{
	".png", ".jpg", ".jpeg", ".gif", ".ico", ".bmp", ".webp",
	".o", ".out", ".so", ".a", ".dylib", ".dll", ".exe", ".class", ".pyc", ".wasm",
	".zip", ".tar", ".gz", ".bz2", ".xz", ".jar", ".war", ".pdf",
	".woff", ".woff2", ".ttf", ".eot", ".mp3", ".mp4", ".mov",
}

// knownExtensionless are extensionless files worth indexing by name —
// moved here from scanner.knownExtensionless (directive 017 §2: "the
// same shape — a module's knowledge about which files it claims"; kept
// beside deniedExtensions rather than split across two modules, since
// both answer the same question, "should this module claim the file",
// from the name alone). An extensionless file not on this list is
// claimed (excluded): it has exactly the shape of a compiled binary,
// and defaulting to include is the dangerous direction.
var knownExtensionless = []string{
	"Makefile", "Dockerfile", "CMakeLists.txt", "LICENSE", "README",
	"Rakefile", "Gemfile", "Procfile", "Vagrantfile",
}

// Extract claims f — decided from f.Path alone, so f.Content is never
// called — when its extension or extensionless shape marks it as
// something that should never be indexed. handled=true with no entries
// means exactly that: f is excluded, and (directive 017 §1) since
// nothing here ever calls f.Content, an excluded file is never read
// either. handled=false means this module has no opinion; the chain
// continues.
//
// An operator assignment for f's own extension (assignments, from
// .giraffe/exclude's "ext:<x> lang:<y>" lines, §25 C-26) always wins
// over the deny list (directive 018 §2: "a project that wants a .png
// indexed by name must be able to say so") — checked first, and
// answered handled=false so a later module, in practice file.Extract,
// still produces a labelled entry for it. This is the same
// assignments map file.Extract already reads to label Lang; no new
// coupling to scanner.Pattern is introduced to get it.
func Extract(f scanner.File, project string, assignments map[string]string) (entries []entry.Entry, handled bool, err error) {
	if assignments[path.Ext(f.Path)] != "" {
		return nil, false, nil
	}
	if excluded(f.Path) {
		return nil, true, nil
	}
	return nil, false, nil
}

func excluded(p string) bool {
	base := path.Base(p)
	ext := path.Ext(p)
	if ext == "" {
		for _, n := range knownExtensionless {
			if base == n {
				return false
			}
		}
		return true // shape of a compiled binary; defaulting to include is the dangerous direction
	}
	for _, d := range deniedExtensions {
		if ext == d {
			return true
		}
	}
	return false
}
