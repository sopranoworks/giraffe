// Directive 058: whether giraffe gets used at all when the conditions
// are the Coder's own. Every run from 030 to 056 went through
// run-arrival.ts, whose prompt names the binary, whose listing holds one
// or two skills, whose system prompt is the SDK's minimal one and whose
// tools are three. This script changes all four and nothing else:
//
// - the system prompt is the claude_code preset;
// - the tools are the preset's, every one a real session gets;
// - settings come from the user, project and local sources, read from a
//   curated copy of the Coder's ~/.claude mounted at /root/.claude
//   (build-faithful-home.sh says what was kept and what was dropped),
//   so the skill listing is whatever that copy holds — skills: 'all';
// - the prompt is the task text and nothing else. How the agent learns
//   giraffe exists is the thing tested, so this script says nothing
//   about it: configuration B puts the binary on PATH, C also installs
//   the skill in the curated home, A does neither (docker-run-faithful.sh
//   does all three). No configuration touches the corpus's CLAUDE.md.
//
// MCP stays closed: strictMcpConfig with no mcpServers, as 029 wanted,
// which no longer needs settingSources [] to hold.
//
// CONFIG (A|B|C), CWD, RUN_ID, OUT_DIR, TASK_TEXT and MODEL are read from
// the environment.

import { existsSync, readdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { runOne, gitCommitOrNull } from "./harness.js";

const config = process.env.CONFIG ?? "A";
const cwd = process.env.CWD ?? "/corpus";
const runId = process.env.RUN_ID ?? `faithful-${config}-${Date.now()}`;
const outDir = process.env.OUT_DIR ?? "/runs";
const taskText = process.env.TASK_TEXT;
const model = process.env.MODEL;
const maxTurns = Number(process.env.MAX_TURNS ?? "60");

// The account-synced plugins (design, cowork-plugin-management) sit in
// the curated home where the host has them, but the CLI in a container
// lists the synced skills beside them and not these — its probe run
// listed 28 skills and none of design:*. Passed as local plugins, they
// reach the listing under the same plugin:skill names the Coder sees.
function syncedPlugins(home: string): { type: "local"; path: string }[] {
  const root = join(home, ".claude", "plugins", "synced");
  if (!existsSync(root)) return [];
  const found: { type: "local"; path: string }[] = [];
  for (const bucket of readdirSync(root, { withFileTypes: true })) {
    if (!bucket.isDirectory() || bucket.name.startsWith(".")) continue;
    for (const plugin of readdirSync(join(root, bucket.name), { withFileTypes: true })) {
      const path = join(root, bucket.name, plugin.name);
      if (plugin.isDirectory() && existsSync(join(path, ".claude-plugin", "plugin.json"))) {
        found.push({ type: "local", path });
      }
    }
  }
  return found;
}

async function main() {
  if (!taskText) throw new Error("TASK_TEXT is required");
  const record = await runOne(
    {
      runId,
      label: `faithful-${config}`,
      prompt: taskText,
      cwd,
      model,
      tools: { type: "preset", preset: "claude_code" },
      // The Coder's own session runs in auto mode: a classifier decides
      // what would otherwise prompt.
      permissionMode: "auto",
      settingSources: ["user", "project", "local"],
      strictMcpConfig: true,
      plugins: syncedPlugins(process.env.HOME ?? "/root"),
      skills: "all",
      systemPrompt: { type: "preset", preset: "claude_code" },
      maxTurns,
      corpus: { root: cwd, commit: gitCommitOrNull(cwd) },
    },
    outDir
  );
  writeFileSync(join(outDir, `${runId}.summary.json`), JSON.stringify(record, null, 2));
  console.log(JSON.stringify(record, null, 2));
}

main().catch((err) => {
  console.error(err);
  process.exitCode = 1;
});
