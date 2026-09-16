package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"giraffe/internal/entry"
)

// TestParseArgs_Page (directive 033, replacing 032's --offset): --page
// parses as a positive integer, defaults to 1 (the top of the ranking)
// when absent, and rejects a missing, zero, negative, or non-numeric
// value the same way --query already rejects a missing one.
func TestParseArgs_Page(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantWord string
		wantPage int
		wantErr  bool
	}{
		{"absent defaults to one", []string{"prefix"}, "prefix", 1, false},
		{"present", []string{"prefix", "--page", "3"}, "prefix", 3, false},
		{"present before word", []string{"--page", "3", "prefix"}, "prefix", 3, false},
		{"one is valid", []string{"prefix", "--page", "1"}, "prefix", 1, false},
		{"missing value", []string{"prefix", "--page"}, "", 0, true},
		{"zero is invalid", []string{"prefix", "--page", "0"}, "", 0, true},
		{"negative", []string{"prefix", "--page", "-1"}, "", 0, true},
		{"non-numeric", []string{"prefix", "--page", "three"}, "", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			word, _, page, err := parseArgs(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error for args %v, got word=%q page=%d", tt.args, word, page)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for args %v: %v", tt.args, err)
			}
			if word != tt.wantWord || page != tt.wantPage {
				t.Fatalf("args %v: got word=%q page=%d, want word=%q page=%d",
					tt.args, word, page, tt.wantWord, tt.wantPage)
			}
		})
	}
}

// TestParseArgs_QueryAndPageTogether: the two flags are independent, same
// as --query already coexists with a positional word.
func TestParseArgs_QueryAndPageTogether(t *testing.T) {
	word, queryRaw, page, err := parseArgs([]string{"--query", "type:LogsOptions", "--page", "2"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if word != "" || queryRaw != "type:LogsOptions" || page != 2 {
		t.Fatalf("got word=%q queryRaw=%q page=%d", word, queryRaw, page)
	}
}

// TestParseArgs_OffsetIsGone (directive 033): --offset shipped one
// directive ago and nothing depended on it (confirmed by grep before
// this directive's change); it is no longer a recognized flag, and is
// now just an unexpected positional-style argument like any other
// unrecognized token.
func TestParseArgs_OffsetIsGone(t *testing.T) {
	_, _, _, err := parseArgs([]string{"prefix", "--offset", "8"})
	if err == nil {
		t.Fatalf("expected --offset to be rejected as an unrecognized flag")
	}
}

// TestIsHelp (directive 042, from 041): help is asked for by --help, -h
// or help first, or by --help after a subcommand; a word that merely
// contains "help" is a search, not a request for help.
func TestIsHelp(t *testing.T) {
	tests := []struct {
		args []string
		want bool
	}{
		{[]string{"--help"}, true},
		{[]string{"-h"}, true},
		{[]string{"help"}, true},
		{[]string{"find", "--help"}, true},
		{[]string{"find", "metrics", "-h"}, true},
		{[]string{"find", "helper"}, false},
		{[]string{"find", "metrics"}, false},
		{[]string{"prepare"}, false},
	}
	for _, tt := range tests {
		if got := isHelp(tt.args); got != tt.want {
			t.Errorf("isHelp(%v) = %v, want %v", tt.args, got, tt.want)
		}
	}
}

// TestHelpText_StaysSmall (directive 042, from 041): the help is read
// once by every caller and then re-read on every later turn of theirs;
// it is bounded the way a result page is, so that explaining the tool
// never costs more than one answer from it. And it says what the range
// on a result line is for.
func TestHelpText_StaysSmall(t *testing.T) {
	const limit = 1000
	if len(helpText) > limit {
		t.Fatalf("helpText is %d bytes, over the %d-byte limit", len(helpText), limit)
	}
	for _, must := range []string{"find <word>", "--query", "--page", "browse a place", "line:A-B", "sed -n 'A,Bp'", "Not indexed: call sites"} {
		if !strings.Contains(helpText, must) {
			t.Errorf("helpText does not mention %q", must)
		}
	}
}

// TestExactSources (directive 044): ## source reads, from the root a
// result's project maps to, exactly the lines its range names, in rank
// order, for the exact matches only, at most maxSources of them; a
// result whose project has no root here, or whose range the file no
// longer holds, contributes nothing and stops nothing.
func TestExactSources(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n\n// X is x.\ntype X struct{}\n\nfunc Y() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	x := entry.Entry{Path: "a.go", Name: "X", Kind: "type", Project: "p", Line: 3, EndLine: 4}
	matches := []entry.Entry{
		{Path: "a.go", Name: "x", Kind: "field", Project: "elsewhere", Line: 1, EndLine: 1}, // exact, but no root for its project
		x,
		{Path: "a.go", Name: "XY", Kind: "type", Project: "p", Line: 3, EndLine: 4}, // not exact
		{Path: "a.go", Name: "X", Kind: "func", Project: "p", Line: 3, EndLine: 40}, // exact, range past the file's end
		{Path: "a.go", Name: "x", Kind: "method", Project: "p", Line: 6, EndLine: 6},
	}
	got := exactSources(matches, "x", map[string]string{"p": root})
	if len(got) != 2 || got[0].Entry.Name != "X" || got[0].Entry.Kind != "type" || got[1].Entry.Kind != "method" {
		t.Fatalf("exactSources = %+v, want X (type) then x (method)", got)
	}
	if want := []string{"// X is x.", "type X struct{}"}; !slices.Equal(got[0].Lines, want) {
		t.Errorf("lines = %q, want %q", got[0].Lines, want)
	}
	if want := []string{"func Y() {}"}; !slices.Equal(got[1].Lines, want) {
		t.Errorf("lines = %q, want %q", got[1].Lines, want)
	}
	many := make([]entry.Entry, 0, maxSources+2)
	for range maxSources + 2 {
		many = append(many, x)
	}
	if got := exactSources(many, "X", map[string]string{"p": root}); len(got) != maxSources {
		t.Errorf("%d exact matches read %d sources, want maxSources (%d)", len(many), len(got), maxSources)
	}
}
