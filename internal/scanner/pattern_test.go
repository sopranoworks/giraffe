package scanner

import "testing"

// TestParsePattern_BlankAndComment (directive 023 item 1): a blank line
// or an unescaped comment is not a pattern at all — ParsePattern's own
// ok=false, the same thing a missing .gitignore already means.
func TestParsePattern_BlankAndComment(t *testing.T) {
	for _, line := range []string{"", "   ", "\t", "# a comment"} {
		if _, ok := ParsePattern(line); ok {
			t.Errorf("ParsePattern(%q) = ok, want not-a-pattern", line)
		}
	}
}

// TestParsePattern_LeadingWhitespaceIsNotAComment: real git requires "#"
// to be the very first character — confirmed with `git check-ignore -v`
// against a fixture: a line indented before its "#" is an ordinary
// (unlikely, but real) literal pattern, not a comment.
func TestParsePattern_LeadingWhitespaceIsNotAComment(t *testing.T) {
	if _, ok := ParsePattern("  # not a comment"); !ok {
		t.Error(`ParsePattern("  # not a comment") = not ok, want a literal pattern (leading whitespace prevents "#" from starting a comment)`)
	}
}

// TestParsePattern_EscapedLeadingCharacters: "\#" and "\!" are literal,
// not a comment or a negation — gitignore(5)'s own escaping rule.
func TestParsePattern_EscapedLeadingCharacters(t *testing.T) {
	p := mustPattern(t, `\#hash.txt`)
	if !p.excludes("#hash.txt", false) {
		t.Error(`expected \#hash.txt to match a literal #hash.txt`)
	}
	p = mustPattern(t, `\!bang.txt`)
	if !p.excludes("!bang.txt", false) {
		t.Error(`expected \!bang.txt to match a literal !bang.txt`)
	}
	if p.negate {
		t.Error(`\!bang.txt must not be treated as negated`)
	}
}

func TestParsePattern_Negation(t *testing.T) {
	p := mustPattern(t, "!keep.txt")
	if !p.negate {
		t.Error("expected negate = true for !keep.txt")
	}
	if !p.excludes("keep.txt", false) {
		t.Error("expected the un-negated pattern to still match keep.txt (negation is applied by included, not excludes)")
	}
}

// TestParsePattern_DirOnlyRequiresIsDirForLeafMatch: "build/" excludes
// the directory itself and everything below it, but not a plain file
// that happens to share its name.
func TestParsePattern_DirOnlyRequiresIsDirForLeafMatch(t *testing.T) {
	p := mustPattern(t, "build/")
	if !p.excludes("build", true) {
		t.Error("expected build/ to match the directory build")
	}
	if p.excludes("build", false) {
		t.Error("expected build/ not to match a plain file named build")
	}
	if !p.excludes("build/output.o", false) {
		t.Error("expected build/ to match a file nested under build/, regardless of isDir")
	}
	if !p.excludes("a/build/output.o", false) {
		t.Error("expected the bare (any-depth) build/ to match a nested build/ ancestor too")
	}
}

// TestParsePattern_AnchoringMatchesRealGit: a leading or interior "/"
// restricts the pattern to the declaring directory; a bare pattern
// matches at any depth below it — gitignore(5)'s own distinction.
func TestParsePattern_AnchoringMatchesRealGit(t *testing.T) {
	anchored := mustPattern(t, "/build")
	if !anchored.excludes("build", false) {
		t.Error("expected /build to match build at the root")
	}
	if anchored.excludes("sub/build", false) {
		t.Error("expected /build not to match a nested sub/build")
	}

	bare := mustPattern(t, "build")
	if !bare.excludes("sub/build", false) {
		t.Error("expected bare build to match at any depth")
	}

	interior := mustPattern(t, "sub/build")
	if !interior.excludes("sub/build", false) {
		t.Error("expected sub/build to match itself")
	}
	if interior.excludes("other/sub/build", false) {
		t.Error("expected sub/build (an interior slash) to be anchored, not any-depth")
	}
}

func TestParsePattern_Wildcards(t *testing.T) {
	star := mustPattern(t, "*.log")
	if !star.excludes("build.log", false) || !star.excludes("a/b/build.log", false) {
		t.Error("expected *.log to match at any depth")
	}
	if star.excludes("a/log", false) {
		t.Error("expected *.log not to match across a path separator")
	}

	question := mustPattern(t, "a?.txt")
	if !question.excludes("ab.txt", false) {
		t.Error("expected a?.txt to match ab.txt")
	}
	if question.excludes("abc.txt", false) {
		t.Error("expected a?.txt not to match abc.txt (? is exactly one character)")
	}
}

func TestParsePattern_CharacterClass(t *testing.T) {
	cls := mustPattern(t, "[abc].txt")
	if !cls.excludes("a.txt", false) || !cls.excludes("b.txt", false) {
		t.Error("expected [abc].txt to match a.txt and b.txt")
	}
	if cls.excludes("d.txt", false) {
		t.Error("expected [abc].txt not to match d.txt")
	}

	neg := mustPattern(t, "[!abc].txt")
	if neg.excludes("a.txt", false) {
		t.Error("expected [!abc].txt not to match a.txt")
	}
	if !neg.excludes("d.txt", false) {
		t.Error("expected [!abc].txt to match d.txt")
	}

	rng := mustPattern(t, "[a-c].txt")
	if !rng.excludes("b.txt", false) {
		t.Error("expected [a-c].txt to match b.txt (a range)")
	}
}

func TestParsePattern_EscapedGlobCharacters(t *testing.T) {
	p := mustPattern(t, `literal\*star.txt`)
	if !p.excludes("literal*star.txt", false) {
		t.Error(`expected literal\*star.txt to match the literal file name`)
	}
	if p.excludes("literalXstar.txt", false) {
		t.Error(`expected literal\*star.txt not to behave like a wildcard`)
	}
}

// TestParsePattern_GlobstarLeading: "**/foo" matches foo anywhere,
// including at the root — gitignore(5)'s own worked example.
func TestParsePattern_GlobstarLeading(t *testing.T) {
	p := mustPattern(t, "**/foo")
	for _, path := range []string{"foo", "a/foo", "a/b/foo"} {
		if !p.excludes(path, false) {
			t.Errorf("expected **/foo to match %s", path)
		}
	}
}

// TestParsePattern_GlobstarTrailing: "abc/**" matches everything inside
// abc, at any depth, but not abc itself.
func TestParsePattern_GlobstarTrailing(t *testing.T) {
	p := mustPattern(t, "abc/**")
	if p.excludes("abc", true) {
		t.Error("expected abc/** not to match abc itself")
	}
	for _, path := range []string{"abc/x", "abc/x/y"} {
		if !p.excludes(path, false) {
			t.Errorf("expected abc/** to match %s", path)
		}
	}
}

// TestParsePattern_GlobstarInterior: "a/**/b" matches a/b, a/x/b,
// a/x/y/b — gitignore(5)'s own worked example.
func TestParsePattern_GlobstarInterior(t *testing.T) {
	p := mustPattern(t, "a/**/b")
	for _, path := range []string{"a/b", "a/x/b", "a/x/y/b"} {
		if !p.excludes(path, false) {
			t.Errorf("expected a/**/b to match %s", path)
		}
	}
	if p.excludes("a/c", false) {
		t.Error("expected a/**/b not to match a/c")
	}
}

// TestParsePattern_OtherConsecutiveAsterisksDegradeToSingleStar:
// gitignore(5) calls this case invalid; this parser degrades to
// single-segment matching rather than erroring (item 1's own forward-
// compatibility posture applied to an already-undefined case).
func TestParsePattern_OtherConsecutiveAsterisksDegradeToSingleStar(t *testing.T) {
	p := mustPattern(t, "a**b.txt")
	if !p.excludes("aXb.txt", false) {
		t.Error("expected a**b.txt to degrade to matching within one segment")
	}
	if p.excludes("a/b.txt", false) {
		t.Error("expected the degraded form still not to cross a path separator")
	}
}

func TestParsePattern_TrailingSpaceEscaped(t *testing.T) {
	p := mustPattern(t, `trailing\ .txt`)
	if !p.excludes("trailing .txt", false) {
		t.Error(`expected trailing\ .txt to match a file name with a real trailing space`)
	}
}

func TestParsePattern_UnclosedBracketIsLiteral(t *testing.T) {
	p := mustPattern(t, "[abc.txt")
	if !p.excludes("[abc.txt", false) {
		t.Error("expected an unclosed [ to be treated as a literal character")
	}
}

// TestDefaultExclude_AllPatternsParse guards defaultExcludeRules' own
// panic: every literal in defaultExclude must be valid gitignore syntax,
// checked directly rather than only incidentally by whatever test
// happens to call Walk first.
func TestDefaultExclude_AllPatternsParse(t *testing.T) {
	for _, raw := range defaultExclude {
		if _, ok := ParsePattern(raw); !ok {
			t.Errorf("defaultExclude entry %q does not parse", raw)
		}
	}
}
