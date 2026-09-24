// Package golang extracts index entries from Go source using go/parser:
// standard library, pure Go, no type checking (specifications/giraffe.md
// §10). It reads only what a file's syntax gives it — top-level
// declarations (exported and unexported), their methods and struct
// fields, each one's kind, and the first line of its preceding doc
// comment — and never touches the filesystem itself (§18); its input is a
// scanner.File it is handed.
package golang

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"strings"

	"giraffe/internal/entry"
	"giraffe/internal/scanner"
)

// Extensions lists the file extensions this module wants from the
// scanner, and is also how it recognises a file as its own: the only
// place ".go" is named (specifications/giraffe.md §18; directive 016
// §1 — no extension table outside a language module).
var Extensions = []string{".go"}

// langName is this module's own value for a produced entry's Lang
// attribute (§6, unmapped) — stamped by Extract itself, not looked up
// from outside (directive 016 §1: "the module reports its own name").
const langName = "go"

// Marker is this module's own root-marker basename — the file whose
// presence in a directory means that directory is Go's own (directive
// 017 §3: "recognising go.mod is Go's knowledge"). Used two ways, from
// one name: giraffe/internal/lang moves this module to the front of
// the chain for files under a directory that has one, and
// giraffe/internal/scanner's root discovery recognises it via
// lang.RegisteredMarkers — one authority over the name, not a second
// copy of it in the scanner.
const Marker = "go.mod"

// scopeExported and scopePackage are Go's own words for a declaration's
// visibility (§21 C-12: the displayed value is the language's own
// vocabulary, kept local to the language module — never a common,
// enumerated value; see entry.Entry.Scope).
const (
	scopeExported = "exported"
	scopePackage  = "package"
)

// Extract reports whether f is a Go file (its own extension check,
// §18) and, if so, returns f's own entry (entry.FileEntry, §22 item 4)
// followed by one entry per top-level declaration: func, type, const,
// var, method, and field (§10, §28 C-38), both exported and unexported
// (§21 C-6) — each carries Scope and Exported accordingly. Grouped
// declarations — type (...), const (...) — are expanded to one entry
// per spec, matching what the sizing prototype in §11 undercounted.
//
// handled is false, regardless of err, whenever this module is not the
// right answer for f — a wrong extension, (§18; directive 016 §1) an
// extension that matched but content that did not parse, or (directive
// 017 §1) content that could not even be read. The caller tries the
// next registered module either way; err, when non-nil, is a
// diagnostic worth reporting even though the file was
// not left unhandled.
//
// A method's Name is its own bare name, never qualified with its
// receiver's type ("Snapshot", not "Cluster.Snapshot"); the receiver
// type is carried as a second Position pair instead. Likewise a field's
// Name is its own bare name, with its containing type as a second
// Position pair. Two attributes, not one, precisely to avoid the
// ranking risk §28 C-38 named: a qualified name would make every method
// or field of a type substring-match a search for the type's own name,
// burying the type declaration itself under its own members. A plain
// method or field name only collides with a search word the way any
// other declaration's name already can.
func Extract(f scanner.File, project string, assignments map[string]string) (entries []entry.Entry, handled bool, err error) {
	if !handles(f.Path) {
		return nil, false, nil
	}

	content, err := f.Content()
	if err != nil {
		return nil, false, err
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, f.Path, content, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, false, fmt.Errorf("parse %s: %w", f.Path, err)
	}
	lines := strings.Split(string(content), "\n")

	position := []entry.Position{{Name: "module", Value: path.Dir(f.Path)}}

	decls := []entry.Entry{entry.FileEntry(f.Path, project, langName)}
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Recv != nil {
				if e, ok := methodEntry(f.Path, project, d, position, fset, lines); ok {
					decls = append(decls, e)
				}
				continue
			}
			exported := d.Name.IsExported()
			line, end, text := lineRange(fset, d.Doc, d, lines)
			decls = append(decls, entry.Entry{
				Path:     f.Path,
				Name:     d.Name.Name,
				Kind:     "func",
				Project:  project,
				Scope:    scopeOf(exported),
				Exported: exported,
				Doc:      firstLine(d.Doc),
				DocText:  docText(d.Doc),
				Position: position,
				Line:     line,
				LineText: text,
				EndLine:  end,
			})

		case *ast.GenDecl:
			kind := genDeclKind(d.Tok)
			if kind == "" {
				continue // import, or another declaration kind this version does not index
			}
			for _, spec := range d.Specs {
				specName, ownDoc, ok := specNameAndDoc(spec)
				if !ok {
					continue
				}
				exported := ast.IsExported(specName)
				line, end, text := lineRange(fset, adjacentDoc(ownDoc, d), spec, lines)
				decls = append(decls, entry.Entry{
					Path:     f.Path,
					Name:     specName,
					Kind:     kind,
					Project:  project,
					Scope:    scopeOf(exported),
					Exported: exported,
					Doc:      firstLine(preferDoc(ownDoc, d.Doc)),
					DocText:  docText(preferDoc(ownDoc, d.Doc)),
					Position: position,
					Line:     line,
					LineText: text,
					EndLine:  end,
				})
				if kind == "type" {
					if ts, ok := spec.(*ast.TypeSpec); ok {
						decls = append(decls, fieldEntries(f.Path, project, ts, position, fset, lines)...)
					}
				}
			}
		}
	}
	// decls[0] is the file's own entry (entry.FileEntry already stamped
	// its Ext and Lang); every declaration after it needs the same two
	// facts, which no single-entry literal above sets, since one module
	// produces every entry here under the one extension and the one
	// module name.
	for i := 1; i < len(decls); i++ {
		decls[i].Ext = "go"
		decls[i].Lang = langName
	}
	return decls, true, nil
}

// handles reports whether path is one this module claims, by
// extension — checked here, inside the module, and nowhere else
// (§18).
func handles(p string) bool {
	for _, ext := range Extensions {
		if strings.HasSuffix(p, ext) {
			return true
		}
	}
	return false
}

// lineRange renders a declaration's extent as 1-based line numbers —
// the line it starts on and the line it ends on, inclusive — plus the
// text of the declaration's own first line, trimmed of surrounding
// whitespace (§31.4; directive 042 for the end, 043 for the doc). The
// extent runs from doc's first line, when doc is non-nil, else from
// node's Pos, through node's End: a func's doc and signature through
// its closing brace, a type's the whole declaration, a spec's the spec
// alone (never the group it sits in), a field's its own line(s). The
// doc comment is inside the range (043) because a caller who wants the
// whole comment otherwise has to widen the start by a number it cannot
// know; 042 had left it out, and the doc's first line appearing twice —
// on the result line and at the top of what sed returns — is the
// cheaper of the two. text is always node's own first line, never the
// doc's: it is the fact that lets a caller discard a candidate
// ("struct X {" vs "v := X.New") without opening the file. Out-of-range
// is defensive only: fset.Position always resolves a valid ast.Node's
// own Pos within lines already split from the same content that
// produced file.
func lineRange(fset *token.FileSet, doc *ast.CommentGroup, node ast.Node, lines []string) (line, end int, text string) {
	start := node.Pos()
	if doc != nil {
		start = doc.Pos()
	}
	line = fset.Position(start).Line
	end = fset.Position(node.End()).Line
	own := fset.Position(node.Pos()).Line
	if own < 1 || own > len(lines) {
		return line, end, ""
	}
	return line, end, strings.TrimSpace(lines[own-1])
}

// methodEntry returns the index entry for a method declaration, or
// ok=false if its receiver's type cannot be determined (a shape the Go
// spec does not actually allow, but the extractor skips rather than
// guesses — the same defensiveness specNameDocAndPos already applies to
// a spec it does not recognise).
func methodEntry(path, project string, d *ast.FuncDecl, base []entry.Position, fset *token.FileSet, lines []string) (entry.Entry, bool) {
	recv := bareTypeName(d.Recv.List[0].Type)
	if recv == "" {
		return entry.Entry{}, false
	}
	exported := d.Name.IsExported()
	line, end, text := lineRange(fset, d.Doc, d, lines)
	return entry.Entry{
		Path:     path,
		Name:     d.Name.Name,
		Kind:     "method",
		Project:  project,
		Scope:    scopeOf(exported),
		Exported: exported,
		Doc:      firstLine(d.Doc),
		DocText:  docText(d.Doc),
		Position: withPosition(base, "receiver", recv),
		Line:     line,
		LineText: text,
		EndLine:  end,
	}, true
}

// fieldEntries returns one entry per field declared directly on ts, when
// ts is a struct type — nil otherwise. An embedded field (no name of its
// own) is addressed, and named here, by its embedded type's own name,
// the same identifier Go resolves it by; a blank identifier field is
// skipped, since "_" is never addressable.
func fieldEntries(path, project string, ts *ast.TypeSpec, base []entry.Position, fset *token.FileSet, lines []string) []entry.Entry {
	st, ok := ts.Type.(*ast.StructType)
	if !ok || st.Fields == nil {
		return nil
	}
	position := withPosition(base, "type", ts.Name.Name)

	var entries []entry.Entry
	for _, field := range st.Fields.List {
		names := field.Names
		if len(names) == 0 { // embedded: named after the embedded type
			if n := bareTypeName(field.Type); n != "" {
				names = []*ast.Ident{{Name: n}}
			}
		}
		line, end, text := lineRange(fset, field.Doc, field, lines)
		for _, n := range names {
			if n.Name == "_" {
				continue
			}
			exported := ast.IsExported(n.Name)
			entries = append(entries, entry.Entry{
				Path:     path,
				Name:     n.Name,
				Kind:     "field",
				Project:  project,
				Scope:    scopeOf(exported),
				Exported: exported,
				Doc:      firstLine(field.Doc),
				DocText:  docText(field.Doc),
				Position: position,
				Line:     line,
				LineText: text,
				EndLine:  end,
			})
		}
	}
	return entries
}

// withPosition copies base and appends one more pair — never mutates
// base's backing array, since base (the file's shared module position)
// is reused across every declaration in the file.
func withPosition(base []entry.Position, name, value string) []entry.Position {
	out := make([]entry.Position, len(base), len(base)+1)
	copy(out, base)
	return append(out, entry.Position{Name: name, Value: value})
}

// bareTypeName renders a type expression's own local name: a pointer
// star, one level of generic type-argument instantiation, and a package
// qualifier are all stripped, leaving the identifier a caller writes to
// name the type itself — "*Cluster", "Cluster[T]", and "pkg.Cluster" all
// resolve to "Cluster". Returns "" for a shape neither a receiver nor an
// embedded field can actually have (an anonymous struct or interface,
// or a raw literal); callers treat that as "skip", not "guess".
func bareTypeName(expr ast.Expr) string {
	for {
		switch t := expr.(type) {
		case *ast.StarExpr:
			expr = t.X
		case *ast.IndexExpr:
			expr = t.X
		case *ast.IndexListExpr:
			expr = t.X
		case *ast.SelectorExpr:
			return t.Sel.Name
		case *ast.Ident:
			return t.Name
		default:
			return ""
		}
	}
}

func scopeOf(exported bool) string {
	if exported {
		return scopeExported
	}
	return scopePackage
}

func genDeclKind(tok token.Token) string {
	switch tok {
	case token.TYPE:
		return "type"
	case token.CONST:
		return "const"
	case token.VAR:
		return "var"
	default:
		return "" // token.IMPORT
	}
}

// specNameAndDoc returns the declared name and the spec's own doc
// comment for one spec inside a (possibly grouped) declaration — nil
// when the spec has none, which is always the case for an ungrouped
// spec, whose comment go/parser hangs on the GenDecl instead. The
// caller resolves that against the group's comment twice over, and
// differently: preferDoc for the text a result line shows, adjacentDoc
// for the range. The spec's position is likewise the caller's to take
// from the spec itself (lineRange), and is always the spec's own, never
// the group's: in `const (\n A = 1\n B = 2\n)` each spec's own line is
// what a caller discarding a candidate needs (§31.4), not the line the
// group starts on.
func specNameAndDoc(spec ast.Spec) (name string, doc *ast.CommentGroup, ok bool) {
	switch s := spec.(type) {
	case *ast.TypeSpec:
		return s.Name.Name, s.Doc, true
	case *ast.ValueSpec:
		if len(s.Names) == 0 {
			return "", nil, false
		}
		// const/var can declare several names on one line (A, B = 1, 2);
		// only the first is reported for this version, matching the
		// declaration's own doc comment.
		return s.Names[0].Name, s.Doc, true
	default:
		return "", nil, false
	}
}

// adjacentDoc returns the comment that sits directly above one spec of
// d, for the spec's range (directive 043): the keyword's own comment
// when d is a single ungrouped declaration (`// Config holds…` above
// `type Config struct` is on the GenDecl, and the spec's own Doc is
// nil), else the spec's own, inside a parenthesised group. A group's
// comment sits above the keyword, with other specs possibly between it
// and this one, so it never widens a grouped spec's range — even though
// preferDoc still lends its text to a grouped spec that has none of its
// own.
func adjacentDoc(own *ast.CommentGroup, d *ast.GenDecl) *ast.CommentGroup {
	if !d.Lparen.IsValid() {
		return d.Doc
	}
	return own
}

// preferDoc picks the comment a result line shows for a spec: its own,
// else the group's — a `// Errors the store returns.` above `var (`
// describes each sentinel inside it well enough to be worth printing,
// though it is not adjacent to any of them (see adjacentDoc).
func preferDoc(specDoc, groupDoc *ast.CommentGroup) *ast.CommentGroup {
	if specDoc != nil {
		return specDoc
	}
	return groupDoc
}

// docText is doc's whole text, trimmed (directive 065): what
// entry.DocText holds.
func docText(doc *ast.CommentGroup) string {
	if doc == nil {
		return ""
	}
	return strings.TrimSpace(doc.Text())
}

func firstLine(doc *ast.CommentGroup) string {
	if doc == nil {
		return ""
	}
	text := doc.Text()
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = text[:i]
	}
	return strings.TrimSpace(text)
}
