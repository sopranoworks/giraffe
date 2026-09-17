import { execFileSync } from "node:child_process";
import { mkdirSync, appendFileSync, existsSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { query, type SDKMessage, type SdkPluginConfig } from "@anthropic-ai/claude-agent-sdk";
import type { RunConfig, RunRecord, Sampling } from "./types.js";

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

// Directive 047 §1: the sampling parameters, observed rather than
// assumed. The Claude Code process logs the fields of every request it
// makes — "[API REQUEST DETAIL] {model, thinking, output_config,
// temperature, ...}" — but only at its `verbose` level, one above what
// `--debug` alone selects; CLAUDE_CODE_DEBUG_LOG_LEVEL is what raises
// it, and `debugFile` puts the log beside the transcript instead of on
// stderr. The auth line in that log redacts the credential itself
// ("Bearer ***"), which is why the file can be kept as evidence.
const DEBUG_LOG_LEVEL_ENV = "CLAUDE_CODE_DEBUG_LOG_LEVEL";
const DEBUG_LOG_LEVEL = "verbose";
const REQUEST_DETAIL = "[API REQUEST DETAIL] ";

// init reports the model with the context window appended —
// "claude-opus-5[1m]" — and the request carries the bare name. Strip the
// suffix so a run's own requests can be told from the helper requests
// (session titles, classifiers) the same process makes on other models.
function wireModel(model: string): string {
  return model.replace(/\[[^\]]*\]$/, "");
}

interface RequestDetail {
  readonly model?: string;
  readonly temperature?: number;
  readonly thinking?: { readonly type?: string };
  readonly output_config?: { readonly effort?: string };
}

// The requests this run's own model received, in the order the log holds
// them. A line that is not JSON is skipped with no error: the log is a
// debugging aid the CLI owns, not an interface it promises this harness,
// and a run must not fail because one line of it changed shape.
function requestDetails(debugPath: string, model: string): RequestDetail[] {
  if (!existsSync(debugPath)) return [];
  const found: RequestDetail[] = [];
  for (const line of readFileSync(debugPath, "utf8").split("\n")) {
    const at = line.indexOf(REQUEST_DETAIL);
    if (at === -1) continue;
    let detail: RequestDetail;
    try {
      detail = JSON.parse(line.slice(at + REQUEST_DETAIL.length)) as RequestDetail;
    } catch {
      continue;
    }
    if (detail.model === model) found.push(detail);
  }
  return found;
}

export function samplingFromDebugLog(debugPath: string, model: string | null): Sampling | null {
  if (model === null) return null;
  const wire = wireModel(model);
  const fields = requestDetails(debugPath, wire).map((d) => ({
    model: wire,
    // A request that carries no temperature field is recorded as null.
    // Every model the CLI does not list as accepting one is sent without
    // it, and the service's own default is not this harness's to state.
    temperature: d.temperature ?? null,
    thinking: d.thinking?.type ?? null,
    effort: d.output_config?.effort ?? null,
  }));
  const first = fields[0];
  // No request for this model in the log: nothing observed, so nothing
  // recorded. The type checker wants the guard too — an index into a
  // possibly-empty array is possibly undefined.
  if (first === undefined) return null;
  return {
    ...first,
    requests: fields.length,
    uniform: fields.every((f) => JSON.stringify(f) === JSON.stringify(first)),
  };
}

// The one place a run is executed and captured. Both smoke scripts call
// this — sharing it is what keeps the transcript/usage capture from
// drifting between them (code-manners D5).
export async function runOne(config: RunConfig, outDir: string): Promise<RunRecord> {
  mkdirSync(outDir, { recursive: true });
  const transcriptPath = join(outDir, `${config.runId}.transcript.jsonl`);
  const debugPath = join(outDir, `${config.runId}.debug.log`);
  if (existsSync(transcriptPath)) {
    throw new Error(`refusing to overwrite an existing transcript: ${transcriptPath}`);
  }

  const startedAt = new Date().toISOString();

  let actualCwd: string | null = null;
  let actualModel: string | null = null;
  let actualTools: readonly string[] | null = null;
  let actualPermissionMode: string | null = null;
  let actualSkills: readonly string[] | null = null;
  let actualPlugins: readonly { name: string; path: string }[] | null = null;
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
  // Directive 029 §3: counted from the SDK's own assistant messages as
  // they stream by, not asked of the agent and not read off a bare field
  // on the result message (there isn't one — see types.ts).
  let toolUseCount = 0;

  for await (const message of query({
    prompt: config.prompt,
    options: {
      cwd: config.cwd,
      model: config.model,
      tools: config.tools as string[] | undefined,
      allowedTools: config.allowedTools as string[] | undefined,
      disallowedTools: config.disallowedTools as string[] | undefined,
      permissionMode: config.permissionMode as never,
      settingSources: config.settingSources as never,
      strictMcpConfig: config.strictMcpConfig,
      plugins: config.plugins as SdkPluginConfig[] | undefined,
      skills: config.skills as string[] | undefined,
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      sandbox: config.sandbox as any,
      maxTurns: config.maxTurns,
      debugFile: debugPath,
      env: { ...process.env, [DEBUG_LOG_LEVEL_ENV]: DEBUG_LOG_LEVEL },
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
      actualSkills = message.skills;
      actualPlugins = message.plugins;
      claudeCodeVersion = message.claude_code_version;
      apiKeySource = message.apiKeySource as unknown as string;
    }

    if (message.type === "assistant") {
      // O9: a boundary type-dispatch against the SDK's own externally-owned
      // content-block union, not a closed set this module owns.
      const content = (message as { message?: { content?: unknown } }).message?.content;
      if (Array.isArray(content)) {
        for (const block of content) {
          if ((block as { type?: string }).type === "tool_use") toolUseCount++;
        }
      }
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

  const u = rawUsage as
    | { input_tokens?: number; output_tokens?: number; cache_creation_input_tokens?: number | null; cache_read_input_tokens?: number | null }
    | null;
  const totalTokens =
    u == null
      ? null
      : (u.input_tokens ?? 0) + (u.output_tokens ?? 0) + (u.cache_creation_input_tokens ?? 0) + (u.cache_read_input_tokens ?? 0);

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
      tools: config.tools,
      allowedTools: config.allowedTools,
      disallowedTools: config.disallowedTools,
      permissionMode: config.permissionMode,
      settingSources: config.settingSources,
      strictMcpConfig: config.strictMcpConfig,
      plugins: config.plugins,
      skills: config.skills,
      sandbox: config.sandbox,
      corpus: config.corpus,
    },
    actual: {
      cwd: actualCwd,
      model: actualModel,
      tools: actualTools,
      permissionMode: actualPermissionMode,
      skills: actualSkills,
      plugins: actualPlugins,
      sampling: samplingFromDebugLog(debugPath, actualModel),
    },
    usage: {
      numTurns,
      durationMs,
      durationApiMs,
      totalCostUsd,
      totalTokens,
      toolUseCount,
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
