#!/usr/bin/env bash
# Directive 058 §1: a curated copy of the Coder's ~/.claude, for a
# container to mount at /root/.claude, so a run's settings, rules, agents
# and skill listing are the Coder's own rather than 029's empty set.
#
# Usage: ./build-faithful-home.sh <out-dir> [--with-giraffe-skill]
#
# Kept, copied as they are on the host:
#   skills/        user skills and the account-synced ones
#   plugins/       installed_plugins.json, the plugin cache, the synced
#                  plugins, the marketplace clone (host paths rewritten
#                  to /root/.claude)
#   rules/         user rules (context7.md)
#   agents/        user subagents (go-coder.md)
#   settings.json  without hooks and statusLine (below)
# Dropped:
#   hooks          every PreToolUse hook names a script by a host path
#                  (/home/takahashi/.claude/hooks/...) and guards the
#                  Coder's own repositories — git add -A, destructive git,
#                  wide go test, pkill of shoka, .git files. None has
#                  anything to act on in a disposable clone.
#   statusLine     a host node binary, and interactive only.
#   projects/, history, sessions, file-history, caches, logs — the
#                  Coder's own state, not configuration. Auto-memory
#                  for the corpus's path is empty either way.
#   .credentials.json is written empty: docker-run-faithful.sh mounts
#                  the host's own file over it, read-only, and never
#                  copies it.
#
# --with-giraffe-skill installs harness/plugins/giraffe/skills/giraffe as
# a user skill, ~/.claude/skills/giraffe, the way a skill is installed —
# one among the rest (configuration C).
set -euo pipefail

OUT="${1:?usage: build-faithful-home.sh <out-dir> [--with-giraffe-skill]}"
WITH_GIRAFFE="${2:-}"
SRC="$HOME/.claude"
HARNESS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if [[ -e "$OUT" ]]; then
  echo "refusing to overwrite $OUT" >&2
  exit 1
fi
mkdir -p "$OUT/plugins"

cp -a "$SRC/skills" "$SRC/rules" "$SRC/agents" "$OUT/"
for p in installed_plugins.json known_marketplaces.json cache synced marketplaces; do
  cp -a "$SRC/plugins/$p" "$OUT/plugins/"
done
sed -i "s#$SRC#/root/.claude#g" "$OUT/plugins/installed_plugins.json" "$OUT/plugins/known_marketplaces.json"

python3 - "$SRC/settings.json" "$OUT/settings.json" <<'EOF'
import json, sys
s = json.load(open(sys.argv[1]))
for k in ("hooks", "statusLine"):
    s.pop(k, None)
json.dump(s, open(sys.argv[2], "w"), indent=2)
EOF

: > "$OUT/.credentials.json"

if [[ "$WITH_GIRAFFE" == "--with-giraffe-skill" ]]; then
  cp -a "$HARNESS_DIR/plugins/giraffe/skills/giraffe" "$OUT/skills/giraffe"
fi
