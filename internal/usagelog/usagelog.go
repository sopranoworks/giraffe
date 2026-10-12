// Package usagelog is giraffe's usage log (directive 087 §10): one JSON
// line per record, a start record when an invocation begins and an end
// record when it finishes, in $XDG_CONFIG_HOME/giraffe/log/, one file
// per day. A start with no end is an invocation that was killed from
// outside, where nothing could be written.
//
// It is read by giraffe's Coder, to see how giraffe is used. The field
// names are short and fixed; a later version may add fields but never
// changes what one means:
//
//	ev     "start" or "end"
//	id     the invocation, the same on its start and its end
//	t      time, RFC 3339 with nanoseconds
//	v      giraffe's version                                    (start)
//	root   the directory giraffe indexes                        (start)
//	cmd    the subcommand: find, prepare, install, update, help (start)
//	args   every argument exactly as typed, options included    (start)
//	ses    the agent session, exactly as the environment gives it (start;
//	       absent when none); sv names the variable
//	rc     the exit code                                        (end)
//	ms     the duration in milliseconds                         (end)
//	ib     true when the index was built or refreshed; nd the number of
//	       files dispatched. Only when an index was opened      (end)
//	res    results per section: [{"s": header, "n": results}]   (end)
//	none   the words answered "none:"                           (end)
//	trunc  true when the answer was truncated                   (end)
//	hint   the narrowing hints offered                          (end)
//	out    bytes written to standard output                     (end)
//	fail   what giraffe noticed: [{"k": kind, "x": full text}]  (end)
//
// Failure kinds: exit (a non-zero exit, the text it printed), usage (a
// command line giraffe does not take), query (a malformed --query),
// index (an index build or refresh that failed), timeout, panic, warn
// (a line under ## warnings).
//
// The log never changes what giraffe prints and never fails a command:
// every error here is dropped.
package usagelog

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SessionVars are the environment variables, in the order they are
// tried, that name an agent's session: Claude Code's, and Codex's
// thread id.
var SessionVars = []string{"CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID"}

// Section is one section of an answer and how many results it held.
type Section struct {
	S string `json:"s"`
	N int    `json:"n"`
}

// Fail is one thing giraffe noticed.
type Fail struct {
	K string `json:"k"`
	X string `json:"x"`
}

type record struct {
	Ev    string    `json:"ev"`
	ID    string    `json:"id"`
	T     string    `json:"t"`
	V     string    `json:"v,omitempty"`
	Root  string    `json:"root,omitempty"`
	Cmd   string    `json:"cmd,omitempty"`
	Args  *[]string `json:"args,omitempty"`
	Ses   string    `json:"ses,omitempty"`
	Sv    string    `json:"sv,omitempty"`
	Rc    *int      `json:"rc,omitempty"`
	Ms    *int64    `json:"ms,omitempty"`
	Ib    *bool     `json:"ib,omitempty"`
	Nd    *int      `json:"nd,omitempty"`
	Res   []Section `json:"res,omitempty"`
	None  []string  `json:"none,omitempty"`
	Trunc bool      `json:"trunc,omitempty"`
	Hint  int       `json:"hint,omitempty"`
	Out   *int      `json:"out,omitempty"`
	Fail  []Fail    `json:"fail,omitempty"`
}

// Log is one invocation's records. The zero value and a nil *Log do
// nothing.
type Log struct {
	dir   string
	id    string
	begin time.Time

	endMu      sync.Mutex
	mu         sync.Mutex
	ended      bool
	fails      []Fail
	indexed    bool
	dispatched int
}

// Begin writes the start record. dir is the log directory; "" turns the
// log off.
func Begin(dir, version, root string, args []string) *Log {
	if dir == "" {
		return nil
	}
	l := &Log{dir: dir, id: newID(), begin: time.Now()}
	a := append([]string{}, args...)
	r := record{Ev: "start", ID: l.id, T: l.begin.Format(time.RFC3339Nano), V: version, Root: root, Cmd: Command(args), Args: &a}
	for _, name := range SessionVars {
		if v := os.Getenv(name); v != "" {
			r.Ses, r.Sv = v, name
			break
		}
	}
	l.write(r)
	return l
}

// Command is the subcommand an argument list names, the way run reads
// it: the first argument that is not one of giraffe's options.
func Command(args []string) string {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--query", "--page":
			i++
		case "--help":
		default:
			return args[i]
		}
	}
	return "help"
}

func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16) + strconv.Itoa(os.Getpid())
	}
	return hex.EncodeToString(b[:])
}

// Fail records something giraffe noticed, to go in the end record.
func (l *Log) Fail(kind, text string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.fails = append(l.fails, Fail{K: kind, X: text})
	l.mu.Unlock()
}

// Indexed records that an index was opened, and how many files were
// dispatched to be read: none means it was fresh.
func (l *Log) Indexed(dispatched int) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.indexed, l.dispatched = true, dispatched
	l.mu.Unlock()
}

// End writes the end record, once: code is the exit code, stdout what
// the invocation printed and stderr what it printed to its error
// stream (recorded as a failure when code is not 0 and nothing more
// specific was).
func (l *Log) End(code int, stdout, stderr string) {
	if l == nil {
		return
	}
	// Held to the end: a second caller (the timeout's, a panic's) waits
	// for the record to be written before it ends the process.
	l.endMu.Lock()
	defer l.endMu.Unlock()
	l.mu.Lock()
	if l.ended {
		l.mu.Unlock()
		return
	}
	l.ended = true
	fails := append([]Fail{}, l.fails...)
	indexed, dispatched := l.indexed, l.dispatched
	l.mu.Unlock()

	a := Analyze(stdout)
	for _, w := range a.Warnings {
		fails = append(fails, Fail{K: "warn", X: w})
	}
	if code != 0 && len(fails) == 0 {
		fails = append(fails, Fail{K: "exit", X: strings.TrimSpace(stderr)})
	}
	ms := time.Since(l.begin).Milliseconds()
	out := len(stdout)
	r := record{Ev: "end", ID: l.id, T: time.Now().Format(time.RFC3339Nano), Rc: &code, Ms: &ms, Out: &out,
		Res: a.Sections, None: a.None, Trunc: a.Truncated, Hint: a.Hints, Fail: fails}
	if indexed {
		built := dispatched > 0
		r.Ib, r.Nd = &built, &dispatched
	}
	l.write(r)
}

func (l *Log) write(r record) {
	defer func() { _ = recover() }()
	b, err := json.Marshal(r)
	if err != nil {
		return
	}
	if err := os.MkdirAll(l.dir, 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(l.dir, l.begin.Format("2006-01-02")+".jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	_, _ = f.Write(append(b, '\n'))
	_ = f.Close()
}

// Answer is what an answer page held, read off its text.
type Answer struct {
	Sections  []Section
	None      []string
	Truncated bool
	Hints     int
	Warnings  []string
}

// Analyze reads a page as output prints it: sections open at a "## "
// line; a result is a line "name:… kind:… path:…" (a file has no line:) in a "## results"
// section; a word answered none: has a "none:" line in its results
// section; narrowing hints are the "narrower:" lines and the lines
// under "## try next"; "## warnings" lines are the warnings.
func Analyze(page string) Answer {
	var a Answer
	section, title := "", ""
	cur := -1
	for _, line := range strings.Split(page, "\n") {
		if strings.HasPrefix(line, "## ") {
			head := strings.TrimPrefix(line, "## ")
			section, title = head, ""
			if k, t, ok := strings.Cut(head, ": "); ok {
				section, title = k, t
			}
			cur = -1
			switch section {
			case "results":
				a.Sections = append(a.Sections, Section{S: title})
				cur = len(a.Sections) - 1
			case "truncated":
				a.Truncated = true
			}
			continue
		}
		switch section {
		case "results":
			switch {
			case strings.HasPrefix(line, "name:") && strings.Contains(line, " kind:") && strings.Contains(line, " path:"):
				a.Sections[cur].N++
			case strings.HasPrefix(line, "none:"):
				a.None = append(a.None, strings.Trim(title, "'"))
			case strings.HasPrefix(line, "narrower:"):
				a.Hints++
			}
		case "try next":
			if strings.TrimSpace(line) != "" {
				a.Hints++
			}
		case "warnings":
			if strings.TrimSpace(line) != "" {
				a.Warnings = append(a.Warnings, line)
			}
		}
	}
	return a
}

// String is for tests and messages.
func (a Answer) String() string {
	return fmt.Sprintf("%+v", []any{a.Sections, a.None, a.Truncated, a.Hints})
}
