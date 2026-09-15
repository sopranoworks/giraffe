// Directive 028 §5: "a smoke run against a trivial task is expected —
// enough to show the transcript, the usage figures and the working
// directory work." This is that run. It also demonstrates §1's tool-
// restriction property (disallowedTools removes a tool definition; the
// init message's own tools[] list is the evidence, not a guess).
//
// Invoke: npm run smoke:basic   (from harness/)

import { mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { runOne, gitCommitOrNull } from "./harness.js";

async function main() {
  const cwd = mkdtempSync(join(tmpdir(), "hrun-basic-"));
  writeFileSync(join(cwd, "marker.txt"), "BASIC_SMOKE_MARKER=284917\n");

  const runId = `basic-${Date.now()}`;
  const outDir = join(process.cwd(), "runs");

  const record = await runOne(
    {
      runId,
      label: "smoke-basic",
      prompt:
        "Read the file marker.txt in your current working directory using the Read tool, " +
        "and report its exact contents in one line. Then say the literal word DONE.",
      cwd,
      allowedTools: ["Read"],
      disallowedTools: ["Bash", "WebSearch", "WebFetch"],
      maxTurns: 6,
      corpus: { root: cwd, commit: gitCommitOrNull(cwd) },
    },
    outDir
  );

  console.log(JSON.stringify(record, null, 2));
}

main().catch((err) => {
  console.error(err);
  process.exitCode = 1;
});
