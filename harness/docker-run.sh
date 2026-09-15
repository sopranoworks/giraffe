#!/usr/bin/env bash
# Directive 029 §2: stage a corpus fixture and the built giraffe binary
# outside this repository, then run a smoke-absence arm inside a
# container whose only bind mounts are that corpus, the built binary
# (aided arm only), and the credentials directory the SDK needs for
# auth. The giraffe repository itself is never mounted, so its source is
# not present in the container at all — the primary guard the directive
# asks for, not a pattern match against it.
#
# Usage: ./docker-run.sh <unaided|aided> <scratch-root>
#
# <scratch-root> is a directory outside this repository (the directive:
# "the corpus goes somewhere the source tree is not an ancestor or
# sibling"); this script does not default one, since a temp/scratch
# convention is host- and session-specific — pass your own.
set -euo pipefail

ARM="${1:?usage: docker-run.sh <unaided|aided> <scratch-root>}"
SCRATCH="${2:?usage: docker-run.sh <unaided|aided> <scratch-root>}"
HARNESS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$HARNESS_DIR/.." && pwd)"
REAL_REPO_PATH="$REPO_ROOT"

if [[ "$ARM" != "unaided" && "$ARM" != "aided" ]]; then
  echo "ARM must be 'unaided' or 'aided', got: $ARM" >&2
  exit 1
fi

CORPUS="$SCRATCH/corpus-$ARM"
mkdir -p "$CORPUS/runs"
echo "GUARD_MARKER_corpus-$ARM=$RANDOM$RANDOM" > "$CORPUS/marker.txt"
ln -sfn "$REAL_REPO_PATH" "$CORPUS/link-to-repo"

GIRAFFE_BIN_ARGS=()
if [[ "$ARM" == "aided" ]]; then
  mkdir -p "$CORPUS/bin"
  # Built once per invocation, from the real repo — the only step that
  # touches it. Nothing from the repo other than this one built binary
  # crosses into the corpus directory the container will see.
  (cd "$REPO_ROOT" && go build -o "$CORPUS/bin/giraffe" ./cmd/giraffe)
  GIRAFFE_BIN_ARGS=(-e "GIRAFFE_BIN=/corpus/bin/giraffe")
fi

docker run --rm \
  -v "$HARNESS_DIR":/harness:ro \
  -v "$CORPUS":/corpus \
  -v "$HOME/.claude":/root/.claude:ro \
  -e HOME=/root \
  -e CWD=/corpus \
  -e "RUN_ID=absence-$ARM-$(date +%s)" \
  -e OUT_DIR=/corpus/runs \
  -e "ARM=$ARM" \
  "${GIRAFFE_BIN_ARGS[@]}" \
  -w /harness \
  node:22-slim \
  node_modules/.bin/tsx src/smoke-absence.ts
