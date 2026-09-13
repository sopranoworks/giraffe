package golang

import (
	"testing"

	"giraffe/internal/entry"
	"giraffe/internal/scanner"
)

const src = `// Package sample is a fixture for the extractor's own tests.
package sample

// Retry retries with backoff.
func Retry() error { return nil }

func unexported() {}

// Config holds retry settings.
type Config struct{}

// Do is a method and is out of scope for this version.
func (c Config) Do() {}

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
	entries, err := Extract(f, "giraffe")
	if err != nil {
		t.Fatal(err)
	}

	got := make(map[string]entry.Entry)
	for _, e := range entries {
		got[e.Name] = e
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
		if e.Path != "internal/retry/retry.go" {
			t.Errorf("%s: path = %q", name, e.Path)
		}
		if len(e.Position) != 1 || e.Position[0].Name != "module" || e.Position[0].Value != "internal/retry" {
			t.Errorf("%s: position = %v, want module:internal/retry", name, e.Position)
		}
	}

	if _, present := got["Do"]; present {
		t.Error("Do should not have been extracted: methods are out of scope regardless of export")
	}
}

func names(entries []entry.Entry) []string {
	var ns []string
	for _, e := range entries {
		ns = append(ns, e.Name)
	}
	return ns
}
