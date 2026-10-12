package usagelog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const page = `## index
commit abc | symbols 5

## results: 'a b zz'
entries per word: a 3 | b 2 | zz none
none: no name, doc comment or path among the index's 5 entries contains two of these words

## results: a
narrower: x 2 | y 1
name:A kind:func path:a.go line:1-2
  func A()
name:A2 kind:func path:a.go line:3-4

## truncated: a
page 1, 1 more: giraffe --page 2 find a

## results: zz
none: no name, doc comment or path among the index's 5 entries contains "zz"

## try next
did you mean: zy

## warnings
unparsed: x.go: boom
`

func TestAnalyze(t *testing.T) {
	a := Analyze(page)
	if len(a.Sections) != 3 || a.Sections[0].N != 0 || a.Sections[1].N != 2 || a.Sections[1].S != "a" || a.Sections[2].N != 0 {
		t.Fatalf("sections = %+v", a.Sections)
	}
	if strings.Join(a.None, "|") != "a b zz|zz" {
		t.Fatalf("none = %v", a.None)
	}
	if !a.Truncated || a.Hints != 2 || len(a.Warnings) != 1 || a.Warnings[0] != "unparsed: x.go: boom" {
		t.Fatalf("answer = %+v", a)
	}
}

func TestCommand(t *testing.T) {
	for args, want := range map[string]string{"": "help", "--help": "help", "find a": "find", "--query q find a": "find", "--page 2 find a": "find", "prepare": "prepare", "bogus": "bogus"} {
		if got := Command(strings.Fields(args)); got != want {
			t.Errorf("Command(%q) = %q, want %q", args, got, want)
		}
	}
}

func read(t *testing.T, dir string) []map[string]any {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("want one file per day, got %v", files)
	}
	b, _ := os.ReadFile(files[0])
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("%q: %v", l, err)
		}
		out = append(out, m)
	}
	return out
}

func TestStartEnd(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "log")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sess-1")
	l := Begin(dir, "v0.1.0", "/repo", []string{"--query", "kind:func", "find", "a", "--page"})
	l.Indexed(7)
	l.Fail("query", "bad")
	l.End(2, page, "boom\n")
	l.End(2, page, "boom\n") // once
	recs := read(t, dir)
	if len(recs) != 2 {
		t.Fatalf("records = %v", recs)
	}
	s, e := recs[0], recs[1]
	if s["ev"] != "start" || e["ev"] != "end" || s["id"] != e["id"] || s["ses"] != "sess-1" || s["sv"] != "CLAUDE_CODE_SESSION_ID" || s["cmd"] != "find" || s["root"] != "/repo" {
		t.Fatalf("start = %v end = %v", s, e)
	}
	if a := s["args"].([]any); len(a) != 5 || a[0] != "--query" || a[4] != "--page" {
		t.Fatalf("args = %v", a)
	}
	if e["rc"] != float64(2) || e["ib"] != true || e["nd"] != float64(7) || e["trunc"] != true || e["out"] != float64(len(page)) {
		t.Fatalf("end = %v", e)
	}
	fails := e["fail"].([]any)
	if len(fails) != 2 || fails[0].(map[string]any)["k"] != "query" || fails[1].(map[string]any)["k"] != "warn" {
		t.Fatalf("fail = %v", fails)
	}
}

func TestEnd_ExitWithoutMoreSpecificFailureIsRecorded(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "log")
	l := Begin(dir, "v", "/r", []string{"bogus"})
	l.End(2, "", "giraffe: no bare search\n")
	f := read(t, dir)[1]["fail"].([]any)[0].(map[string]any)
	if f["k"] != "exit" || f["x"] != "giraffe: no bare search" {
		t.Fatalf("fail = %v", f)
	}
}

func TestNilAndUnwritableLogsDoNothing(t *testing.T) {
	var l *Log
	l.Fail("x", "y")
	l.Indexed(1)
	l.End(0, "", "")
	if Begin("", "v", "/r", nil) != nil {
		t.Fatal("no directory means no log")
	}
	file := filepath.Join(t.TempDir(), "f")
	os.WriteFile(file, nil, 0o644)
	u := Begin(filepath.Join(file, "log"), "v", "/r", nil) // a path under a file
	u.End(0, "", "")
}
