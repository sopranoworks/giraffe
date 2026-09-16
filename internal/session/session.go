// Package session remembers what one caller's session has already been
// shown, so that the part of an answer that describes the index rather
// than the query — the ## index and ## warnings lines — prints once per
// session and not on every call (directive 046: in a measured run the
// two blocks were a fifth of giraffe's bytes, printed on every call and
// read once). A session is whatever the caller's environment names:
// Claude Code puts its own id in CLAUDE_CODE_SESSION_ID for every
// command it runs, on the host and under the Agent SDK alike, and that
// is the one carrier read today. With no session named, every call is
// its own session and everything prints, as before — a caller giraffe
// cannot tell from a stranger is shown everything, never less.
//
// The record is derived, disposable state under the cache directory,
// like the store (specifications/giraffe.md §23 C-14): one file, the
// session's key on its first line and the lines it has been shown
// after. A record another session left is not read, only overwritten
// by the next Remember, so two sessions taking turns in one checkout
// each see the blocks again — over-printing, never suppression for
// someone who has not seen them.
package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// envKey names the caller's session in the environment. Claude Code
// sets it for every command it runs (directive 046's probe: present
// under the CLI and under the Agent SDK, equal to init's session_id).
const envKey = "CLAUDE_CODE_SESSION_ID"

// filename is the record's name under the cache directory.
const filename = "shown"

// Key is the calling session's key, or "" when the environment names
// none.
func Key() string { return os.Getenv(envKey) }

// Shown returns the lines session key has already been shown according
// to the record under cacheDir: none when key is empty, when there is
// no record, when the record belongs to another session, or when it
// cannot be read — unreadable derived state is absent state (§23 C-14),
// and the cost of treating it so is one repeat of the block.
func Shown(cacheDir, key string) []string {
	if key == "" {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(cacheDir, filename))
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if lines[0] != key {
		return nil
	}
	return lines[1:]
}

// Remember records that session key has been shown lines — all of
// them, replacing whatever the record held: the caller passes what it
// was shown before (Shown) together with what it printed now. Nothing
// is written when key is empty: with no session there is nothing to
// remember against. The record is written whole and renamed into
// place, so a reader never sees half of one.
func Remember(cacheDir, key string, lines []string) error {
	if key == "" {
		return nil
	}
	var b strings.Builder
	b.WriteString(key)
	b.WriteByte('\n')
	seen := make(map[string]bool, len(lines))
	for _, l := range lines {
		if seen[l] {
			continue
		}
		seen[l] = true
		b.WriteString(l)
		b.WriteByte('\n')
	}
	path := filepath.Join(cacheDir, filename)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("session record: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("session record: %w", err)
	}
	return nil
}
