#!/usr/bin/env bash
# Directive 041 §1: one measurement round — N runs of each arm, same
# task, same corpus commit, same isolation, one container at a time.
# 040 was one run of each arm and 030/032 (identical setups) differed
# 9.6× in tokens, so a single pair cannot tell a real difference from
# noise; five pairs give a median and a range. The arms alternate
# (unaided, aided, unaided, ...) so whatever drifts over the round's
# wall-clock — API latency, model-side variance — lands on both arms
# equally rather than on whichever arm ran last.
#
# Sequenced, never parallel: 021 exhausted a RAM-backed /tmp holding four
# clones at once, and the directive names that. One corpus clone is
# shared across every run (docker-run-arrival.sh stages it once).
#
# Usage: TASK_TEXT='...' [ARMS='aided'] ./docker-run-arrival-round.sh <pairs> <scratch-root> [corpus-git-url]
#
# ARMS names the arms each pair runs, in order; unset, it is both
# ("unaided aided"), 041's alternation. Directive 044 fixed the unaided
# arm's median from 041's twenty runs as the denominator — nothing
# giraffe does can reach that arm — so its rounds run the aided arm
# alone, twenty times, and ARMS='aided' is how. The word "pair" then
# counts one run.
#
# Each run's own stdout/stderr goes to <scratch-root>/logs/<run>.log; the
# transcripts and run records land in <scratch-root>/runs as always.
set -euo pipefail

PAIRS="${1:?usage: docker-run-arrival-round.sh <pairs> <scratch-root> [corpus-git-url]}"
SCRATCH="${2:?usage: docker-run-arrival-round.sh <pairs> <scratch-root> [corpus-git-url]}"
CORPUS_URL="${3:-https://github.com/kubernetes/kubernetes.git}"
ARMS="${ARMS:-unaided aided}"
HARNESS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

mkdir -p "$SCRATCH/logs"
for ((i = 1; i <= PAIRS; i++)); do
  for arm in $ARMS; do
    stamp="$(date +%s)"
    echo "pair $i/$PAIRS: $arm ($stamp)" >&2
    "$HARNESS_DIR/docker-run-arrival.sh" "$arm" "$SCRATCH" "$CORPUS_URL" \
      > "$SCRATCH/logs/$arm-$stamp.log" 2>&1 \
      || echo "pair $i $arm failed, see $SCRATCH/logs/$arm-$stamp.log" >&2
  done
done
