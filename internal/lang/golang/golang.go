// Package golang extracts index entries from Go source using go/parser:
// standard library, pure Go, no type checking (specifications/giraffe.md
// §10). It reads only what a file's syntax gives it — top-level
// declarations (exported and unexported), their methods and struct
// fields, each one's kind, and the first line of its preceding doc
// comment — plus, since directive 050, every call expression's called
// name and line (see callSites) — and never touches the filesystem
// itself (§18); its input is a scanner.File it is handed.
package golang

import (
	"fmt"
	"go/ast"
	"go/doc"
	"go/parser"
	"go/token"
	"path"
	"sort"
	"strings"

	"github.com/sopranoworks/giraffe/internal/entry"
	"github.com/sopranoworks/giraffe/internal/scanner"
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
//
// calls is every call site in the file (directive 050; callSites): the
// same syntax walk, with no type checking — the name is the identifier
// called, so a method and a function of one name are one name here,
// which is the cost §28 C-39's reference search was always going to
// pay without go/types, and the reason a Call carries Via.
//
// A "_test.go" file is handled exactly like any other Go file (directive
// 051 §1 — the scanner no longer excludes it, §28 C-39's gap): its
// declarations and call sites are extracted the same way, each entry
// carrying Test true (isTestFile).
func Extract(f scanner.File, project string, assignments map[string]string) (entries []entry.Entry, calls []entry.Call, handled bool, err error) {
	if !handles(f.Path) {
		return nil, nil, false, nil
	}

	content, err := f.Content()
	if err != nil {
		return nil, nil, false, err
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, f.Path, content, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, nil, false, fmt.Errorf("parse %s: %w", f.Path, err)
	}
	lines := strings.Split(string(content), "\n")
	test := isTestFile(f.Path)

	position := []entry.Position{{Name: "module", Value: path.Dir(f.Path)}}

	decls := []entry.Entry{entry.FileEntry(f.Path, project, langName)}
	decls[0].Test = test
	// A file's own doc is its package clause's (directive 073): what the
	// package is for, said once by its author.
	decls[0].Doc = firstLine(file.Doc)
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
		decls[i].Test = test
	}
	return decls, callSites(f.Path, file, fset, test, lines), true, nil
}

// callSites is every call expression in file, one Call per distinct
// (name, line) (directive 050): the name called — an identifier, or
// the selected name of a selector, through parentheses, a generic
// instantiation's brackets and a call's own result — and, for a
// selector, the qualifier it hangs off (calledName), plus the line's
// own text (directive 051 §1 — Text — the same fact lineRange already
// takes from a declaration's own first line). A call of a
// predeclared identifier (len, append, string(x), panic) is not a call
// site of anything the index holds and is not recorded; a call of a
// literal or an anonymous func has no name to record. Sorted by line
// then name, so the same file always yields the same list (§3 C5).
func callSites(filePath string, file *ast.File, fset *token.FileSet, test bool, lines []string) []entry.Call {
	type site struct {
		name string
		line int
	}
	seen := make(map[site]bool)
	// The declaration each call is inside (Call.In): every FuncDecl's
	// range and its name, a method as Type.Name (directive 065, g48).
	type span struct {
		name       string
		from, upto int
	}
	var spans []span
	for _, d := range file.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		name := fd.Name.Name
		if fd.Recv != nil && len(fd.Recv.List) > 0 {
			if r := receiverName(fd.Recv.List[0].Type); r != "" {
				name = r + "." + name
			}
		}
		spans = append(spans, span{name, fset.Position(fd.Pos()).Line, fset.Position(fd.End()).Line})
	}
	enclosing := func(line int) string {
		for _, s := range spans {
			if line >= s.from && line <= s.upto {
				return s.name
			}
		}
		return ""
	}
	var calls []entry.Call
	// A selector that is what a call calls is the call's site, not a
	// use of its own (directive 072): Inspect reaches the call before
	// its Fun, so the selector is known as called by the time it is
	// visited.
	called := make(map[*ast.SelectorExpr]bool)
	ast.Inspect(file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			if sel := calledSelector(x.Fun); sel != nil {
				called[sel] = true
			}
			name, via := calledName(x.Fun)
			if name == "" || doc.IsPredeclared(name) {
				return true
			}
			at := site{name, fset.Position(x.Lparen).Line}
			if seen[at] {
				return true // the line is the place; a second call of the name on it adds nothing
			}
			seen[at] = true
			calls = append(calls, entry.Call{Path: filePath, Name: name, Line: at.line, Via: via, Test: test, Text: lineText(lines, at.line), In: enclosing(at.line)})
		case *ast.SelectorExpr:
			// A selected name that is not called — o.Prefix read, set
			// or its address taken, a package's exported name referred
			// to — is a use of that name (directive 072): what a caller
			// grepped a file for after a page had shown it the field.
			if called[x] {
				return true
			}
			name := x.Sel.Name
			at := site{name, fset.Position(x.Sel.Pos()).Line}
			if seen[at] {
				return true
			}
			seen[at] = true
			calls = append(calls, entry.Call{Path: filePath, Name: name, Line: at.line, Via: lastIdent(x.X), Test: test, Text: lineText(lines, at.line), In: enclosing(at.line), Use: true})
		}
		return true
	})
	sort.Slice(calls, func(i, j int) bool {
		if calls[i].Line != calls[j].Line {
			return calls[i].Line < calls[j].Line
		}
		return calls[i].Name < calls[j].Name
	})
	return calls
}

// lineText returns line's own text (1-based), trimmed of surrounding
// whitespace, or "" when line falls outside lines — defensive only, the
// same guard lineRange applies to a declaration's own line: fset always
// resolves a parsed file's own node within lines split from that same
// content.
func lineText(lines []string, line int) string {
	if line < 1 || line > len(lines) {
		return ""
	}
	return strings.TrimSpace(lines[line-1])
}

// calledSelector is the selector a call's Fun comes down to through
// parentheses, instantiation brackets and a pointer star — the same
// descent as calledName — or nil when the call is not of a selected
// name.
func calledSelector(fun ast.Expr) *ast.SelectorExpr {
	for {
		switch f := fun.(type) {
		case *ast.SelectorExpr:
			return f
		case *ast.ParenExpr:
			fun = f.X
		case *ast.IndexExpr:
			fun = f.X
		case *ast.IndexListExpr:
			fun = f.X
		case *ast.StarExpr:
			fun = f.X
		default:
			return nil
		}
	}
}

// calledName resolves what a call expression's Fun names: the
// identifier itself for a bare call, the selected name for x.Name(...)
// — with via the last identifier of x (an identifier's own name, a
// selector's selected name, a call's own called name), or "" when x
// has none — and "" for anything without a name to call (a func
// literal, an indexed element). Parentheses, generic instantiation
// (F[int](x), Name[T]) and a pointer star are looked through, the way
// bareTypeName looks through them for a type.
func calledName(fun ast.Expr) (name, via string) {
	for {
		switch f := fun.(type) {
		case *ast.Ident:
			return f.Name, ""
		case *ast.SelectorExpr:
			return f.Sel.Name, lastIdent(f.X)
		case *ast.ParenExpr:
			fun = f.X
		case *ast.IndexExpr:
			fun = f.X
		case *ast.IndexListExpr:
			fun = f.X
		case *ast.StarExpr:
			fun = f.X
		default:
			return "", ""
		}
	}
}

// lastIdent is the rightmost identifier of a qualifier expression —
// "metrics" for c.metrics, "Metrics" for ms.provider.Metrics(), "m"
// for m — or "" when the expression ends in something that is not a
// name (a literal, an index, a func literal). The same descent as
// calledName, since a call's result is qualified by what was called.
func lastIdent(x ast.Expr) string {
	for {
		switch e := x.(type) {
		case *ast.Ident:
			return e.Name
		case *ast.SelectorExpr:
			return e.Sel.Name
		case *ast.CallExpr:
			x = e.Fun
		case *ast.ParenExpr:
			x = e.X
		case *ast.IndexExpr:
			x = e.X
		case *ast.IndexListExpr:
			x = e.X
		case *ast.StarExpr:
			x = e.X
		case *ast.TypeAssertExpr:
			x = e.X
		case *ast.UnaryExpr:
			x = e.X
		default:
			return ""
		}
	}
}

// isTestFile reports whether p is a Go test file by its own name — the
// one fact "_test.go" carries (directive 065, g42), the convention the
// scanner used to exclude it by (internal/scanner/exclude.go's former
// defaultExclude entry) and now leaves for this module to record
// instead of hide.
func isTestFile(p string) bool {
	return strings.HasSuffix(p, "_test.go")
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
				DocText:  strings.TrimSpace(docText(field.Doc) + "\n" + docText(field.Comment)),
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

// receiverName is the type name a method's receiver names, through a
// pointer and a generic instantiation: *Cluster -> Cluster, Box[T] -> Box.
func receiverName(t ast.Expr) string {
	switch x := t.(type) {
	case *ast.StarExpr:
		return receiverName(x.X)
	case *ast.Ident:
		return x.Name
	case *ast.IndexExpr:
		return receiverName(x.X)
	case *ast.IndexListExpr:
		return receiverName(x.X)
	}
	return ""
}
