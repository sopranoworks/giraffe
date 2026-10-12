#!/usr/bin/env bash
# Directive 087 §2: the skill's one source is skill/SKILL.md at the
# repository root (embedded in the binary). The harness's plugin
# directory gets a copy of that same file, never an edit of its own;
# the copy is untracked. The scripts that stage a run call this first.
set -euo pipefail
HARNESS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEST="$HARNESS_DIR/plugins/giraffe/skills/giraffe"
mkdir -p "$DEST"
cp "$HARNESS_DIR/../skill/SKILL.md" "$DEST/SKILL.md"
