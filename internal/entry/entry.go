// Package entry defines the one abstraction giraffe is built around: the
// type that represents a single index entry (specifications/giraffe.md
// §18). Every language module's contract is "produce values of this type";
// the type's fields, the attributes printed in a result line, and the
// attribute names --query accepts are one vocabulary with one definition.
package entry

import (
	"path"
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
	// directive 009 item 3, giraffe.md §25 C-25: printed only when it
	// discriminates, never on every line). Left zero-valued on
	// declaration entries; nothing reads them there.
	Size      int64
	DeclCount int

	// Line is the 1-based source line the declaration starts at, and
	// LineText is that line's own text, trimmed of surrounding
	// whitespace (specifications/giraffe.md §31.4): a caller can
	// discard a candidate from these two facts without opening the
	// file, the same way `struct X {` and `v := X.New` answer the same
	// name search differently. Zero/empty on a Kind==KindFile entry,
	// which addresses no specific line — absence is the correct
	// encoding (§25 C-24), never a placeholder.
	Line     int
	LineText string
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
