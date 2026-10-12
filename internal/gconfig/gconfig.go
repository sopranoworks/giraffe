// Package gconfig is giraffe's global configuration directory, the one
// place a person's own settings live beside every repository they use
// (directive 087): $XDG_CONFIG_HOME/giraffe, or ~/.config/giraffe when
// XDG_CONFIG_HOME is unset. It holds the extensions file (directive
// 023), agents (a person's own coding agents, for `giraffe install`),
// installs.json (where giraffe put the skill), config (settings) and
// log/ (the usage log).
package gconfig

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// Dir is the global configuration directory — "" if neither
// $XDG_CONFIG_HOME nor $HOME can be resolved.
func Dir() string {
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return filepath.Join(v, "giraffe")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "giraffe")
}

// Settings are the values of the `config` file.
type Settings struct {
	// Log is whether every invocation appends to the usage log (directive
	// 087 §10). On unless `log = off` is set.
	Log bool
}

// Load reads <dir>/config: one `key = value` per line, # starts a
// comment, unknown keys are ignored. A missing or unreadable file is
// the defaults — a setting that cannot be read never fails a command.
func Load(dir string) Settings {
	s := Settings{Log: true}
	if dir == "" {
		return s
	}
	f, err := os.Open(filepath.Join(dir, "config"))
	if err != nil {
		return s
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key, val = strings.TrimSpace(key), strings.ToLower(strings.TrimSpace(val))
		if key == "log" {
			s.Log = !(val == "off" || val == "false" || val == "no" || val == "0")
		}
	}
	return s
}
