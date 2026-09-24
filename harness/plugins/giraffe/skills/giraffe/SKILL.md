---
name: giraffe
description: The first step for any question about where something is in this repository or whether it exists — which file handles X, where Y is defined, is there a Z feature. Just run `giraffe <words>` in Bash (no need to load this skill first), with every word you would try in the one call — `giraffe cache store registry`: it returns the matching declarations and files with exact line ranges. Use it before find, ls, grep or starting an Explore agent, and in any subagent exploring the code.
---

# giraffe

`giraffe` is an index of this repository. Run it through Bash:

    giraffe <words>          e.g.  giraffe metrics      giraffe "mailbox overflow"

Several words, or a quoted phrase, answer each word and first list the
entries that carry all of them.

Each result is one line:

    name:NodeMetrics kind:type path:internal/core/metrics.go line:16-60

`line:A-B` is the declaration's whole extent — read exactly that with
`sed -n 'A,Bp' <path>` (or Read with that offset and limit) instead of
opening the whole file. Under each result: its source line and the first
line of its doc comment, usually enough to judge it without opening it.

An answer with no results says `none:` — no name, doc line or path in the
index contains the word. Bodies are not indexed: grep only for text
inside functions (call sites, string literals).

Ask for the next page of the same answer with `--page 2`.
