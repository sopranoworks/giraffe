# giraffe

## What giraffe is

giraffe is a code search tool for coding agents. In a large codebase it points an agent at the files and declarations it needs, so the agent spends fewer tokens and fewer round trips finding them. It is a search, not a duplicate checker: the agent asks `giraffe find <words>` and gets back matching declarations and files with exact line ranges, and a function's call sites.

## Status at v0.1.0

Implemented:

- `giraffe find <words>` over declaration names, doc comments, paths and call sites, tests included. A word that no name, doc comment or path contains is looked up in the text of the files, and the answer says where it is, or that it is nowhere.
- `--query` to narrow a search by attribute (`path:`, `project:`, `callers:`, `calls:` and others that result lines print), and `--page` for the next page.
- `giraffe prepare` to build or refresh the index; a search builds or refreshes it by itself when needed. A refresh re-reads only what changed.
- Repositories made of several Go modules, and extra roots listed in `.giraffe/roots`.
- `giraffe install` and `giraffe update`, which put giraffe's skill where coding agents read it.
- A local usage log (off with one setting).

**Go is the only language with a declaration extractor.** Files in any other language are indexed as files only: a search can find them by path and by the text in them, but giraffe does not read their declarations.

## Install

    go install github.com/sopranoworks/giraffe/cmd/giraffe@latest

This needs a Go toolchain; the binary lands in `$(go env GOPATH)/bin`, which should be on your `PATH`.

## Install the skill

Agents learn about giraffe from its skill, so the skill has to be installed. The skill is built into the `giraffe` binary.

    giraffe install                      every agent below, global; lists where it will write and asks y/n
    giraffe install --agent claude       one agent
    giraffe install --local              for this repository only (the repository root, from git)
    giraffe install --global             for every repository (the default)
    giraffe install --path <dir>         write <dir>/SKILL.md, for an agent not listed
    giraffe update                       refresh the skill wherever giraffe installed it

With no `--agent` and no `--path`, `giraffe install` writes for every agent, after listing the files it will write and asking y/n. If a skill is already at a destination, giraffe shows it and asks before overwriting. `--global` and `--local` conflict with each other, and neither goes with `--path`; `--path` does not go with `--agent`.

Built-in agents, and where the skill goes (the file is always `SKILL.md` in a directory called `giraffe`):

| `--agent` | `--global` (default) | `--local` |
|---|---|---|
| `claude` | `~/.claude/skills/giraffe/SKILL.md` | `<repository root>/.claude/skills/giraffe/SKILL.md` |
| `codex` | `~/.agents/skills/giraffe/SKILL.md` | `<repository root>/.agents/skills/giraffe/SKILL.md` |

Codex's destinations are from its documentation of skills (<https://developers.openai.com/codex/skills>): `$HOME/.agents/skills` for a user, `$REPO_ROOT/.agents/skills` for a repository.

giraffe keeps a list of what it installed, with a content hash of each file, in `~/.config/giraffe/installs.json`. `giraffe update` drops entries whose file is gone (a deleted skill is never recreated), lists what it will rewrite and the files you changed by hand separately, and asks once. It also looks in every agent's destinations for a giraffe skill that is not on the list and offers to add it. `giraffe`, `giraffe --help` and `giraffe prepare` say in one line when an installed skill is out of date.

giraffe never writes `AGENTS.md`, `CLAUDE.md` or any other instruction file of yours.

### Other agents

Add an agent in `~/.config/giraffe/agents` (`$XDG_CONFIG_HOME/giraffe/agents` if that variable is set). One agent per line: a name, the directory that receives `SKILL.md` for a global install, and the same for a local install, relative to the repository root. Put a path with spaces in double quotes; `#` starts a comment.

    # name     global                      local
    myagent    ~/.myagent/skills/giraffe   .myagent/skills/giraffe

Then `giraffe install --agent myagent` works, and `giraffe install` with no options includes it. A configured agent named `claude` or `codex` replaces that built-in's destinations.

## Use

The agent runs `giraffe find <words>`; you rarely type it yourself.

    giraffe find cluster metrics actor count
    giraffe --query 'path:internal/ project:cluster' find Snapshot
    giraffe --page 2 find cluster metrics actor count

- The first search builds the index. It creates a `.giraffe/` working directory at the repository root, and that directory ignores itself: giraffe writes `.giraffe/.gitignore` containing `*`, so nothing in it reaches `git status`.
- `giraffe prepare` builds or refreshes the index ahead of time, so the first search is not the one that pays for it.
- `--query` and `--page` go before `find`. Everything after `find` is a search word, never an option.

`giraffe --help` has the rest.

## Usage log

During this early stage giraffe writes a usage log, by default, to `~/.config/giraffe/log/` (`$XDG_CONFIG_HOME/giraffe/log/` if that variable is set): one file per day, one JSON line per record, a start record and an end record for each invocation. It records the command and its arguments (your search words), the repository root, how long it took, how many results it returned, the agent's session identifier from the environment, and how a failed command failed. A record is about 220 bytes. **The log stays on your machine; giraffe sends it nowhere.** It never changes what giraffe prints and a failure to write it never fails a command.

To turn it off, put this line in `~/.config/giraffe/config`:

    log = off

## Measured results

Each figure is a count of tokens for one run of a coding agent answering one question about a codebase, with giraffe and without it.

Conditions, the same in every row:

- Model: Sonnet (`claude-sonnet-5`), with Claude Code's own system prompt and tool set.
- With giraffe: the skill installed, and no `CLAUDE.md` line about giraffe. Without giraffe: the same agent, no giraffe.
- Tokens are counted per run, over the main agent and its subagents: input, cache reads, cache creation and output, every assistant message once.
- The two arms ran interleaved, in the same hour.
- Every run is counted. Median is the middle run (the mean of the middle two when the count is even). P80 is the run at nearest rank ⌈0.8 n⌉.

One round per corpus, not the best of several. gekka, TiDB and Kubernetes are from the round of 2026-10-10 that compared the shipped result display (10 runs a cell and arm). The others are from the round of 2026-10-09 that tried several other Go projects (5 runs a cell and arm); grpc-go and etcd are from the follow-up round that repeated those questions after giraffe learned to index repositories with several same-named Go modules, because in the first round giraffe could not index either of them.

| Corpus | Question, in a few words | Median, giraffe as % of without | P80, giraffe as % of without | Runs per arm |
|---|---|---:|---:|---:|
| gekka | where to add an actor count to the cluster's metrics | 22.6% | 25.9% | 10 |
| gekka | which file says what happens when a Classic Actor's mailbox overflows | 30.7% | 32.2% | 10 |
| gekka | where to start on signing e-mail | 27.5% | 25.0% | 10 |
| TiDB | where to add a "log instead of exit" option for an idle standby server | 16.3% | 12.6% | 10 |
| TiDB | follow a subtask failing on a full TiKV disk | 64.3% | 54.6% | 10 |
| TiDB | can workload-based read-cost analysis be started on demand | 46.6% | 49.7% | 10 |
| Kubernetes | where to start on prefixing a Pod's name with its start time | 73.8% | 78.5% | 10 |
| k9s | follow a force delete from the dialog to the Kubernetes call | 28.6% | 27.6% | 5 |
| moby | follow an unhealthy container's health check to its event and its fate | 69.6% | 72.0% | 5 |

Notes on what came out level or worse:

- **When the model already knows a codebase from its training, giraffe may not lower the cost.** What is required there is that it does not raise it much, and a search can still guard against knowledge that has gone out of date. etcd: 90.4% at the median and 82.7% at P80 (5 runs per arm). grpc-go: 94.8% and 86.5% (5 runs per arm; one of the five giraffe runs never called giraffe, and without it the median is 98.0% over 4 runs). In both, the runs without giraffe already name the right place in their first search, and the two arms' ranges overlap (etcd 102,364–538,489 without, 150,379–326,746 with; grpc-go 258,920–481,126 without, 190,500–369,110 with).

Ten runs a cell is few, and a figure here moves by tens of percent from run to run: read these as an indication, not as a measurement of a rate.

## License

This project is licensed under the **MIT License** — see [LICENSE](LICENSE) for the full text.
