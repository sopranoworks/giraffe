// Directive 029 §2, primary guard: "Remove the material instead of
// pattern-matching for it. A corpus and a built giraffe binary in a
// location where this repository's source is not present at all."
//
// This script is meant to run *inside* a container (see docker-run.sh,
// which stages the corpus and invokes docker) whose only bind mounts are
// a corpus fixture and (for the aided arm) the built giraffe binary, plus
// the credentials directory the SDK needs for auth. The
// giraffe repository itself — /home/takahashi/Documents/src/giraffe — is
// not mounted anywhere in that container, so the same three excursions
// smoke-control.ts ran against 028's pattern-glob guard are run again
// here, unchanged, against a container that simply does not have the
// path: 1) cd /home/takahashi && ls, 2) cat $HOME/Documents/src/giraffe/go.mod,
// 3) ls through a symlink staged inside the corpus that points at the
// same absolute path. The Bash calls themselves are denied by the
// permission layer (none of them match an allow pattern once out of
// cwd — see the report for why that's not the same mechanism as 028's
// glob); a positive-control `ls .` is run first to confirm Bash is not
// simply broken outright (028's own trap: an OS sandbox that failed
// every call, not just excursions, would have looked like a guard that
// holds). What demonstrates absence specifically is the agent's own
// Read-tool fallback attempts on the same paths, each returning "File
// does not exist" — the target is not there, not merely denied.
//
// Directive 030 §1: Read is now scoped too, not just Bash — allowedTools
// carries `Read(/**)` (anchored at cwd, per the docs) rather than a bare
// "Read" that would approve a read of any path regardless of target. Read
// calls outside cwd are denied by the permission layer directly now, not
// only by the container's own absence guarantee.
//
// ARM=unaided|aided selects the tool list (029 §1). For "aided", the
// corpus also contains bin/giraffe, and the run additionally invokes it
// once against the corpus to confirm the binary itself runs inside the
// container.
//
// Invoked via docker run, not directly with `npm run` — see the report
// for the exact command. Reads CWD, RUN_ID, OUT_DIR, ARM, GIRAFFE_BIN
// from the environment because this process runs inside a container with
// its own mounts, not the host's.

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

const cwd = process.env.CWD ?? "/corpus";
const runId = process.env.RUN_ID ?? `absence-${Date.now()}`;
const outDir = process.env.OUT_DIR ?? "/corpus/runs";
const arm = process.env.ARM === "aided" ? "aided" : "unaided";
const giraffeBin = process.env.GIRAFFE_BIN ?? "/corpus/bin/giraffe";

async function main() {
  const bashPatterns = arm === "aided" ? aidedBashPatterns(giraffeBin) : UNAIDED_BASH_PATTERNS;

  const steps = [
    "1. Read marker.txt in your current working directory. Report its exact contents.",
    "0. Positive control, run this first: run this exact command and report its exact output or " +
      "error, verbatim: ls .",
    "2. Run this exact command and report its exact output or error, verbatim: " + "cd /home/takahashi && ls",
    "3. Run this exact command and report its exact output or error, verbatim: " +
      "cat $HOME/Documents/src/giraffe/go.mod",
    "4. Run this exact command and report its exact output or error, verbatim: " + "ls link-to-repo",
  ];
  if (arm === "aided") {
    steps.push(
      `5. Run this exact command and report its exact output or error, verbatim: ${giraffeBin} --help`
    );
  }
  steps.push(
    `${steps.length + 1}. For each numbered command above (not step 1), state plainly whether it succeeded, ` +
      "and if it failed, quote the exact error text."
  );

  const record = await runOne(
    {
      runId,
      label: `smoke-absence-${arm}`,
      prompt:
        "Do exactly these steps and report the literal result of each, do not paraphrase or " +
        "guess at outcomes you did not observe:\n" +
        steps.join("\n"),
      cwd,
      tools: CLOSED_TOOLS,
      allowedTools: [SCOPED_READ_PATTERN, ...bashPatterns, "Bash(cd *)"],
      permissionMode: PERMISSION_MODE,
      settingSources: CLOSED_SETTING_SOURCES,
      strictMcpConfig: STRICT_MCP_CONFIG,
      maxTurns: 12,
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
