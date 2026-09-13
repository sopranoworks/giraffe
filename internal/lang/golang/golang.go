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
// scanner.
var Extensions = []string{".go"}

// scopeExported and scopePackage are Go's own words for a declaration's
// visibility (§21 C-12: the displayed value is the language's own
// vocabulary, kept local to the language module — never a common,
// enumerated value; see entry.Entry.Scope).
const (
	scopeExported = "exported"
	scopePackage  = "package"
)

// Extract returns one entry per top-level declaration in f: func, type,
// const, var, method, and field (§10, §28 C-38), both exported and
// unexported (§21 C-6) — each carries Scope and Exported accordingly.
// Grouped declarations — type (...), const (...) — are expanded to one
// entry per spec, matching what the sizing prototype in §11 undercounted.
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
func Extract(f scanner.File, project string) ([]entry.Entry, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, f.Path, f.Content, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", f.Path, err)
	}

	position := []entry.Position{{Name: "module", Value: path.Dir(f.Path)}}

	var entries []entry.Entry
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Recv != nil {
				if e, ok := methodEntry(f.Path, project, d, position); ok {
					entries = append(entries, e)
				}
				continue
			}
			exported := d.Name.IsExported()
			entries = append(entries, entry.Entry{
				Path:     f.Path,
				Name:     d.Name.Name,
				Kind:     "func",
				Project:  project,
				Scope:    scopeOf(exported),
				Exported: exported,
				Doc:      firstLine(d.Doc),
				Position: position,
			})

		case *ast.GenDecl:
			kind := genDeclKind(d.Tok)
			if kind == "" {
				continue // import, or another declaration kind this version does not index
			}
			for _, spec := range d.Specs {
				name, doc, ok := specNameAndDoc(spec, d.Doc)
				if !ok {
					continue
				}
				exported := ast.IsExported(name)
				entries = append(entries, entry.Entry{
					Path:     f.Path,
					Name:     name,
					Kind:     kind,
					Project:  project,
					Scope:    scopeOf(exported),
					Exported: exported,
					Doc:      doc,
					Position: position,
				})
				if kind == "type" {
					if ts, ok := spec.(*ast.TypeSpec); ok {
						entries = append(entries, fieldEntries(f.Path, project, ts, position)...)
					}
				}
			}
		}
	}
	return entries, nil
}

// methodEntry returns the index entry for a method declaration, or
// ok=false if its receiver's type cannot be determined (a shape the Go
// spec does not actually allow, but the extractor skips rather than
// guesses — the same defensiveness specNameAndDoc already applies to a
// spec it does not recognise).
func methodEntry(path, project string, d *ast.FuncDecl, base []entry.Position) (entry.Entry, bool) {
	recv := bareTypeName(d.Recv.List[0].Type)
	if recv == "" {
		return entry.Entry{}, false
	}
	exported := d.Name.IsExported()
	return entry.Entry{
		Path:     path,
		Name:     d.Name.Name,
		Kind:     "method",
		Project:  project,
		Scope:    scopeOf(exported),
		Exported: exported,
		Doc:      firstLine(d.Doc),
		Position: withPosition(base, "receiver", recv),
	}, true
}

// fieldEntries returns one entry per field declared directly on ts, when
// ts is a struct type — nil otherwise. An embedded field (no name of its
// own) is addressed, and named here, by its embedded type's own name,
// the same identifier Go resolves it by; a blank identifier field is
// skipped, since "_" is never addressable.
func fieldEntries(path, project string, ts *ast.TypeSpec, base []entry.Position) []entry.Entry {
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
				Position: position,
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

// specNameAndDoc returns the declared name and doc comment for one spec
// inside a (possibly grouped) declaration. A spec's own Doc — present when
// each item in a group has its own comment — takes precedence; groupDoc is
// the comment above the declaration keyword itself, used as a fallback.
func specNameAndDoc(spec ast.Spec, groupDoc *ast.CommentGroup) (name, doc string, ok bool) {
	switch s := spec.(type) {
	case *ast.TypeSpec:
		return s.Name.Name, firstLine(preferDoc(s.Doc, groupDoc)), true
	case *ast.ValueSpec:
		if len(s.Names) == 0 {
			return "", "", false
		}
		// const/var can declare several names on one line (A, B = 1, 2);
		// only the first is reported for this version, matching the
		// declaration's own doc comment.
		return s.Names[0].Name, firstLine(preferDoc(s.Doc, groupDoc)), true
	default:
		return "", "", false
	}
}

func preferDoc(specDoc, groupDoc *ast.CommentGroup) *ast.CommentGroup {
	if specDoc != nil {
		return specDoc
	}
	return groupDoc
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
