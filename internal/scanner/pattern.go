package scanner

import (
	"regexp"
	"strings"
)

// Pattern is one exclusion rule, in gitignore's own syntax — the same
// syntax and the same parser for .gitignore and .giraffe/exclude alike
// (directive 023 item 1: "there is one parser to write, not two").
// scanner.Pattern's own bespoke format (directive 022) is gone; every
// Pattern in this package, wherever it came from, is built by
// ParsePattern.
//
// The zero value is never used as a real pattern (Go policy G20 does not
// apply here the way it does to a persisted cache: nothing constructs a
// Pattern any way but ParsePattern, and an empty line is never a valid
// pattern — ParsePattern's own ok=false covers that case instead of a
// usable zero value).
type Pattern struct {
	raw      string         // the original line, unparsed — String's own source, and the fingerprint's
	negate   bool           // a leading "!"
	dirOnly  bool           // a trailing, unescaped "/"
	anchored bool           // an interior "/", or a leading one — relative to the pattern's own directory, not any depth below it
	leaf     *regexp.Regexp // matches relPath itself, as a whole
	nested   *regexp.Regexp // matches relPath as anything strictly below a path leaf matches — real git's "cannot re-include a file whose parent directory is excluded", the reason pruning by a .gitignore-sourced rule is safe (walk.go's own comment)
}

// String reports p's original source line — the exact text ParsePattern
// was given, negation and all. Used only for diagnostics and for
// enum.go's own configuration fingerprint (fingerprintConfig), which
// needs a stable, comparable representation of "what patterns were in
// force", not a re-derivation of the parsed form.
func (p Pattern) String() string { return p.raw }

// negation reports whether p is a negated pattern and the same Pattern
// with negate cleared — included (below) uses this to apply a negated
// rule's own un-negated match without also treating the rule itself as
// negated recursively.
func (p Pattern) negation() (bool, Pattern) {
	if !p.negate {
		return false, p
	}
	q := p
	q.negate = false
	return true, q
}

// trimTrailingUnescapedSpace removes trailing whitespace from line
// unless it is escaped with a backslash — gitignore's own rule
// ("Trailing spaces are ignored unless they are quoted with backslash"),
// applied once here rather than by every caller, so .gitignore and
// .giraffe/exclude get it identically (item 1's own "one parser").
// Leading whitespace is left alone: gitignore gives it no special
// meaning, and a pattern with genuine leading spaces in a real file name
// is rare enough that guessing at it would cost more than it returns.
func trimTrailingUnescapedSpace(line string) string {
	trimmed := strings.TrimRight(line, " \t")
	if trimmed == line {
		return line
	}
	// A backslash immediately before the run of trailing whitespace
	// escapes one trailing space (git keeps only the one it quotes, not
	// the whole run) — keep exactly that one escaped space, its
	// backslash removed, rather than the rest of the run.
	if strings.HasSuffix(trimmed, `\`) && !strings.HasSuffix(trimmed, `\\`) {
		return trimmed[:len(trimmed)-1] + line[len(trimmed):len(trimmed)+1]
	}
	return trimmed
}

// ParsePattern parses one line of gitignore syntax into a Pattern. ok is
// false for a blank line or a comment (an unescaped leading "#") — not a
// pattern at all, the same way a blank line in the file is not — and for
// any other line this parser cannot make sense of, which is a forward-
// compatibility guard for gitignore syntax gaining a form this parser
// does not know, not licence to leave anything gitignore specifies today
// unimplemented (directive 023 item 1). Nothing in the syntax this
// function implements below can actually reach that path — every string
// gitignore(5) permits compiles to something — so ok=false in practice
// means exactly "blank or comment".
//
// Supported, matching gitignore(5) in full:
//   - blank lines and comments ("#", unless escaped "\#")
//   - a leading "!" negates ("\!" for a literal one)
//   - "\" escapes the character after it, anywhere in the pattern
//     (special or not), so a literal "*", "?", "[", or space can appear
//     in a pattern
//   - a trailing, unescaped "/" restricts the pattern to directories
//   - a pattern with an interior "/", or a leading one, is anchored to
//     the declaring directory; one with no interior "/" (bare) matches
//     at any depth below it
//   - "*" matches any run of characters except "/"; "?" matches exactly
//     one, except "/"
//   - "[...]" is a character class ("[a-z]", "[!a-z]" or "[^a-z]" negated)
//   - "**/" (a leading component) matches zero or more leading directories
//   - "/**" (a trailing component) matches one or more characters below
//     the directory named by the rest of the pattern
//   - "/**/ " (an interior component) matches zero or more directories
//   - any other run of consecutive "*" is not one of the three forms
//     above ("other consecutive asterisks are considered invalid",
//     gitignore(5)) and is treated as a single "*" — matching within one
//     path segment, degrading rather than erroring, since this is
//     already a case real gitignore leaves undefined
func ParsePattern(rawLine string) (Pattern, bool) {
	raw := rawLine
	line := trimTrailingUnescapedSpace(rawLine)
	if line == "" {
		return Pattern{}, false
	}
	if line[0] == '#' {
		return Pattern{}, false
	}

	negate := false
	switch {
	case strings.HasPrefix(line, "!"):
		negate = true
		line = line[1:]
	case strings.HasPrefix(line, `\!`):
		line = line[1:]
	case strings.HasPrefix(line, `\#`):
		line = line[1:]
	}
	if line == "" {
		return Pattern{}, false
	}

	dirOnly := false
	if strings.HasSuffix(line, "/") && !strings.HasSuffix(line, `\/`) {
		dirOnly = true
		line = strings.TrimSuffix(line, "/")
		if line == "" {
			return Pattern{}, false
		}
	}

	// Anchored is decided from the pattern as written, including a
	// leading "/" — checked before that leading "/" is stripped below,
	// exactly as directive 022's own Pattern.excludes already did for
	// the bespoke format this replaces: "/build" and "sub/build" are
	// both anchored (a separator at the beginning or middle, gitignore's
	// own words); "build" alone is not.
	anchored := strings.Contains(line, "/")
	line = strings.TrimPrefix(line, "/")
	if line == "" {
		// A pattern that was only ever "/" (or "!/", "/ /", etc. after
		// trimming) matches nothing meaningful — not a real case any
		// gitignore file has a reason to contain.
		return Pattern{}, false
	}

	body := compilePatternBody(line)
	prefix := "^"
	if !anchored {
		prefix = `^(?:.*/)?`
	}
	leaf := regexp.MustCompile(prefix + body + "$")
	nested := regexp.MustCompile(prefix + body + "/.*$")

	return Pattern{raw: raw, negate: negate, dirOnly: dirOnly, anchored: anchored, leaf: leaf, nested: nested}, true
}

// excludes reports whether p matches relPath — a file when isDir is
// false, a directory when true. Checked in two parts:
//
//   - nested: does some proper ancestor directory of relPath match p's
//     own pattern? If so, relPath is excluded regardless of isDir and
//     regardless of whether p is itself dirOnly — a pattern that matches
//     a directory excludes everything below it, gitignore's own rule,
//     and only a directory can be a proper ancestor of anything, so this
//     check alone never needs isDir to know that.
//   - leaf: does p's own pattern match relPath itself? A dirOnly pattern
//     can only match a directory at this level (isDir must be true); any
//     other pattern matches a file or a directory alike.
func (p Pattern) excludes(relPath string, isDir bool) bool {
	if p.nested.MatchString(relPath) {
		return true
	}
	if p.dirOnly && !isDir {
		return false
	}
	return p.leaf.MatchString(relPath)
}

// compilePatternBody translates line — a gitignore pattern with negation,
// directory-only trailing slash, and leading "/" already stripped — into
// a regexp fragment matching exactly the path (or path suffix, for an
// unanchored pattern's leaf match — see ParsePattern's own prefix) it
// denotes. line is split on "/" into segments; a segment that is exactly
// "**" is a globstar component (compiled by compileGlobstars, which
// needs to see the whole segment list to know whether each one is
// leading, trailing, or interior); every other segment is translated
// independently by compileSegment, since "/" cannot occur inside a
// non-globstar segment by construction.
func compilePatternBody(line string) string {
	segments := strings.Split(line, "/")
	return compileGlobstars(segments)
}

// compileGlobstars joins segments' own translated regexes with "/",
// except where a segment is a bare "**", which contributes a zero-or-
// more-directories construct instead of a literal component — the three
// forms gitignore(5) defines (leading, trailing, interior), chosen by
// this globstar segment's position among the others.
func compileGlobstars(segments []string) string {
	var parts []string
	prevGlobstar := false
	for i, seg := range segments {
		if seg == "**" {
			switch {
			case len(segments) == 1:
				parts = append(parts, ".*")
			case i == 0:
				parts = append(parts, `(?:.*/)?`)
			case i == len(segments)-1:
				parts = append(parts, `/.+`)
			default:
				// Zero or more whole directories, the slash on both
				// sides included in the repeated group itself so the
				// following segment attaches directly: "a/**/b" must
				// match "a/b" (zero directories between them) as well
				// as "a/x/b" and "a/x/y/b".
				parts = append(parts, `/(?:[^/]+/)*`)
			}
			prevGlobstar = true
			continue
		}
		frag := compileSegment(seg)
		if i > 0 && !prevGlobstar {
			frag = "/" + frag
		}
		parts = append(parts, frag)
		prevGlobstar = false
	}
	return strings.Join(parts, "")
}

// compileSegment translates one "/"-free glob segment — "*", "?", "[...]",
// "\"-escapes, and literal characters — into a regexp fragment matching
// exactly that segment (never a "/", since a single path component never
// contains one). A run of "*" longer than the globstar forms
// compileGlobstars already extracted (this function never sees a bare
// "**" segment; the only way multiple asterisks reach here is something
// like "a**b" within one segment) collapses to the same "match within
// this segment" behaviour a single "*" gets — gitignore(5)'s own
// "other consecutive asterisks are considered invalid", degraded rather
// than rejected.
func compileSegment(seg string) string {
	var b strings.Builder
	for i := 0; i < len(seg); {
		c := seg[i]
		switch {
		case c == '\\' && i+1 < len(seg):
			b.WriteString(regexp.QuoteMeta(string(seg[i+1])))
			i += 2
		case c == '\\':
			b.WriteString(regexp.QuoteMeta(`\`))
			i++
		case c == '*':
			b.WriteString(`[^/]*`)
			for i < len(seg) && seg[i] == '*' {
				i++
			}
		case c == '?':
			b.WriteString(`[^/]`)
			i++
		case c == '[':
			frag, next := compileClass(seg, i)
			b.WriteString(frag)
			i = next
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
			i++
		}
	}
	return b.String()
}

// compileClass translates a "[...]" character class starting at seg[i]
// (seg[i] == '[') into a regexp character class, and returns the index
// just past it. If seg has no closing "]" at all, "[" is treated as a
// literal character — the same tolerant fallback fnmatch itself gives a
// malformed class, rather than failing the whole pattern over it.
func compileClass(seg string, i int) (frag string, next int) {
	j := i + 1
	neg := false
	if j < len(seg) && (seg[j] == '!' || seg[j] == '^') {
		neg = true
		j++
	}
	start := j
	if j < len(seg) && seg[j] == ']' {
		// A "]" immediately after the opening bracket (or its negation)
		// is a literal member of the class, not the close.
		j++
	}
	for j < len(seg) && seg[j] != ']' {
		j++
	}
	if j >= len(seg) {
		return regexp.QuoteMeta("["), i + 1
	}
	cls := seg[start:j]
	cls = strings.ReplaceAll(cls, `\`, `\\`)
	var b strings.Builder
	b.WriteByte('[')
	if neg {
		b.WriteByte('^')
	}
	b.WriteString(cls)
	b.WriteByte(']')
	return b.String(), j + 1
}
