package skillinst

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
)

// ListFile is the install list, in the global configuration directory.
const ListFile = "installs.json"

// Entry is one install: the absolute path of the SKILL.md giraffe wrote
// and the hash of the content it wrote there.
type Entry struct {
	Path string `json:"path"`
	Hash string `json:"hash"`
}

// State is how an installed file stands against the skill embedded in
// this giraffe.
type State int

const (
	// Current: the file is the embedded skill.
	Current State = iota
	// Stale: the file is what an older giraffe wrote.
	Stale
	// Hand: the file is neither — changed by hand.
	Hand
)

func (s State) String() string {
	return [...]string{"current", "stale", "changed by hand"}[s]
}

// Hash is the form a content is recorded in.
func Hash(b []byte) string {
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:])
}

// List is the installs giraffe has recorded: one entry per destination,
// by absolute path, so it cannot grow past the skills that exist where
// giraffe put them.
type List struct {
	file    string
	Entries []Entry
}

type listJSON struct {
	Version int     `json:"version"`
	Entries []Entry `json:"installs"`
}

// Load reads the list in cfgDir and drops every entry whose file no
// longer exists — a deleted skill is taken as removed on purpose, a
// moved one looks the same — writing the list back when it dropped any
// (best effort: a list that cannot be written is still returned).
func Load(cfgDir string) *List {
	l := &List{}
	if cfgDir == "" {
		return l
	}
	l.file = filepath.Join(cfgDir, ListFile)
	b, err := os.ReadFile(l.file)
	if err != nil {
		return l
	}
	var j listJSON
	if json.Unmarshal(b, &j) != nil {
		return l
	}
	dropped := false
	for _, e := range j.Entries {
		if _, err := os.Stat(e.Path); err != nil {
			dropped = true
			continue
		}
		l.Entries = append(l.Entries, e)
	}
	if dropped {
		_ = l.Save()
	}
	return l
}

// Has reports whether path is in the list.
func (l *List) Has(path string) bool {
	for _, e := range l.Entries {
		if e.Path == path {
			return true
		}
	}
	return false
}

// Set records that giraffe wrote content at path, replacing the entry
// the path already has.
func (l *List) Set(path string, content []byte) {
	h := Hash(content)
	for i := range l.Entries {
		if l.Entries[i].Path == path {
			l.Entries[i].Hash = h
			return
		}
	}
	l.Entries = append(l.Entries, Entry{Path: path, Hash: h})
}

// Save writes the list, entries sorted by path.
func (l *List) Save() error {
	if l.file == "" {
		return nil
	}
	sort.Slice(l.Entries, func(i, j int) bool { return l.Entries[i].Path < l.Entries[j].Path })
	b, err := json.MarshalIndent(listJSON{Version: 1, Entries: l.Entries}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(l.file), 0o755); err != nil {
		return err
	}
	return writeFile(l.file, append(b, '\n'))
}

// StateOf tells whether the file of e is current, stale or changed by
// hand, against the embedded skill.
func StateOf(e Entry, embedded []byte) State {
	b, err := os.ReadFile(e.Path)
	if err != nil {
		return Hand
	}
	h := Hash(b)
	switch {
	case h == Hash(embedded):
		return Current
	case h == e.Hash:
		return Stale
	}
	return Hand
}

// NotCurrent is how many listed destinations are stale or changed by
// hand — what the one line a person sees counts. Reading the list
// drops the entries whose file is gone.
func NotCurrent(cfgDir string, embedded []byte) int {
	n := 0
	for _, e := range Load(cfgDir).Entries {
		if StateOf(e, embedded) != Current {
			n++
		}
	}
	return n
}

// writeFile writes b at path through a temporary file in the same
// directory, so a reader never sees half a file.
func writeFile(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".giraffe-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, 0o644); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}
