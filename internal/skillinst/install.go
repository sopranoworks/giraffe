package skillinst

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// File is the skill's file name in every destination.
const File = "SKILL.md"

// Env is what install and update work with: the skill to write, where
// the global configuration lives, who the person is, and the terminal.
type Env struct {
	Skill   []byte
	CfgDir  string
	Home    string
	RepoDir func() string // the repository root of the current directory, "" outside one
	In      *bufio.Reader
	Out     io.Writer
	Err     io.Writer
}

// Options are install's: what was given, as given. Scope and Where are
// told apart from their defaults so a conflict is one that was typed.
type Options struct {
	Global, Local bool
	Agent         string
	Path          string
	HasAgent      bool
	HasPath       bool
}

// ParseOptions reads the arguments after `install`.
func ParseOptions(args []string) (Options, error) {
	var o Options
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--global":
			o.Global = true
		case "--local":
			o.Local = true
		case "--agent", "--path":
			if i+1 >= len(args) {
				return o, fmt.Errorf("%s takes a value", args[i])
			}
			if args[i] == "--agent" {
				if o.HasAgent {
					return o, fmt.Errorf("--agent given twice; one agent, or none for every agent")
				}
				o.Agent, o.HasAgent = args[i+1], true
			} else {
				if o.HasPath {
					return o, fmt.Errorf("--path given twice")
				}
				o.Path, o.HasPath = args[i+1], true
			}
			i++
		default:
			return o, fmt.Errorf("install takes --global | --local, --agent <name>, --path <dir>; not %q", args[i])
		}
	}
	return o, nil
}

// Validate rejects the combinations that contradict each other.
func (o Options) Validate(agents []Agent) error {
	switch {
	case o.Global && o.Local:
		return fmt.Errorf("--global and --local cannot be given together")
	case o.HasPath && o.HasAgent:
		return fmt.Errorf("--path and --agent cannot be given together")
	case o.HasPath && (o.Global || o.Local):
		return fmt.Errorf("--path cannot be combined with --global or --local")
	case o.HasAgent:
		if _, ok := Find(agents, o.Agent); !ok {
			return fmt.Errorf("unknown agent %q; known: %s", o.Agent, Names(agents))
		}
	}
	return nil
}

func (e *Env) printf(format string, a ...any) { fmt.Fprintf(e.Out, format, a...) }

// fail prints one line to the error stream.
func (e *Env) fail(format string, a ...any) { fmt.Fprintf(e.Err, "giraffe: "+format+"\n", a...) }

// ask prints prompt and reads y or n. Anything but y/yes — including
// the end of input — is no.
func (e *Env) ask(prompt string) bool {
	e.printf("%s [y/n] ", prompt)
	line, _ := e.In.ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	if line == "" {
		e.printf("\n")
	}
	return line == "y" || line == "yes"
}

// targets lists the SKILL.md files an install writes, each once.
func targets(o Options, agents []Agent, home, repo string) ([]string, error) {
	scope := Global
	if o.Local {
		scope = Local
	}
	var dirs []string
	switch {
	case o.HasPath:
		abs, err := filepath.Abs(o.Path)
		if err != nil {
			return nil, err
		}
		dirs = []string{abs}
	case o.HasAgent:
		a, _ := Find(agents, o.Agent)
		d, err := a.Dir(scope, home, repo)
		if err != nil {
			return nil, err
		}
		dirs = []string{d}
	default:
		for _, a := range agents {
			d, err := a.Dir(scope, home, repo)
			if err != nil {
				return nil, err
			}
			dirs = append(dirs, d)
		}
	}
	var out []string
	seen := map[string]bool{}
	for _, d := range dirs {
		p := filepath.Join(d, File)
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out, nil
}

// Install writes the skill where o says, asking where the choice is
// giraffe's (every agent) or an existing skill would be overwritten,
// and records each destination it wrote. It returns the exit code.
func Install(env *Env, o Options) int {
	agents, err := Agents(env.CfgDir)
	if err != nil {
		env.fail("%v", err)
		return 1
	}
	if err := o.Validate(agents); err != nil {
		env.fail("%v", err)
		return 2
	}
	repo := ""
	if o.Local {
		if repo = env.RepoDir(); repo == "" {
			env.fail("--local needs a repository, and this is not inside one")
			return 2
		}
	}
	files, err := targets(o, agents, env.Home, repo)
	if err != nil {
		env.fail("%v", err)
		return 2
	}
	if !o.HasAgent && !o.HasPath {
		env.printf("giraffe will write its skill to:\n")
		for _, f := range files {
			env.printf("  %s\n", f)
		}
		if !env.ask("Install?") {
			env.printf("nothing written\n")
			return 0
		}
	}
	return env.write(files, true)
}

// write puts the skill at each file, asking first about one that is
// already there when askExisting, and records what it wrote.
func (env *Env) write(files []string, askExisting bool) int {
	list := Load(env.CfgDir)
	code := 0
	wrote := 0
	for _, f := range files {
		if _, err := os.Stat(f); err == nil && askExisting {
			if !env.ask(fmt.Sprintf("%s already holds a skill. Overwrite?", f)) {
				env.printf("kept %s\n", f)
				continue
			}
		}
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err == nil {
			err = writeFile(f, env.Skill)
			if err == nil {
				list.Set(f, env.Skill)
				env.printf("wrote %s\n", f)
				wrote++
				continue
			}
		}
		env.fail("write %s failed", f)
		code = 1
	}
	if wrote > 0 {
		if err := list.Save(); err != nil {
			env.fail("the install list could not be written: %v", err)
			code = 1
		}
	}
	if wrote == 0 && code == 0 {
		env.printf("nothing written\n")
	}
	return code
}

// isGiraffeSkill reports whether b is a skill whose frontmatter says
// name: giraffe.
func isGiraffeSkill(b []byte) bool {
	lines := strings.Split(string(bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return false
	}
	for _, l := range lines[1:] {
		l = strings.TrimSpace(l)
		if l == "---" {
			return false
		}
		if k, v, ok := strings.Cut(l, ":"); ok && strings.TrimSpace(k) == "name" {
			return strings.Trim(strings.TrimSpace(v), `"'`) == "giraffe"
		}
	}
	return false
}

// strays are the giraffe skills in an agent's own global and local
// destinations that the list does not have: installed by hand, or moved
// there.
func (env *Env) strays(agents []Agent, list *List, repo string) []string {
	var out []string
	seen := map[string]bool{}
	for _, a := range agents {
		for _, s := range []Scope{Global, Local} {
			if s == Local && repo == "" {
				continue
			}
			d, err := a.Dir(s, env.Home, repo)
			if err != nil {
				continue
			}
			p := filepath.Join(d, File)
			if seen[p] || list.Has(p) {
				continue
			}
			seen[p] = true
			if b, err := os.ReadFile(p); err == nil && isGiraffeSkill(b) {
				out = append(out, p)
			}
		}
	}
	return out
}

// Update rewrites the skill at every install in the list, asking once
// before it writes, and a second time about a giraffe skill an agent
// has that the list does not. With nothing in the list it does what
// install with no options does. It returns the exit code.
func Update(env *Env) int {
	agents, err := Agents(env.CfgDir)
	if err != nil {
		env.fail("%v", err)
		return 1
	}
	list := Load(env.CfgDir)
	code := 0

	var plain, hand []string
	for _, e := range list.Entries {
		if st := StateOf(e, env.Skill); st == Hand {
			hand = append(hand, e.Path)
		} else {
			plain = append(plain, e.Path+" ("+st.String()+")")
		}
	}
	if len(list.Entries) > 0 {
		env.printf("giraffe will rewrite its skill at:\n")
		for _, p := range plain {
			env.printf("  %s\n", p)
		}
		if len(hand) > 0 {
			env.printf("changed by hand, and overwritten too:\n")
			for _, p := range hand {
				env.printf("  %s\n", p)
			}
		}
		if env.ask("Update?") {
			var files []string
			for _, e := range list.Entries {
				files = append(files, e.Path)
			}
			if c := env.write(files, false); c != 0 {
				code = c
			}
		} else {
			env.printf("nothing written\n")
		}
	}

	stray := env.strays(agents, list, env.RepoDir())
	taken := 0
	if len(stray) > 0 {
		env.printf("a giraffe skill the install list does not have:\n")
		for _, p := range stray {
			env.printf("  %s\n", p)
		}
		if env.ask("Take them into the list and update them?") {
			if c := env.write(stray, false); c != 0 {
				code = c
			}
			taken = len(stray)
		}
	}

	if len(list.Entries) == 0 && taken == 0 {
		return Install(env, Options{})
	}
	return code
}
