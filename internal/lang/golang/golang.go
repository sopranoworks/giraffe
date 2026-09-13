// Package golang extracts index entries from Go source using go/parser:
// standard library, pure Go, no type checking (specifications/giraffe.md
// §10). It reads only what a file's syntax gives it — exported top-level
// declarations, their kind, and the first line of the preceding doc
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
// const, and var (§10), both exported and unexported (§21 C-6) — each
// carries Scope and Exported accordingly. Grouped declarations — type
// (...), const (...) — are expanded to one entry per spec, matching what
// the sizing prototype in §11 undercounted. Methods are out of scope for
// this version and are skipped, exported or not.
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
			if d.Recv != nil { // methods are out of scope
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
			}
		}
	}
	return entries, nil
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
