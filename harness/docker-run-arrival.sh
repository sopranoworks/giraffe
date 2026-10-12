#!/usr/bin/env bash
# Directive 030: stage the corpus once (shared read-only between both
# arms — one corpus, one commit, per the directive's own "one corpus at a
# time" constraint), build the giraffe binary once for the aided arm, and
# invoke run-arrival.ts inside a container for exactly one arm per
# invocation. Each invocation is a fresh container and a fresh `node`
# process (a fresh SDK query() call) — the "separate sessions" fix spec
# §28 C-37 asked for: run this twice, once per arm, and the aided arm's
# process has no way to see the unaided arm's output.
#
# Usage: ./docker-run-arrival.sh <unaided|aided> <scratch-root> [corpus-git-url]
#
# <scratch-root> is a directory outside this repository, same convention
# as docker-run.sh. The corpus is staged at <scratch-root>/<repository
# name> (the URL's last path element, .git stripped) and reused across
# invocations if it already exists there — delete it yourself to force a
# fresh clone (and a new pinned commit). The URL defaults to kubernetes
# (030); directive 041 made it a parameter so gekka runs through this
# same script rather than a second copy of it (040 kept a copy for one
# run and deleted it; a repeated measurement wants the parameter).
#
# TASK_TEXT, when set in the caller's environment, is passed through to
# run-arrival.ts and replaces 030's kubernetes task text — the only other
# thing that differs between a kubernetes run and a gekka one.
set -euo pipefail

ARM="${1:?usage: docker-run-arrival.sh <unaided|aided> <scratch-root> [corpus-git-url]}"
SCRATCH="${2:?usage: docker-run-arrival.sh <unaided|aided> <scratch-root> [corpus-git-url]}"
CORPUS_URL="${3:-https://github.com/kubernetes/kubernetes.git}"
HARNESS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
"$HARNESS_DIR/sync-skill.sh"  # the skill's one source is skill/SKILL.md (directive 087 §2)
REPO_ROOT="$(cd "$HARNESS_DIR/.." && pwd)"

if [[ "$ARM" != "unaided" && "$ARM" != "aided" ]]; then
  echo "ARM must be 'unaided' or 'aided', got: $ARM" >&2
  exit 1
fi

CORPUS_NAME="$(basename "$CORPUS_URL" .git)"
CORPUS="$SCRATCH/$CORPUS_NAME"
RUNS_DIR="$SCRATCH/runs"
mkdir -p "$RUNS_DIR"

if [[ ! -d "$CORPUS/.git" ]]; then
  git clone --depth 1 "$CORPUS_URL" "$CORPUS"
fi
COMMIT="$(git -C "$CORPUS" rev-parse HEAD)"
echo "corpus commit: $COMMIT" >&2

GIRAFFE_BIN_ARGS=()
MOUNT_ARGS=()
if [[ "$ARM" == "aided" ]]; then
  mkdir -p "$SCRATCH/bin"
  # Built once per invocation, from the real repo — the only step that
  # touches it. The corpus mount never contains it; it is mounted
  # separately, read-only.
  (cd "$REPO_ROOT" && go build -o "$SCRATCH/bin/giraffe" ./cmd/giraffe)
  MOUNT_ARGS+=(-v "$SCRATCH/bin":/giraffe-bin:ro)
  GIRAFFE_BIN_ARGS=(-e "GIRAFFE_BIN=/giraffe-bin/giraffe")
fi

TASK_ARGS=()
if [[ -n "${TASK_TEXT:-}" ]]; then
  TASK_ARGS=(-e "TASK_TEXT=$TASK_TEXT")
fi

docker run --rm \
  -v "$HARNESS_DIR":/harness:ro \
  -v "$CORPUS":/corpus \
  -v "$RUNS_DIR":/runs \
  "${MOUNT_ARGS[@]}" \
  -v "$HOME/.claude":/root/.claude:ro \
  -e HOME=/root \
  -e CWD=/corpus \
  -e "RUN_ID=arrival-$ARM-$(date +%s)" \
  -e OUT_DIR=/runs \
  -e "ARM=$ARM" \
  "${GIRAFFE_BIN_ARGS[@]}" \
  "${TASK_ARGS[@]}" \
  -w /harness \
  node:22-slim \
  bash -c 'apt-get update -qq && apt-get install -y -qq git >/dev/null && node_modules/.bin/tsx src/run-arrival.ts'
  # git is installed fresh in every container invocation, not baked into a
  # custom image: giraffe's own enumeration shells out to `git ls-files
  # -s` (a documented hard constraint, not something this directive is
  # free to change) and the base node:22-slim image does not carry git at
  # all. The first aided run against this script, before this line
  # existed, hit exactly that: `giraffe find` failed outright with "git:
  # executable file not found in $PATH" and the agent fell back to
  # unaided manual exploration mid-run — a harness/environment gap, not a
  # giraffe defect, and one this directive's own constraint against
  # changing giraffe does not cover. See the report.
  #
  # The corpus mount is read-write, not :ro: giraffe's cache directory
  # (internal/index/index.go: const Dir = ".giraffe") is hardcoded
  # relative to the project root, with no env override — it can only ever
  # write its index inside the corpus itself. A second run against a
  # read-only mount confirmed this fails outright too ("mkdir
  # /corpus/.giraffe: read-only file system"). The agent's own Bash
  # patterns for this arm (grep/find/ls/sed -n/cat/wc, plus the
  # giraffe binary itself) have no general write capability, so the only
  # thing that can be written into the corpus through this mount is
  # giraffe's own cache directory.
