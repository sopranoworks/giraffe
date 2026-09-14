package golang

import (
	"testing"

	"giraffe/internal/entry"
	"giraffe/internal/scanner"
)

const src = `// Package sample is a fixture for the extractor's own tests.
package sample

import "sync"

// Retry retries with backoff.
func Retry() error { return nil }

func unexported() {}

// Config holds retry settings.
type Config struct {
	// Name is the config's own label.
	Name string
	max  int // unexported, no doc

	sync.Mutex // embedded, named after its own type

	_ int // blank: never addressable
}

// Do runs the configured retry.
func (c Config) Do() {}

func (c *Config) reset() {}

const (
	// Attempts is the retry ceiling.
	Attempts = 3
	backoff  = 2 // unexported, no doc
)

type (
	// Policy groups retry knobs.
	Policy struct{}
	hidden  struct{}
)

var Enabled = true
`

func TestExtract(t *testing.T) {
	f := scanner.File{Path: "internal/retry/retry.go", Content: []byte(src)}
	entries, handled, err := Extract(f, "giraffe", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !handled {
		t.Fatal("expected Extract to handle a .go file")
	}

	got := make(map[string]entry.Entry)
	for _, e := range entries {
		got[e.Name] = e
	}

	// The file's own entry (§22 item 4) always comes back alongside its
	// declarations now (directive 016 §1) — not computed separately by
	// index.Refresh.
	if fe, ok := got["retry"]; !ok || fe.Kind != entry.KindFile {
		t.Errorf("expected a file entry named %q, got %v", "retry", got["retry"])
	} else if fe.Lang != "go" || fe.Ext != "go" {
		t.Errorf("file entry Lang/Ext = %q/%q, want go/go", fe.Lang, fe.Ext)
	}

	want := map[string]struct {
		kind     string
		doc      string
		scope    string
		exported bool
	}{
		"Retry":      {"func", "Retry retries with backoff.", scopeExported, true},
		"unexported": {"func", "", scopePackage, false},
		"Config":     {"type", "Config holds retry settings.", scopeExported, true},
		"Attempts":   {"const", "Attempts is the retry ceiling.", scopeExported, true},
		"backoff":    {"const", "", scopePackage, false},
		"Policy":     {"type", "Policy groups retry knobs.", scopeExported, true},
		"hidden":     {"type", "", scopePackage, false},
		"Enabled":    {"var", "", scopeExported, true},
	}
	for name, w := range want {
		e, ok := got[name]
		if !ok {
			t.Errorf("expected %s in extracted entries, got %v", name, names(entries))
			continue
		}
		if e.Kind != w.kind {
			t.Errorf("%s: kind = %q, want %q", name, e.Kind, w.kind)
		}
		if e.Doc != w.doc {
			t.Errorf("%s: doc = %q, want %q", name, e.Doc, w.doc)
		}
		if e.Scope != w.scope {
			t.Errorf("%s: scope = %q, want %q", name, e.Scope, w.scope)
		}
		if e.Exported != w.exported {
			t.Errorf("%s: exported = %v, want %v", name, e.Exported, w.exported)
		}
		if e.Ext != "go" || e.Lang != "go" {
			t.Errorf("%s: Ext/Lang = %q/%q, want go/go", name, e.Ext, e.Lang)
		}
		if e.Line == 0 || e.LineText == "" {
			t.Errorf("%s: Line/LineText = %d/%q, want both set (§31.4)", name, e.Line, e.LineText)
		}
		if e.Path != "internal/retry/retry.go" {
			t.Errorf("%s: path = %q", name, e.Path)
		}
		if len(e.Position) != 1 || e.Position[0].Name != "module" || e.Position[0].Value != "internal/retry" {
			t.Errorf("%s: position = %v, want module:internal/retry", name, e.Position)
		}
	}

	// Methods and fields (§28 C-38) carry a second Position pair — their
	// receiver or containing type — after the shared module position;
	// their Name is their own bare name, never qualified by the type.
	wantSecond := map[string]struct {
		kind         string
		doc          string
		exported     bool
		positionName string
		positionVal  string
	}{
		"Do":    {"method", "Do runs the configured retry.", true, "receiver", "Config"},
		"reset": {"method", "", false, "receiver", "Config"},
		"Name":  {"field", "Name is the config's own label.", true, "type", "Config"},
		"max":   {"field", "", false, "type", "Config"},
		"Mutex": {"field", "", true, "type", "Config"}, // embedded, named after sync.Mutex
	}
	for name, w := range wantSecond {
		e, ok := got[name]
		if !ok {
			t.Errorf("expected %s in extracted entries, got %v", name, names(entries))
			continue
		}
		if e.Kind != w.kind {
			t.Errorf("%s: kind = %q, want %q", name, e.Kind, w.kind)
		}
		if e.Doc != w.doc {
			t.Errorf("%s: doc = %q, want %q", name, e.Doc, w.doc)
		}
		if e.Exported != w.exported {
			t.Errorf("%s: exported = %v, want %v", name, e.Exported, w.exported)
		}
		if len(e.Position) != 2 || e.Position[0].Name != "module" || e.Position[1].Name != w.positionName || e.Position[1].Value != w.positionVal {
			t.Errorf("%s: position = %v, want module:internal/retry then %s:%s", name, e.Position, w.positionName, w.positionVal)
		}
		if e.Line == 0 || e.LineText == "" {
			t.Errorf("%s: Line/LineText = %d/%q, want both set (§31.4)", name, e.Line, e.LineText)
		}
	}

	if _, present := got["_"]; present {
		t.Error("a blank field (_) should never be extracted: it is not addressable")
	}
}

// TestExtract_DeclinesAWrongExtension (directive 016 §1): this module
// checks its own extension, and no other file reaches it.
func TestExtract_DeclinesAWrongExtension(t *testing.T) {
	f := scanner.File{Path: "README.md", Content: []byte("# hello\n")}
	entries, handled, err := Extract(f, "giraffe", nil)
	if handled || entries != nil || err != nil {
		t.Errorf("Extract(.md) = %v, %v, %v, want nil, false, nil", entries, handled, err)
	}
}

// TestExtract_DeclinesUnparseableContent (directive 016 §1): the
// extension matched, the content did not — the same "not handled"
// answer as a wrong extension, with the parse failure attached as a
// diagnostic rather than silently dropped.
func TestExtract_DeclinesUnparseableContent(t *testing.T) {
	f := scanner.File{Path: "broken.go", Content: []byte("not valid go {{{")}
	entries, handled, err := Extract(f, "giraffe", nil)
	if handled || entries != nil {
		t.Errorf("Extract(unparseable .go) = %v, %v, want nil, false", entries, handled)
	}
	if err == nil {
		t.Error("expected a parse-error diagnostic, got nil")
	}
}

func names(entries []entry.Entry) []string {
	var ns []string
	for _, e := range entries {
		ns = append(ns, e.Name)
	}
	return ns
}
