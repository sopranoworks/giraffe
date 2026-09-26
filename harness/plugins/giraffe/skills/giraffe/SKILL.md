---
name: giraffe
description: An index of this repository's declarations, doc comments, paths and call sites — for any question about where something is or whether it exists: which file handles X, where Y is defined, what calls Z, is there a W feature. Just run `giraffe find <words>` in Bash (no need to load this skill first), with every word you would try in the one call — `giraffe find cache store registry`: it returns the matching declarations and files with exact line ranges, and a function's call sites. It answers each such lookup, not only the first: the next name you are after — a function a result mentions, what calls it, where a value is sent — is another `giraffe find`. A word no name, doc comment or path contains is looked for in the text of the files too — string literals, local variables, anything inside a body — and the answer names where it is, or says it is nowhere: a word reported nowhere is in no name, doc comment, path or source file of the whole repository. Use it before find, ls, grep or starting an Explore agent, and in any subagent exploring the code.
---

# giraffe

`giraffe` is an index of this repository. Run it through Bash:

    giraffe find <words>     e.g.  giraffe find metrics      giraffe find mailbox overflow
    giraffe prepare          builds or refreshes the index; a search does it itself when needed

Options go before `find` (`giraffe --page 2 find metrics`); every
argument after `find` is a word, as typed. Several words first list the
entries that carry two or more of them (more, and rarer, words first;
with how many entries each word matches), then answer each word.

When a word of the call names a directory the results lie in (a
segment of its path contains the word), that directory's own block
follows on the same page (`## results: 'in pkg/x'`), as `--query
path:pkg/x` would list it, so there is no need to narrow to it in a
second call.

Test files are indexed too; a result from one carries `test:true`.

Each result is one line:

    name:NodeMetrics kind:type path:internal/core/metrics.go line:16-60

`line:A-B` is the declaration's whole extent — read exactly that with
`sed -n 'A,Bp' <path>` (or Read with that offset and limit) instead of
opening the whole file. Under each result: its source line and the line
of its doc comment the words are on (else its first), usually enough to
judge it without opening it. A function or method also carries
`callers:` — the call sites that are its own (in its directory, or
qualified by its type or package; every site when the name is declared
once), one site a line as `file:line in Declaration: the call's own
line`, up to five and a count of the rest; sites of the same name under
other declarations are only counted; a call from a test file comes
after the code's. A field, variable or constant carries `uses:` the same
way — where its name is read, set, checked or its address taken without
a call; a name that is both called and used carries both. A result
named exactly one of the words also prints its text, under ## source.

An answer with no results says `none:` — no name, doc comment or path in
the index contains the word — and under it `text:`, where the word
occurs in the text of the indexed files (first site of each file), or
that it occurs in none of them.

Ask for the next page of the same answer with `giraffe --page 2 find <the same words>`.
