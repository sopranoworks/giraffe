// Package entry defines the one abstraction giraffe is built around: the
// type that represents a single index entry (specifications/giraffe.md
// §18). Every language module's contract is "produce values of this type";
// the type's fields, the attributes printed in a result line, and the
// attribute names --query accepts are one vocabulary with one definition.
package entry

import (
	"path"
	"strconv"
	"strings"
)

// Entry is one fact in the index: one declaration, addressable by its
// attributes.
//
// Path, Name, Kind and Project are the four common attributes (§6): always
// present, for every language. Position holds the language-specific
// position — module, crate, namespace, package — as named pairs rather
// than fixed fields, so adding a language never changes this type.
type Entry struct {
	Path    string // repo-relative filesystem path
	Name    string // symbol name
	Kind    string // declaration kind, in the language's own vocabulary
	Project string // repository / project root this entry belongs to
	Doc     string // first line of the declaration's documentation; "" if undocumented

	// Ext is the file extension, without the leading dot ("go", "h",
	// "md"). A fact about every file, requiring no judgement (§25
	// C-24) — always present, unlike Lang below.
	Ext string

	// Lang is the language's own name ("go", "c++", "python", unmapped
	// per §6 — the same treatment kind and scope already get), present
	// only when it is known: a registered extractor's own name, or an
	// assignment the operator wrote (index.LoadExtensions — its own
	// file since directive 023 item 2, .giraffe/exclude before it, §25
	// C-26).
	// "" when neither applies — not a placeholder value. §24 shipped
	// "none" as a placeholder; §25 C-24 withdrew it, since it delivered
	// neither visibility nor a narrowing axis. Absence is itself
	// information, the convention §6 already uses for a missing
	// language position.
	Lang string

	// Scope is displayed as-is; its values are the language's own
	// vocabulary ("exported"/"package" for Go, something else for a
	// later language) and are never enumerated or compared against here
	// (§21 C-12 — the same reason kind's values stay native, §6). The
	// language module decides the word; this package only carries it.
	Scope string

	// Exported reports whether the declaration is referenceable from
	// outside its own module — the boolean judgement §21 C-12 requires
	// in place of matching Scope against a fixed value. query.Search
	// uses it, not Scope, to decide cross-project visibility and
	// ranking, so a second language's different visibility vocabulary
	// (Java's four levels, Rust's four) never has to be shoehorned into
	// two strings here.
	Exported bool

	Position []Position // language position; nil for files with no language structure

	// Size is the file's byte size on disk, and DeclCount the number of
	// declarations extracted from it (0 when no language module found
	// any). Both are facts about the *file*, not the declaration, so
	// they are only ever meaningful on a Kind==KindFile entry — a
	// caller deciding whether to open a file directly or narrow it
	// first (with --query, or with grep) wants exactly this (§27
	// directive 009 item 3). Size prints on every file result
	// (directive 043); DeclCount only when it discriminates (§25 C-25).
	// Left zero-valued on declaration entries; nothing reads them there.
	Size      int64
	DeclCount int

	// Line is the 1-based source line the declaration's range starts
	// at: its doc comment's first line when it has one directly above
	// it, else the declaration's own line (directive 043; 042 had the
	// range start at the declaration). LineText is the declaration's
	// own line's text, trimmed of surrounding whitespace
	// (specifications/giraffe.md §31.4) — the signature, never the doc,
	// so it may sit a few lines below Line: a caller can discard a
	// candidate from these facts without opening the file, the same way
	// `struct X {` and `v := X.New` answer the same name search
	// differently. Zero/empty on a Kind==KindFile entry, which addresses
	// no specific line — absence is the correct encoding (§25 C-24),
	// never a placeholder.
	Line     int
	LineText string

	// EndLine is the 1-based source line the declaration ends on,
	// inclusive (directive 042): the closing brace of a func or a
	// struct, the last line of a multi-line const or var, Line itself
	// for an undocumented declaration that fits on one line. With Line
	// it is the declaration's whole extent, doc included, absolute
	// within Path — a member's range lies inside its owner's but is not
	// expressed relative to it — so a caller hands the two numbers to
	// `sed -n 'A,Bp'` with no arithmetic. §31.4 asked for the line and
	// its text and dropped the end; 041 measured whole-file reads of
	// files giraffe had just named as the largest cost of every aided
	// run, because a start line alone cannot aim sed. Zero, like Line,
	// on a file entry.
	EndLine int

	// Callers is where this declaration's name is called from (directive
	// 050): every Call in the index whose Name is this entry's, ordered
	// by query for the reader, closest evidence first. A fact about the
	// name, not the declaration — matching is by name alone, never by
	// resolved type, so a same-named method on another receiver is
	// listed too, at the same false-positive cost ## source already
	// carries for an exact name; each line is a place to open, not a
	// claim about what calls what. Not part of the entry's own stored
	// shape: a call in file A is a fact about a name declared in B, held
	// by the index under the name and attached at query time, so a
	// change to A never rewrites B's entries (§31.2). Nil when nothing
	// calls the name, or for a file entry — absence, as everywhere here.
	Callers []Call
}

// Call is one call site: the place in Path, at Line, where Name is
// called (directive 050). Via is the identifier the call is qualified
// by, when it has one — the last name before the dot in c.metrics.
// Snapshot(...) is "metrics", in ms.provider.Metrics().Snapshot(...)
// it is "Metrics", in a bare Snapshot(...) it is "" — the one fact a
// call site carries about what it calls, recorded so a reader shown a
// bounded list gets the sites that name the declaration's own type
// first (query.orderCallers). A language module produces these
// alongside its entries, one per (Name, Line) in a file; the index
// holds them under the name.
type Call struct {
	// Path is the file the call is in, repo-relative.
	Path string
	// Name is the name called.
	Name string
	// Line is the 1-based line the call's opening parenthesis is on.
	Line int
	// Via is the qualifier the call hangs off; "" for a bare call.
	Via string
}

// Place is the site as a result line prints it and --query callers:
// matches it: path:line, paste-ready (§7).
func (c Call) Place() string {
	return c.Path + ":" + strconv.Itoa(c.Line)
}

// LineRange is the one value the line attribute carries — printed on a
// result line as line:<LineRange>, and what --query line:<value>
// compares against — so what a caller copies from an answer is what
// the filter matches (§7's paste-ready rule, kept over a value that
// is no longer a single number). "17-62" for a declaration spanning
// lines 17 to 62 (doc included), "19" for an undocumented one that
// fits on its line, "" for an entry that addresses no line at all (a
// file entry).
func (e Entry) LineRange() string {
	switch {
	case e.Line <= 0:
		return ""
	case e.EndLine > e.Line:
		return strconv.Itoa(e.Line) + "-" + strconv.Itoa(e.EndLine)
	default:
		return strconv.Itoa(e.Line)
	}
}

// Position is one named language-position pair, e.g. {"module",
// "internal/retry"} for Go. Absence of a language position is information
// (§6) and is represented by a nil Position slice, never a placeholder
// value.
type Position struct {
	Name  string
	Value string
}

// FileEntry returns filePath's own entry (§22 item 4): a file is a
// search target addressed by its own name, not by any declaration
// inside it, so every language module that handles a file needs this
// same construction — the golang module for the file it parses, and
// the generic module for a file no other module claims (directive 016
// §1: "the generic module ... is not a fallback mechanism, not a
// special case ... a language module like any other"). lang is the
// value this file's own Lang attribute should carry ("" for none —
// absence, not a placeholder, §25 C-24). Size and DeclCount are left
// zero; index.Refresh fills them in from facts only it has (the
// freshness stat already taken this call, and the count of whatever
// declarations were extracted alongside this entry).
func FileEntry(filePath, project, lang string) Entry {
	base := path.Base(filePath)
	ext := path.Ext(base)
	return Entry{
		Path:     filePath,
		Name:     strings.TrimSuffix(base, ext),
		Kind:     KindFile,
		Project:  project,
		Ext:      strings.TrimPrefix(ext, "."),
		Lang:     lang,
		Exported: true,
	}
}

// KindFile is the Kind value for an entry representing a file itself,
// not a declaration inside it (§22 item 4: a file name resists naming
// drift better than a symbol name, and is a search target of its own).
// A common constant, not a language-specific one like Scope's values
// (§21 C-12): every language module's files can produce one the same
// way, via the scanner's own file list, not per-language judgement.
const KindFile = "file"
