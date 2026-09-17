---
name: giraffe
description: How to search this codebase with the giraffe command — choosing query words, narrowing a truncated answer, judging what a miss means, and feeding vocabulary back. Use this skill whenever you need to know whether something already exists, before writing a new file, type, or exported function, and whenever a search comes back empty or too large. Read it before the first giraffe call in a task.
---

# Searching with giraffe

`giraffe` indexes this repository's declarations and files and answers
with a small, bounded result set.

**This skill does not document the command's syntax.** Flags, attribute
names, and subcommands come from `giraffe find --help` and from the
output itself. Anything restated here would be a second copy that goes
stale the moment the command changes — the exact duplication this
discipline exists to prevent. What follows is how to search well, which
`--help` cannot tell you.

## 1. What kind of answer this is

A **finder, not an oracle.** Results are places that might be relevant.
You then open them and decide.

The costs are asymmetric and should shape your behaviour:

- a false positive costs one look at a declaration — its range, or the
  text the answer already printed
- a false negative costs a duplicate implementation, permanently

So lean toward opening things. "Probably not it" is not a conclusion
you can reach from an index line.

An empty answer is weak evidence. It means *these words* found nothing,
not that nothing exists.

## 2. Choosing words

The index cannot know which word you would have chosen. Someone named
it before you, and they named it differently.

Run **two or three queries, not one**:

1. the concept noun — what the thing is
2. a synonym — `fetch`/`get`/`load`/`retrieve`, `job`/`task`/`work`,
   `store`/`repository`/`registry`/`cache`
3. the verb of the operation

Search the *capability*, not your intended class name. You are looking
for "bounded retry with backoff", not for `RetryPolicyManager`.

Start with no narrowing at all. Narrow only in response to a result,
never in anticipation of one.

## 3. Reading the answer

The index line reports the state of the index. Paste it in your report
verbatim; it is the evidence the search ran.

Warnings matter. A file the index could not parse has no declarations
in it, so an empty result is weaker evidence than usual. Check before
concluding anything from silence.

**Truncation means narrow the question. It never means you have seen
everything.** A truncated answer says where the rest is — use it: copy
one of the places it offers into the query and search again.

**Never page through a truncated answer and search it yourself.** A
large result set you then grep is the state this command replaces. If
you find yourself scanning output, you skipped a narrowing step.

## 4. Narrowing

Attributes in a result line are exactly what the query accepts. Copy
them; do not invent them. An unknown attribute is an error, not a
filter that quietly does nothing.

What they select:

- `kind:` the sort of declaration — a type, a func, a method, a field
- `scope:` exported, or package-private
- `type:` for a field or method, the type it belongs to — so
  `type:Options` is every member of that type
- `receiver:` for a method, the type it hangs off — that type's methods
- `line:` the declaration's whole extent, to read exactly it

`--query 'name:Prefix type:Options'` is the shape of "the member of
this type called that", and it answers in one call what a search and a
read would.

Narrow by what you actually know. Knowing the area is different from
knowing the name — if you know where it lives, say so and the answer
gets short; if you only know roughly what it does, stay wide and use
better words instead. A place is a directory, not a file you have
already seen: copying a result's whole path narrows to that one file.
