package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sopranoworks/giraffe/internal/entry"
	"github.com/sopranoworks/giraffe/internal/query"
)

// TestParseOptions_Page (directive 033, replacing 032's --offset;
// directive 071: before the subcommand): --page parses as a positive
// integer, defaults to 1 when absent, and rejects a missing, zero,
// negative, or non-numeric value the same way --query rejects a
// missing one.
func TestParseOptions_Page(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantRest string
		wantPage int
		wantErr  bool
	}{
		{"absent defaults to one", []string{"find", "prefix"}, "find prefix", 1, false},
		{"present", []string{"--page", "3", "find", "prefix"}, "find prefix", 3, false},
		{"one is valid", []string{"--page", "1", "find", "prefix"}, "find prefix", 1, false},
		{"missing value", []string{"--page"}, "", 0, true},
		{"zero is invalid", []string{"--page", "0", "find", "prefix"}, "", 0, true},
		{"negative", []string{"--page", "-1", "find", "prefix"}, "", 0, true},
		{"non-numeric", []string{"--page", "three", "find", "prefix"}, "", 0, true},
		{"after find it is a word", []string{"find", "prefix", "--page", "3"}, "find prefix --page 3", 1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o, rest, err := parseOptions(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error for args %v, got rest=%v page=%d", tt.args, rest, o.page)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for args %v: %v", tt.args, err)
			}
			if got := strings.Join(rest, " "); got != tt.wantRest || o.page != tt.wantPage {
				t.Fatalf("args %v: got rest=%q page=%d, want rest=%q page=%d", tt.args, got, o.page, tt.wantRest, tt.wantPage)
			}
		})
	}
}

// TestParseOptions_ValueIsVerbatim (directive 071): an option that
// takes a value takes the next argument verbatim, whatever it starts
// with; nothing inside a value is interpreted.
func TestParseOptions_ValueIsVerbatim(t *testing.T) {
	o, rest, err := parseOptions([]string{"--query", "--x path:--y", "find", "usages"})
	if err != nil || o.query != "--x path:--y" || strings.Join(rest, " ") != "find usages" {
		t.Fatalf("got query=%q rest=%v err=%v", o.query, rest, err)
	}
	o, rest, err = parseOptions([]string{"--query", "kind:func", "--page", "2", "find", "usages"})
	if err != nil || o.query != "kind:func" || o.page != 2 || strings.Join(rest, " ") != "find usages" {
		t.Fatalf("got query=%q page=%d rest=%v err=%v", o.query, o.page, rest, err)
	}
	if _, _, err := parseOptions([]string{"--query"}); err == nil {
		t.Fatalf("--query without a value was accepted")
	}
	if _, _, err := parseOptions([]string{"--query", "a:b", "--query", "c:d", "find", "x"}); err == nil {
		t.Fatalf("--query twice was accepted")
	}
}

// TestParseOptions_EverythingAfterFindIsAWord (directive 071): the
// options end at the subcommand; every argument after find is a word,
// exactly as given — find, prepare, --help, --query, --show-kind, -rn,
// a path, a pattern.
func TestParseOptions_EverythingAfterFindIsAWord(t *testing.T) {
	for _, tail := range [][]string{
		{"--help"},
		{"find", "usages"},
		{"prepare"},
		{"--query", "kind:func"},
		{"--show-kind", "ShowKind"},
		{"-rn", "metrics"},
		{"-i", "email\\|mail.*send", "--type", "Classic Actor"},
		{"prefix", "--offset", "8"},
		{"Send", "actor/mailbox.go"},
		{"usages", "--"},
	} {
		args := append([]string{"find"}, tail...)
		o, rest, err := parseOptions(args)
		if err != nil || o.hasQuery || o.page != 1 || o.help || strings.Join(rest, "\x00") != strings.Join(args, "\x00") {
			t.Errorf("args %q: got rest=%q opts=%+v err=%v", args, rest, o, err)
		}
	}
}

// TestRun_Grammar (directive 071): run in a directory with no
// repository. --help and no arguments print the usage, exit 0; find
// with nothing to search, prepare with arguments, and anything else
// where the subcommand belongs exit 2 — and none of them does any index
// work: no .giraffe appears.
func TestRun_Grammar(t *testing.T) {
	dir := t.TempDir()
	wd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(wd)
	tests := []struct {
		args []string
		want int
	}{
		{nil, 0},
		{[]string{"--help"}, 0},
		{[]string{"--query", "kind:func", "--help"}, 0},
		{[]string{"find"}, 2},
		{[]string{"--page", "2", "find"}, 2},
		{[]string{"--query"}, 2},
		{[]string{"--page", "x", "find", "a"}, 2},
		{[]string{"--query", "kind:func"}, 2},
		{[]string{"cluster", "metrics"}, 2},
		{[]string{"--query", "kind:func", "cluster", "metrics"}, 2},
		{[]string{"--prepare"}, 2},
		{[]string{"-h"}, 2},
		{[]string{"help"}, 2},
		{[]string{"-rn", "metrics"}, 2},
		{[]string{"prepare", "extra"}, 2},
		{[]string{"--page", "2", "prepare"}, 2},
	}
	for _, tt := range tests {
		if got := run(t.Context(), tt.args); got != tt.want {
			t.Errorf("run(%q) = %d, want %d", tt.args, got, tt.want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".giraffe")); err == nil {
		t.Fatalf("a .giraffe directory was created by a call that must do no index work")
	}
}

// TestCommandLine (directive 071): the redirect line names the exact
// command to run instead, in the grammar — the options as given
// before find, the arguments as given after it, quoted for a shell
// where they need it.
func TestCommandLine(t *testing.T) {
	tests := []struct {
		o    options
		rest []string
		want string
	}{
		{options{page: 1}, []string{"cluster", "metrics"}, "giraffe find cluster metrics"},
		{options{page: 1}, []string{"-rn", "metrics"}, "giraffe find -rn metrics"},
		{options{page: 1}, []string{"mailbox overflow"}, "giraffe find 'mailbox overflow'"},
		{options{page: 1}, []string{"email\\|mail.*send"}, `giraffe find 'email\|mail.*send'`},
		{options{page: 1}, []string{"it's"}, `giraffe find 'it'\''s'`},
		{options{page: 2, query: "kind:func", hasQuery: true}, []string{"usages"}, "giraffe --query kind:func --page 2 find usages"},
		{options{page: 1, query: "--x path:--y", hasQuery: true}, []string{"usages"}, "giraffe --query '--x path:--y' find usages"},
	}
	for _, tt := range tests {
		if got := commandLine(tt.o, "find", tt.rest); got != tt.want {
			t.Errorf("commandLine(%+v, %q) = %q, want %q", tt.o, tt.rest, got, tt.want)
		}
	}
}

// TestHelpText_StaysSmall (directive 042, from 041): the help is read
// once by every caller and then re-read on every later turn of theirs;
// it is bounded the way a result page is, so that explaining the tool
// never costs more than one answer from it. It states the grammar
// (directive 071): every option, before find; every argument after find
// a word; prepare; and what the range on a result line is for, and
// that callers: is where a name is called (directive 050).
func TestHelpText_StaysSmall(t *testing.T) {
	const limit = 1000
	if len(helpText) > limit {
		t.Fatalf("helpText is %d bytes, over the %d-byte limit", len(helpText), limit)
	}
	for _, must := range []string{"giraffe find <word>", "giraffe prepare", "giraffe --help", "--query 'attr:value ...' find", "--page N find", "browse a place", "line:A-B", "sed -n 'A,Bp'", "callers:", "Options go before find", "every argument after find is a word"} {
		if !strings.Contains(helpText, must) {
			t.Errorf("helpText does not mention %q", must)
		}
	}
}

// Directive 073: the directory a word of the call names — a segment
// containing the word — gets its own block; the most populous such
// directory when several; none when no word names one or the call
// already narrows by path:.
func TestWordDir(t *testing.T) {
	m := []entry.Entry{{Path: "pkg/standby/a.go"}, {Path: "pkg/standby/b.go"}, {Path: "pkg/standby/c.go"}, {Path: "pkg/server/standby.go"}, {Path: "pkg/exec/x.go"}, {Path: "pkg/exec/y.go"}, {Path: "pkg/exec/z.go"}, {Path: "pkg/exec/w.go"}, {Path: "main.go"}}
	if d, n := wordDir(m, []string{"standby", "idle"}, nil); d != "pkg/standby" || n != 3 {
		t.Errorf("got %q %d, want pkg/standby 3", d, n)
	}
	if d, _ := wordDir(m, []string{"idle", "exit"}, nil); d != "" {
		t.Errorf("no word names a directory: got %q", d)
	}
	if d, _ := wordDir(m, []string{"standby"}, query.Filter{"path": {"pkg"}}); d != "" {
		t.Errorf("a call already narrowed by path: got %q", d)
	}
}

// Directive 073: a word only text can hold is looked for in the files'
// text whatever it matched; a word that could be a name is not.
func TestNameLike(t *testing.T) {
	for w, want := range map[string]bool{"Tell": true, "mailbox_full": true, "mailbox-full": false, `"x"`: false, "a.b": false} {
		if got := nameLike(w); got != want {
			t.Errorf("nameLike(%q) = %v, want %v", w, got, want)
		}
	}
}
