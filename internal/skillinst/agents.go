// Package skillinst installs and updates giraffe's skill for the coding
// agents that read one (directive 087). It writes the skill's own file,
// <dir>/SKILL.md, and nothing else: never AGENTS.md, CLAUDE.md or any
// other instruction file of the person's.
package skillinst

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Scope is where an agent reads skills from: for every repository the
// person works in, or for one.
type Scope int

const (
	Global Scope = iota
	Local
)

// Agent is a coding agent that reads skills. Global and Local are the
// directories that hold its giraffe skill's SKILL.md: Global as the
// person's home sees it ("~/" is the home directory), Local relative to
// the repository root.
type Agent struct {
	Name   string
	Global string
	Local  string
}

// Builtin is the agents giraffe knows without being told: claude and
// codex, nothing else. Claude Code reads ~/.claude/skills/<name>/ and
// <repository>/.claude/skills/<name>/. Codex reads
// $HOME/.agents/skills and, from the working directory up to the
// repository root, .agents/skills (https://learn.chatgpt.com/docs/build-skills,
// formerly developers.openai.com/codex/skills).
func Builtin() []Agent {
	return []Agent{
		{Name: "claude", Global: "~/.claude/skills/giraffe", Local: ".claude/skills/giraffe"},
		{Name: "codex", Global: "~/.agents/skills/giraffe", Local: ".agents/skills/giraffe"},
	}
}

// Agents is the built-in agents with the ones configured in
// <cfgDir>/agents added. A configured agent with a built-in's name
// replaces that built-in's destinations (and keeps its place in the
// order); the others follow in file order.
//
// The file has one agent per line — a name, its global destination, its
// local destination — separated by spaces or tabs; a path with spaces
// is wrapped in double quotes; # starts a comment:
//
//	# name    global destination         local destination (from the repository root)
//	cursor    ~/.cursor/skills/giraffe   .cursor/skills/giraffe
//
// A missing file is no configured agents.
func Agents(cfgDir string) ([]Agent, error) {
	out := Builtin()
	if cfgDir == "" {
		return out, nil
	}
	f, err := os.Open(filepath.Join(cfgDir, "agents"))
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Join(cfgDir, "agents"), err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields, err := splitFields(line)
		if err != nil || len(fields) != 3 {
			return nil, fmt.Errorf("%s:%d: want: <name> <global destination> <local destination>", filepath.Join(cfgDir, "agents"), n)
		}
		a := Agent{Name: fields[0], Global: fields[1], Local: fields[2]}
		if !validName(a.Name) {
			return nil, fmt.Errorf("%s:%d: agent name %q: letters, digits, - and _ only", filepath.Join(cfgDir, "agents"), n, a.Name)
		}
		if !(strings.HasPrefix(a.Global, "~/") || filepath.IsAbs(a.Global)) {
			return nil, fmt.Errorf("%s:%d: global destination %q must be absolute or start with ~/", filepath.Join(cfgDir, "agents"), n, a.Global)
		}
		if filepath.IsAbs(a.Local) || a.Local == "" || strings.HasPrefix(filepath.Clean(a.Local), "..") {
			return nil, fmt.Errorf("%s:%d: local destination %q must be relative to the repository root", filepath.Join(cfgDir, "agents"), n, a.Local)
		}
		replaced := false
		for i := range out {
			if out[i].Name == a.Name {
				out[i], replaced = a, true
			}
		}
		if !replaced {
			out = append(out, a)
		}
	}
	return out, sc.Err()
}

func validName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r == '-' || r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return true
}

// splitFields splits line on spaces and tabs, a double-quoted field
// keeping its spaces.
func splitFields(line string) ([]string, error) {
	var out []string
	for i := 0; i < len(line); {
		switch c := line[i]; {
		case c == ' ' || c == '\t':
			i++
		case c == '"':
			j := strings.IndexByte(line[i+1:], '"')
			if j < 0 {
				return nil, fmt.Errorf("unterminated quote")
			}
			out = append(out, line[i+1:i+1+j])
			i += j + 2
		default:
			j := i
			for j < len(line) && line[j] != ' ' && line[j] != '\t' {
				j++
			}
			out = append(out, line[i:j])
			i = j
		}
	}
	return out, nil
}

// Find returns the agent called name.
func Find(agents []Agent, name string) (Agent, bool) {
	for _, a := range agents {
		if a.Name == name {
			return a, true
		}
	}
	return Agent{}, false
}

// Names lists the agents' names, for a message.
func Names(agents []Agent) string {
	var n []string
	for _, a := range agents {
		n = append(n, a.Name)
	}
	return strings.Join(n, ", ")
}

// Dir is the directory the agent's skill goes in at scope: absolute.
// home is the person's home directory; repo the repository root, used
// by Local only.
func (a Agent) Dir(s Scope, home, repo string) (string, error) {
	if s == Local {
		if repo == "" {
			return "", fmt.Errorf("--local needs a repository, and this is not inside one")
		}
		return filepath.Join(repo, filepath.FromSlash(a.Local)), nil
	}
	g := a.Global
	if strings.HasPrefix(g, "~/") {
		if home == "" {
			return "", fmt.Errorf("no home directory for %s", g)
		}
		g = filepath.Join(home, g[2:])
	}
	return filepath.Clean(g), nil
}
