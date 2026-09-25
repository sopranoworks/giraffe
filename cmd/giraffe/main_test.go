package main

import (
	"strings"
	"testing"
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
			words, _, page, err := parseArgs(tt.args)
			word := strings.Join(words, " ")
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

// TestParseArgs_SeveralWords (directive 062): every positional argument
// is a word of its own, in order, around the flags; an unknown flag is
// still an error, not a word.
func TestParseArgs_SeveralWords(t *testing.T) {
	words, _, page, err := parseArgs([]string{"metrics", "--page", "2", "actor", "count"})
	if err != nil || strings.Join(words, ",") != "metrics,actor,count" || page != 2 {
		t.Fatalf("got words=%v page=%d err=%v", words, page, err)
	}
	// h3: an unknown flag is dropped; a pattern is its words.
	words, _, _, err = parseArgs([]string{"-i", "email\\|mail.*send", "--type", "Classic Actor"})
	if err != nil || strings.Join(words, ",") != "email,mail,send,type,Classic,Actor" && strings.Join(words, ",") != "email,mail,send,Classic,Actor" {
		t.Fatalf("got words=%v err=%v", words, err)
	}
}

// TestParseArgs_QueryAndPageTogether: the two flags are independent, same
// as --query already coexists with a positional word.
func TestParseArgs_QueryAndPageTogether(t *testing.T) {
	words, queryRaw, page, err := parseArgs([]string{"--query", "type:LogsOptions", "--page", "2"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	word := strings.Join(words, " ")
	if word != "" || queryRaw != "type:LogsOptions" || page != 2 {
		t.Fatalf("got word=%q queryRaw=%q page=%d", word, queryRaw, page)
	}
}

// TestParseArgs_OffsetIsGone (directive 033): --offset is not a
// recognized flag. Since directive 062 h3 an unrecognized flag is
// dropped rather than refused, so --offset pages nothing: the page
// stays 1.
func TestParseArgs_OffsetIsGone(t *testing.T) {
	_, _, page, err := parseArgs([]string{"prefix", "--offset", "8"})
	if err != nil || page != 1 {
		t.Fatalf("--offset changed the page or failed: page=%d err=%v", page, err)
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
// on a result line is for, and that callers: is where a name is called
// (directive 050 — the line that said call sites were not indexed is
// gone with the gap).
func TestHelpText_StaysSmall(t *testing.T) {
	const limit = 1000
	if len(helpText) > limit {
		t.Fatalf("helpText is %d bytes, over the %d-byte limit", len(helpText), limit)
	}
	for _, must := range []string{"find <word>", "--query", "--page", "browse a place", "line:A-B", "sed -n 'A,Bp'", "callers:"} {
		if !strings.Contains(helpText, must) {
			t.Errorf("helpText does not mention %q", must)
		}
	}
}
