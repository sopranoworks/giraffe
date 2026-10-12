package index

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/sopranoworks/giraffe/internal/gconfig"
)

// LoadExtensions reads operator language assignments — the only tier
// that can know a project's own convention (".inc" is C here, assembler
// there), and the only tier that can say "index this" without also
// having to say what it is (directive 019 §4). Separated from exclusion
// patterns into their own file and their own format (directive 023 item
// 2: "extension definitions are not exclusion rules") — before this
// directive they lived in .giraffe/exclude, told apart from exclusion
// patterns only by line shape, which is exactly why exclude could not
// simply be gitignore syntax (item 1).
//
// Two layers, read in this order so the second overrides the first by
// plain map assignment — no separate precedence bookkeeping needed:
//
//   - global: $XDG_CONFIG_HOME/giraffe/extensions, or
//     ~/.config/giraffe/extensions when XDG_CONFIG_HOME is unset — that
//     ".inc" means C is a fact about a person's work, not about one
//     repository (item 2), so it belongs beside every other project this
//     operator touches, not copied into each one
//   - per project: root/.giraffe/extensions — overrides the global for
//     the same extension, the project's own word on its own convention
//
// Neither file existing is not an error: assignments is simply empty,
// the same "nothing configured yet" convention LoadExclude and LoadRoots
// already have.
func LoadExtensions(root string) (map[string]string, error) {
	assignments := make(map[string]string)
	if dir := globalConfigDir(); dir != "" {
		if err := loadExtensionsFile(filepath.Join(dir, "extensions"), assignments); err != nil {
			return nil, err
		}
	}
	if err := loadExtensionsFile(filepath.Join(root, Dir, "extensions"), assignments); err != nil {
		return nil, err
	}
	return assignments, nil
}

// globalConfigDir is giraffe's own directory under the operator's XDG
// config home — "" if neither $XDG_CONFIG_HOME nor $HOME can be
// resolved, in which case LoadExtensions simply has no global layer to
// read (a missing environment is not a fatal condition here: the
// per-project layer alone is still a complete, working configuration).
func globalConfigDir() string { return gconfig.Dir() }

// loadExtensionsFile reads path as a list of assignment lines
// (readLinesAt — the same blank-line/comment/trim convention LoadRoots
// already uses, this file's grammar being a small, fixed one rather
// than gitignore's evolving syntax) and merges each into assignments,
// overwriting any value already there — the mechanism that makes a
// second call (the per-project layer, after the global one) override
// the first with no separate precedence logic. A missing file
// contributes nothing and is not an error (readLinesAt's own
// convention). A line that is not a well-formed "ext:<value>" or
// "ext:<value> lang:<value>" assignment is a hard error naming the file
// and the line (§26 C-29's own reasoning, kept here: this file's
// grammar is a small, fixed one — every real line in it starts "ext:",
// so a line that starts that way but does not parse is always a typo,
// never a plausible something-else the way an ordinary .gitignore-syntax
// line in .giraffe/exclude can be).
func loadExtensionsFile(path string, assignments map[string]string) error {
	lines, lineNos, err := readLinesAt(path)
	if err != nil {
		return err
	}
	for i, l := range lines {
		ext, langName, ok := parseAssignment(l)
		if !ok {
			return fmt.Errorf("%s:%d: malformed language assignment %q (want \"ext:<value>\" or \"ext:<value> lang:<value>\", either order)", path, lineNos[i], l)
		}
		assignments["."+ext] = langName
	}
	return nil
}

// parseAssignment recognises an operator assignment line: either
// "ext:<value>" alone (directive 019 §4 — index this extension, no
// language claim) or "ext:<value> lang:<value>" (§25 C-26 — index this
// extension and label it), in either order for the two-token form,
// neither value empty. ok=true and langName=="" together are how the
// assignments map distinguishes "not assigned at all" (the key absent)
// from "assigned, no language" (the key present with an empty value) —
// see lang/exclude.Extract, which checks presence, not non-emptiness.
func parseAssignment(line string) (ext, langName string, ok bool) {
	fields := strings.Fields(line)
	switch len(fields) {
	case 1:
		f := fields[0]
		if !strings.HasPrefix(f, "ext:") {
			return "", "", false
		}
		ext = strings.TrimPrefix(f, "ext:")
		if ext == "" {
			return "", "", false
		}
		return ext, "", true
	case 2:
		var haveExt, haveLang bool
		for _, f := range fields {
			switch {
			case strings.HasPrefix(f, "ext:"):
				ext, haveExt = strings.TrimPrefix(f, "ext:"), true
			case strings.HasPrefix(f, "lang:"):
				langName, haveLang = strings.TrimPrefix(f, "lang:"), true
			default:
				return "", "", false
			}
		}
		if !haveExt || !haveLang || ext == "" || langName == "" {
			return "", "", false
		}
		return ext, langName, true
	default:
		return "", "", false
	}
}
