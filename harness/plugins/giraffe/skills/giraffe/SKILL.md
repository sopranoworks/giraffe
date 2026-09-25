---
name: giraffe
description: The first step for any question about where something is in this repository or whether it exists — which file handles X, where Y is defined, is there a Z feature. Just run `giraffe <words>` in Bash (no need to load this skill first), with every word you would try in the one call — `giraffe cache store registry`: it returns the matching declarations and files with exact line ranges. Use it before find, ls, grep or starting an Explore agent, and in any subagent exploring the code.
---

# giraffe

`giraffe` is an index of this repository. Run it through Bash:

    giraffe <words>          e.g.  giraffe metrics      giraffe "mailbox overflow"

Several words, or a quoted phrase, first list the entries that carry two
or more of them (more, and rarer, words first; with how many entries each
word matches), then answer each word.

Test files are indexed too; a result from one carries `test:true`.

Each result is one line:

    name:NodeMetrics kind:type path:internal/core/metrics.go line:16-60

`line:A-B` is the declaration's whole extent — read exactly that with
`sed -n 'A,Bp' <path>` (or Read with that offset and limit) instead of
opening the whole file. Under each result: its source line and the line
of its doc comment the words are on (else its first), usually enough to
judge it without opening it. A function or method also carries
`callers:` — where its name is called, as `file:line`, the sites
likeliest to be this declaration's first, up to five and a count of the
rest; a call from a test file comes after the code's. A result named
exactly one of the words also prints its text, under ## source.

An answer with no results says `none:` — no name, doc comment or path in
the index contains the word. Bodies are not indexed: grep only for text
inside functions (string literals, a local variable).

Ask for the next page of the same answer with `--page 2`.
