// Package file is the generic language module: registered last, in
// internal/lang's order, and the only one that always handles what it
// is given (specifications/giraffe.md §18; directive 016 §1). It
// produces exactly the file's own entry (§22 item 4) and nothing
// else — not a fallback mechanism, not a special case, not a branch: a
// language module like any other, satisfying the same contract
// ("produce values of the attribute type"). Because it always accepts,
// nothing reaches the end of internal/lang's registration order
// unhandled.
package file

import (
	"path"

	"github.com/sopranoworks/giraffe/internal/entry"
	"github.com/sopranoworks/giraffe/internal/scanner"
)

// Extract always handles f, and never touches its content: a file this
// module reaches has already been declined by every module that reads
// declarations out of file content, so there are none to report — only
// the fact that the file itself exists and is addressable by name.
// lang is the operator's own assignment for f's extension
// (index.LoadExtensions — its own file since directive 023 item 2,
// .giraffe/exclude before it, §25 C-26) if one exists — the only tier that can
// label a file giraffe has no extractor for at all — or "" when none
// applies, including when the operator assigned the extension without
// a language claim (directive 019 §4's "ext:<x>" alone): that case
// reads back from assignments as an empty string the same way "not
// assigned" would, which is correct here — file.Extract's own contract
// with a caller is "the label, or unknown", and an operator who
// declined to name one is exactly the unknown case.
func Extract(f scanner.File, project string, assignments map[string]string) (entries []entry.Entry, calls []entry.Call, handled bool, err error) {
	lang := assignments[path.Ext(f.Path)]
	return []entry.Entry{entry.FileEntry(f.Path, project, lang)}, nil, true, nil
}
