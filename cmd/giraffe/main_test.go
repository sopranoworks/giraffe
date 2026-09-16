package main

import "testing"

// TestParseArgs_Offset (directive 032): --offset parses as a non-negative
// integer, defaults to 0 when absent, and rejects a missing, negative, or
// non-numeric value the same way --query already rejects a missing one.
func TestParseArgs_Offset(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantWord   string
		wantOffset int
		wantErr    bool
	}{
		{"absent defaults to zero", []string{"prefix"}, "prefix", 0, false},
		{"present", []string{"prefix", "--offset", "8"}, "prefix", 8, false},
		{"present before word", []string{"--offset", "8", "prefix"}, "prefix", 8, false},
		{"zero is valid", []string{"prefix", "--offset", "0"}, "prefix", 0, false},
		{"missing value", []string{"prefix", "--offset"}, "", 0, true},
		{"negative", []string{"prefix", "--offset", "-1"}, "", 0, true},
		{"non-numeric", []string{"prefix", "--offset", "eight"}, "", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			word, _, offset, err := parseArgs(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error for args %v, got word=%q offset=%d", tt.args, word, offset)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for args %v: %v", tt.args, err)
			}
			if word != tt.wantWord || offset != tt.wantOffset {
				t.Fatalf("args %v: got word=%q offset=%d, want word=%q offset=%d",
					tt.args, word, offset, tt.wantWord, tt.wantOffset)
			}
		})
	}
}

// TestParseArgs_QueryAndOffsetTogether: the two flags are independent,
// same as --query already coexists with a positional word.
func TestParseArgs_QueryAndOffsetTogether(t *testing.T) {
	word, queryRaw, offset, err := parseArgs([]string{"--query", "type:LogsOptions", "--offset", "8"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if word != "" || queryRaw != "type:LogsOptions" || offset != 8 {
		t.Fatalf("got word=%q queryRaw=%q offset=%d", word, queryRaw, offset)
	}
}
