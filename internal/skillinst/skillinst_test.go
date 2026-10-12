package skillinst

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var skillText = []byte("---\nname: giraffe\ndescription: d\n---\nbody\n")

type fixture struct {
	t    *testing.T
	home string
	repo string
	cfg  string
	out  bytes.Buffer
	err  bytes.Buffer
}

func newFixture(t *testing.T) *fixture {
	root := t.TempDir()
	f := &fixture{t: t, home: filepath.Join(root, "home"), repo: filepath.Join(root, "repo"), cfg: filepath.Join(root, "cfg")}
	for _, d := range []string{f.home, f.repo, f.cfg} {
		os.MkdirAll(d, 0o755)
	}
	return f
}

func (f *fixture) env(answers string, skill []byte) *Env {
	f.out.Reset()
	f.err.Reset()
	return &Env{Skill: skill, CfgDir: f.cfg, Home: f.home, RepoDir: func() string { return f.repo }, In: bufio.NewReader(strings.NewReader(answers)), Out: &f.out, Err: &f.err}
}

func (f *fixture) file(p string) string { return filepath.Join(f.home, p) }

func TestBuiltinAgents_AndConfigured(t *testing.T) {
	f := newFixture(t)
	as, err := Agents(f.cfg)
	if err != nil || Names(as) != "claude, codex" {
		t.Fatalf("%v %v", as, err)
	}
	os.WriteFile(filepath.Join(f.cfg, "agents"), []byte("# c\ncursor ~/.cursor/s/giraffe .cursor/s/giraffe\nclaude \"~/my claude/giraffe\" .mc/giraffe\n"), 0o644)
	as, err = Agents(f.cfg)
	if err != nil || Names(as) != "claude, codex, cursor" {
		t.Fatalf("%v %v", as, err)
	}
	c, _ := Find(as, "claude")
	g, _ := c.Dir(Global, f.home, f.repo)
	l, _ := c.Dir(Local, f.home, f.repo)
	if g != filepath.Join(f.home, "my claude/giraffe") || l != filepath.Join(f.repo, ".mc/giraffe") {
		t.Fatalf("claude replaced: %s %s", g, l)
	}
	for _, bad := range []string{"x only-two\n", "x /abs ../up\n", "x rel/global .l\n", "bad!name /a .b\n", "x /a /abs\n"} {
		os.WriteFile(filepath.Join(f.cfg, "agents"), []byte(bad), 0o644)
		if _, err := Agents(f.cfg); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
}

func TestOptions_Conflicts(t *testing.T) {
	as := Builtin()
	for args, want := range map[string]string{
		"--global --local":         "--global and --local",
		"--path x --agent claude":  "--path and --agent",
		"--path x --global":        "--path cannot be combined",
		"--path x --local":         "--path cannot be combined",
		"--agent nope":             "unknown agent",
		"--agent claude --agent x": "twice",
		"--bogus":                  "install takes",
		"--agent":                  "takes a value",
	} {
		o, err := ParseOptions(strings.Fields(args))
		if err == nil {
			err = o.Validate(as)
		}
		if err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "\n") {
			t.Errorf("%q: err = %v, want one line containing %q", args, err, want)
		}
	}
	if o, err := ParseOptions([]string{"--local", "--agent", "codex"}); err != nil || o.Validate(as) != nil {
		t.Errorf("a valid combination was rejected: %v", err)
	}
}

func TestInstall_WritesRecordsAndNeverInstructionFiles(t *testing.T) {
	f := newFixture(t)
	if c := Install(f.env("", skillText), Options{HasAgent: true, Agent: "claude"}); c != 0 {
		t.Fatal(c, f.err.String())
	}
	if b, _ := os.ReadFile(f.file(".claude/skills/giraffe/SKILL.md")); !bytes.Equal(b, skillText) {
		t.Fatal("not written")
	}
	// Again to the same place, confirmed: one entry, rewritten.
	if c := Install(f.env("y\n", skillText), Options{HasAgent: true, Agent: "claude"}); c != 0 {
		t.Fatal(c)
	}
	if !strings.Contains(f.out.String(), "already holds a skill") {
		t.Fatalf("not asked: %q", f.out.String())
	}
	if n := len(Load(f.cfg).Entries); n != 1 {
		t.Fatalf("entries = %d", n)
	}
	// The default asks, and n writes nothing.
	if c := Install(f.env("n\n", skillText), Options{Local: true}); c != 0 {
		t.Fatal(c)
	}
	if _, err := os.Stat(filepath.Join(f.repo, ".claude")); err == nil {
		t.Fatal("answering n wrote")
	}
	// Local, every agent, y.
	Install(f.env("y\n", skillText), Options{Local: true})
	for _, p := range []string{".claude/skills/giraffe/SKILL.md", ".agents/skills/giraffe/SKILL.md"} {
		if _, err := os.Stat(filepath.Join(f.repo, p)); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	filepath.Walk(filepath.Dir(f.repo), func(p string, i os.FileInfo, _ error) error {
		if b := strings.ToLower(filepath.Base(p)); b == "agents.md" || b == "claude.md" {
			t.Errorf("an instruction file was written: %s", p)
		}
		return nil
	})
}

func TestStates_AndDropOfMissing(t *testing.T) {
	f := newFixture(t)
	old := []byte("---\nname: giraffe\n---\nolder\n")
	Install(f.env("", old), Options{HasAgent: true, Agent: "claude"})
	Install(f.env("", old), Options{HasAgent: true, Agent: "codex"})
	Install(f.env("", skillText), Options{HasPath: true, Path: filepath.Join(f.repo, "p")})
	st := map[string]State{}
	for _, e := range Load(f.cfg).Entries {
		st[filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(e.Path))))] = StateOf(e, skillText)
	}
	if st[".claude"] != Stale || st[".agents"] != Stale {
		t.Fatalf("states = %v", st)
	}
	if n := NotCurrent(f.cfg, skillText); n != 2 {
		t.Fatalf("NotCurrent = %d", n)
	}
	os.WriteFile(f.file(".claude/skills/giraffe/SKILL.md"), []byte("mine"), 0o644)
	var hand int
	for _, e := range Load(f.cfg).Entries {
		if StateOf(e, skillText) == Hand {
			hand++
		}
	}
	if hand != 1 {
		t.Fatalf("hand = %d", hand)
	}
	os.Remove(f.file(".agents/skills/giraffe/SKILL.md"))
	if n := len(Load(f.cfg).Entries); n != 2 {
		t.Fatalf("a deleted file's entry was not dropped: %d", n)
	}
	// Dropped for good: update does not recreate it.
	Update(f.env("y\n", skillText))
	if _, err := os.Stat(f.file(".agents/skills/giraffe/SKILL.md")); err == nil {
		t.Fatal("update recreated a deleted skill")
	}
}

func TestUpdate(t *testing.T) {
	f := newFixture(t)
	old := []byte("---\nname: giraffe\n---\nolder\n")
	Install(f.env("", old), Options{HasAgent: true, Agent: "claude"})
	os.WriteFile(f.file(".claude/skills/giraffe/SKILL.md"), []byte("---\nname: giraffe\n---\nmine\n"), 0o644)
	// Hand-changed: listed apart, kept on n.
	Update(f.env("n\nn\n", skillText))
	if !strings.Contains(f.out.String(), "changed by hand") {
		t.Fatalf("out = %q", f.out.String())
	}
	if b, _ := os.ReadFile(f.file(".claude/skills/giraffe/SKILL.md")); !strings.Contains(string(b), "mine") {
		t.Fatal("overwritten without a y")
	}
	Update(f.env("y\n", skillText))
	if b, _ := os.ReadFile(f.file(".claude/skills/giraffe/SKILL.md")); !bytes.Equal(b, skillText) {
		t.Fatal("not updated on y")
	}
	if n := NotCurrent(f.cfg, skillText); n != 0 {
		t.Fatalf("NotCurrent = %d", n)
	}
}

func TestUpdate_FindsAHandPlacedSkill_AndAnEmptyListInstalls(t *testing.T) {
	f := newFixture(t)
	os.MkdirAll(f.file(".agents/skills/giraffe"), 0o755)
	os.WriteFile(f.file(".agents/skills/giraffe/SKILL.md"), []byte("---\nname: giraffe\n---\nhand\n"), 0o644)
	os.MkdirAll(f.file(".claude/skills/giraffe"), 0o755)
	os.WriteFile(f.file(".claude/skills/giraffe/SKILL.md"), []byte("---\nname: other\n---\nnot ours\n"), 0o644)
	Update(f.env("y\n", skillText))
	if !strings.Contains(f.out.String(), ".agents/skills/giraffe/SKILL.md") || strings.Contains(f.out.String(), ".claude/skills/giraffe/SKILL.md") {
		t.Fatalf("out = %q", f.out.String())
	}
	if e := Load(f.cfg).Entries; len(e) != 1 || !strings.HasSuffix(e[0].Path, ".agents/skills/giraffe/SKILL.md") {
		t.Fatalf("entries = %v", e)
	}
	if b, _ := os.ReadFile(f.file(".claude/skills/giraffe/SKILL.md")); strings.Contains(string(b), "name: giraffe") {
		t.Fatal("a skill that is not giraffe's was touched")
	}
	// Empty list, nothing found: behaves as install with no options.
	g := newFixture(t)
	Update(g.env("y\n", skillText))
	if !strings.Contains(g.out.String(), "giraffe will write its skill to") || len(Load(g.cfg).Entries) != 2 {
		t.Fatalf("out = %q entries = %v", g.out.String(), Load(g.cfg).Entries)
	}
}
