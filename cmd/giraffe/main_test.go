package main

import "testing"

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
