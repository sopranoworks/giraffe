package index

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/sopranoworks/giraffe/internal/scanner"
)

// encodeWalkCache renders w as one self-contained line per (directory,
// mtime) fact and one per file or subdirectory name it held that time —
// the same self-contained-line shape encodeEnumCache already uses for
// what is structurally the same kind of fact: a cache blob this package
// persists in the same store, decoded the same defensive way. Every
// line names its own directory rather than relying on a "most recently
// declared directory" the way a nested format would, so decode order
// never matters (§3 C5 is about output determinism, not input order,
// but a decoder that depended on line order would be a second,
// needless source of nondeterminism to reason about).
func encodeWalkCache(w scanner.WalkCache) []byte {
	dirs := make([]string, 0, len(w.Dirs))
	for d := range w.Dirs {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs) // §3 C5

	var buf strings.Builder
	for _, d := range dirs {
		e := w.Dirs[d]
		fmt.Fprintf(&buf, "D\t%s\t%d\n", sanitize(d), e.ModTime)
		files := append([]string(nil), e.Files...)
		sort.Strings(files)
		for _, f := range files {
			fmt.Fprintf(&buf, "F\t%s\t%s\n", sanitize(d), sanitize(f))
		}
		subdirs := append([]string(nil), e.Subdirs...)
		sort.Strings(subdirs)
		for _, sd := range subdirs {
			fmt.Fprintf(&buf, "S\t%s\t%s\n", sanitize(d), sanitize(sd))
		}
	}
	return []byte(buf.String())
}

// decodeWalkCache parses a walk-cache blob. Any shape it does not
// recognise is reported as ok=false, never an error — the caller's
// correct response is to walk cold, the same discard-and-rebuild
// convention the rest of the store follows (§23 C-14).
func decodeWalkCache(blob []byte) (scanner.WalkCache, bool) {
	w := scanner.WalkCache{Dirs: make(map[string]scanner.WalkDirEntry)}
	for _, line := range strings.Split(string(blob), "\n") {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) != 3 {
			return scanner.WalkCache{}, false
		}
		tag, dir, rest := fields[0], fields[1], fields[2]
		e := w.Dirs[dir]
		switch tag {
		case "D":
			mtime, err := strconv.ParseInt(rest, 10, 64)
			if err != nil {
				return scanner.WalkCache{}, false
			}
			e.ModTime = mtime
		case "F":
			e.Files = append(e.Files, rest)
		case "S":
			e.Subdirs = append(e.Subdirs, rest)
		default:
			return scanner.WalkCache{}, false
		}
		w.Dirs[dir] = e
	}
	return w, true
}
