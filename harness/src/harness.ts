import { execFileSync } from "node:child_process";
import { mkdirSync, appendFileSync, existsSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { query, type SDKMessage } from "@anthropic-ai/claude-agent-sdk";
import type { RunConfig, RunRecord } from "./types.js";

function sdkVersion(): string {
  // Read the file directly rather than `require(".../package.json")` —
  // the installed package's own `exports` map does not expose that
  // subpath, so module resolution refuses it even though the file is
  // right there. Read from the installed package itself, not this
  // repo's own package.json: the two can drift (a lockfile-resolved
  // version vs. the requested semver range), and §3 needs the one
  // actually running.
  const pkgPath = new URL("../node_modules/@anthropic-ai/claude-agent-sdk/package.json", import.meta.url);
  return (JSON.parse(readFileSync(pkgPath, "utf8")) as { version: string }).version;
}

// §3: corpus pinning. A cwd that is not a git repository returns null,
// recorded as such — not guessed, not silently omitted. stderr is
// discarded: "fatal: not a git repository" is the expected, handled
// case for a plain fixture directory, not a real error to surface.
export function gitCommitOrNull(root: string): string | null {
  try {
    return execFileSync("git", ["rev-parse", "HEAD"], {
      cwd: root,
      encoding: "utf8",
      stdio: ["ignore", "pipe", "ignore"],
    }).trim();
  } catch {
    return null;
  }
}

// The one place a run is executed and captured. Both smoke scripts call
// this — sharing it is what keeps the transcript/usage capture from
// drifting between them (code-manners D5).
export async function runOne(config: RunConfig, outDir: string): Promise<RunRecord> {
  mkdirSync(outDir, { recursive: true });
  const transcriptPath = join(outDir, `${config.runId}.transcript.jsonl`);
  if (existsSync(transcriptPath)) {
    throw new Error(`refusing to overwrite an existing transcript: ${transcriptPath}`);
  }

  const startedAt = new Date().toISOString();

  let actualCwd: string | null = null;
  let actualModel: string | null = null;
  let actualTools: readonly string[] | null = null;
  let actualPermissionMode: string | null = null;
  let claudeCodeVersion: string | null = null;
  let apiKeySource: string | null = null;

  let numTurns: number | null = null;
  let durationMs: number | null = null;
  let durationApiMs: number | null = null;
  let totalCostUsd: number | null = null;
  let rawUsage: unknown = null;
  let permissionDenials: unknown = null;
  let resultSubtype: string | null = null;
  let isError: boolean | null = null;
  let resultText: string | null = null;

  let messageCount = 0;

  for await (const message of query({
    prompt: config.prompt,
    options: {
      cwd: config.cwd,
      model: config.model,
      allowedTools: config.allowedTools as string[] | undefined,
      disallowedTools: config.disallowedTools as string[] | undefined,
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      sandbox: config.sandbox as any,
      maxTurns: config.maxTurns,
    },
  }) as AsyncIterable<SDKMessage>) {
    // §1: the full transcript, as data — every message the SDK emits,
    // written as it arrives, not summarized and not buffered until the
    // end (a crash mid-run still leaves everything received so far).
    appendFileSync(transcriptPath, JSON.stringify(message) + "\n");
    messageCount++;

    if (message.type === "system" && message.subtype === "init") {
      actualCwd = message.cwd;
      actualModel = message.model;
      actualTools = message.tools;
      actualPermissionMode = message.permissionMode;
      claudeCodeVersion = message.claude_code_version;
      apiKeySource = message.apiKeySource as unknown as string;
    }

    if (message.type === "result") {
      numTurns = message.num_turns;
      durationMs = message.duration_ms;
      durationApiMs = message.duration_api_ms;
      totalCostUsd = message.total_cost_usd;
      rawUsage = message.usage;
      permissionDenials = message.permission_denials;
      resultSubtype = message.subtype;
      isError = message.is_error;
      resultText = message.subtype === "success" ? message.result : null;
    }
  }

  const endedAt = new Date().toISOString();

  const record: RunRecord = {
    runId: config.runId,
    label: config.label,
    startedAt,
    endedAt,
    sdkVersion: sdkVersion(),
    claudeCodeVersion,
    apiKeySource,
    requested: {
      prompt: config.prompt,
      cwd: config.cwd,
      model: config.model,
      allowedTools: config.allowedTools,
      disallowedTools: config.disallowedTools,
      sandbox: config.sandbox,
      corpus: config.corpus,
    },
    actual: {
      cwd: actualCwd,
      model: actualModel,
      tools: actualTools,
      permissionMode: actualPermissionMode,
    },
    usage: {
      numTurns,
      durationMs,
      durationApiMs,
      totalCostUsd,
      rawUsage,
      permissionDenials,
      subtype: resultSubtype,
      isError,
    },
    transcriptPath,
    transcriptMessageCount: messageCount,
    resultText,
  };

  const recordPath = join(outDir, `${config.runId}.run-record.json`);
  appendFileSync(recordPath, JSON.stringify(record, null, 2));

  return record;
}
