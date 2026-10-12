#!/usr/bin/env bash
# Directive 058 §2: one run of one configuration, in a fresh container
# against a fresh clone, with the Coder's own settings (see
# build-faithful-home.sh and src/run-faithful.ts).
#
# Usage: TASK_TEXT='...' ./docker-run-faithful.sh <A|B|C> <scratch-root> <home-dir> <pristine-clone>
#
#   A  no giraffe: the binary is not in the container.
#   B  giraffe at /usr/local/bin/giraffe, on PATH. Nothing else: no arm
#      writes the corpus's CLAUDE.md, whose owner is the repository (073).
#   C  B, and <home-dir> is a home built with --with-giraffe-skill.
#      The script does not check which home it was given; the run
#      record's contextSkills says what the listing held.
#
# Every run gets its own clone of <pristine-clone> (a clone of the
# corpus at its pinned commit, made once by the caller), because the
# preset's tools can write: a run that edits the corpus must not be the
# next run's corpus. The clone's origin is set back to the corpus's own
# URL, so it looks like the clone a developer has. It is mounted at
# /root/src/<name>, a path a developer's checkout could have, rather
# than 030's /corpus.
#
# The harness does not run giraffe: the clone has no .giraffe/ when the
# agent starts, and whatever builds the index is the agent's own call
# (directive 068; 067 §4 had the harness build it first).
#
# The home is mounted read-only, as run-arrival's ~/.claude always was;
# the host's .credentials.json is mounted over the empty one inside it,
# read-only. The host's Go toolchain is mounted read-only and put on
# PATH: the Coder's machine has one, and gekka is a Go module.
set -euo pipefail

CONFIG="${1:?usage: docker-run-faithful.sh <A|B|C> <scratch-root> <home-dir> <pristine-clone>}"
SCRATCH="${2:?usage}"
HOME_DIR="$(cd "${3:?usage}" && pwd)"
PRISTINE="$(cd "${4:?usage}" && pwd)"
HARNESS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
"$HARNESS_DIR/sync-skill.sh"  # the skill's one source is skill/SKILL.md (directive 087 §2)
REPO_ROOT="$(cd "$HARNESS_DIR/.." && pwd)"
: "${TASK_TEXT:?TASK_TEXT is required}"

case "$CONFIG" in A|B|C) ;; *) echo "CONFIG must be A, B or C, got: $CONFIG" >&2; exit 1 ;; esac

RUN_ID="faithful-$CONFIG-$(date +%s)"
NAME="$(basename "$PRISTINE")"
CORPUS="$SCRATCH/corpora/$RUN_ID/$NAME"
RUNS_DIR="$SCRATCH/runs"
mkdir -p "$RUNS_DIR" "$(dirname "$CORPUS")"

git clone -q "$PRISTINE" "$CORPUS"
git -C "$CORPUS" remote set-url origin "$(git -C "$PRISTINE" remote get-url origin)"
echo "corpus commit: $(git -C "$CORPUS" rev-parse HEAD)" >&2

GIRAFFE_ARGS=()
if [[ "$CONFIG" != "A" ]]; then
  mkdir -p "$SCRATCH/bin"
  (cd "$REPO_ROOT" && go build -o "$SCRATCH/bin/giraffe" ./cmd/giraffe)
  GIRAFFE_ARGS=(-v "$SCRATCH/bin/giraffe":/usr/local/bin/giraffe:ro)
fi

# Debian's GOROOT (/usr/lib/go-1.26) is half symlinks into
# /usr/share/go-1.26, so both are mounted at their own paths.
GOROOT_HOST="$(go env GOROOT)"
GOSHARE_HOST="$(cd "$GOROOT_HOST/src" && pwd -P | xargs dirname)"
docker run --rm \
  -v "$HARNESS_DIR":/harness:ro \
  -v "$CORPUS":"/root/src/$NAME" \
  -v "$RUNS_DIR":/runs \
  -v "$HOME_DIR":/root/.claude:ro \
  -v "$HOME/.claude/.credentials.json":/root/.claude/.credentials.json:ro \
  -v "$GOROOT_HOST":"$GOROOT_HOST":ro \
  -v "$GOSHARE_HOST":"$GOSHARE_HOST":ro \
  "${GIRAFFE_ARGS[@]}" \
  -e HOME=/root \
  -e "PATH=$GOROOT_HOST/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" \
  -e "CWD=/root/src/$NAME" \
  -e "RUN_ID=$RUN_ID" \
  -e OUT_DIR=/runs \
  -e "CONFIG=$CONFIG" \
  -e "TASK_TEXT=$TASK_TEXT" \
  ${MODEL:+-e "MODEL=$MODEL"} \
  -w /harness \
  node:22-slim \
  bash -c 'apt-get update -qq && apt-get install -y -qq git python3 jq >/dev/null && git config --global --add safe.directory "*" && node_modules/.bin/tsx src/run-faithful.ts'
