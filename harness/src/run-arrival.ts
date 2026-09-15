// Directive 030: the measurement itself. Both arms, on the arrival task,
// through the harness — everything from 010 to 029 was implementation or
// harness; this is the first run that produces a comparison since 009,
// and 009 was contaminated (spec §28 C-37: the aided arm ran second in
// the same session and already knew the names the unaided arm had found).
//
// Fix, per C-37 and this directive: separate sessions. This script is
// invoked once per arm, each invocation its own process and its own
// query() call (see docker-run-arrival.sh) — the aided arm's process
// never sees the unaided arm's transcript, output, or any trace of it.
// Order between the two runs (which docker invocation happens first)
// therefore cannot contaminate either arm, unlike 009's single-session
// unaided-then-aided design.
//
// Task text is identical for both arms (directive's own wording, verbatim,
// unmodified): "Add an option that prefixes a Pod's name with its start
// timestamp. Where do you start?" The aided arm's prompt additionally
// names the giraffe binary's existence and location (not its design) as a
// short preamble before that same sentence — the directive's own
// distinction between "the task" and what the aided arm is told about its
// tool.
//
// Directive 030 §4: no scripted steps, no forced search strategy — unlike
// smoke-control.ts/smoke-absence.ts's excursion tests, this prompt does
// not dictate any commands. The agent explores freely with whatever tools
// its arm has (029 §1's closed lists, 030 §1's Read(/**) scoping); round
// trips are counted and classified from the transcript afterward, not
// prescribed here.
//
// ARM=unaided|aided, CWD (the corpus root), RUN_ID, OUT_DIR, and (aided
// only) GIRAFFE_BIN are read from the environment — this runs inside a
// container with its own mounts, not the host's (see
// docker-run-arrival.sh for the exact invocation).

import { writeFileSync } from "node:fs";
import { join } from "node:path";
import { runOne, gitCommitOrNull } from "./harness.js";
import {
  CLOSED_TOOLS,
  CLOSED_SETTING_SOURCES,
  STRICT_MCP_CONFIG,
  UNAIDED_BASH_PATTERNS,
  aidedBashPatterns,
  PERMISSION_MODE,
  SCOPED_READ_PATTERN,
} from "./tool-lists.js";

const TASK_TEXT = "Add an option that prefixes a Pod's name with its start timestamp. Where do you start?";

const cwd = process.env.CWD ?? "/corpus";
const runId = process.env.RUN_ID ?? `arrival-${Date.now()}`;
const outDir = process.env.OUT_DIR ?? "/runs";
const arm = process.env.ARM === "aided" ? "aided" : "unaided";
const giraffeBin = process.env.GIRAFFE_BIN ?? "/giraffe-bin/giraffe";

async function main() {
  const bashPatterns = arm === "aided" ? aidedBashPatterns(giraffeBin) : UNAIDED_BASH_PATTERNS;

  const prompt =
    arm === "aided"
      ? `You have access to a command-line tool at ${giraffeBin}. It searches this codebase; run "${giraffeBin} --help" if you want to see its usage. ` +
        TASK_TEXT
      : TASK_TEXT;

  const record = await runOne(
    {
      runId,
      label: `arrival-${arm}`,
      prompt,
      cwd,
      tools: CLOSED_TOOLS,
      allowedTools: [SCOPED_READ_PATTERN, ...bashPatterns, "Bash(cd *)"],
      permissionMode: PERMISSION_MODE,
      settingSources: CLOSED_SETTING_SOURCES,
      strictMcpConfig: STRICT_MCP_CONFIG,
      maxTurns: 40,
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
