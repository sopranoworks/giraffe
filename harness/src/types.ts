// Directive 028: shared shapes for the measurement harness. No classes —
// this harness is data plus functions over it (oop.md's class-based rules
// are scoped to class-based TypeScript; a plain functional module sidesteps
// them by construction, the same instinct behind giraffe's own Go style).

import type { SdkPluginConfig } from "@anthropic-ai/claude-agent-sdk";

export interface RunConfig {
  readonly runId: string;
  readonly label: string;
  readonly prompt: string;
  readonly cwd: string;
  readonly model?: string;
  // Directive 029 §1: the availability-layer closed list — "omit it from
  // `tools` ... keep[s] the tool out of context" (docs). This is what
  // makes an arm's tool set closed rather than "everything ambient minus
  // a few names"; allowedTools/disallowedTools below only scope
  // permission within whatever `tools` grants.
  readonly tools?: readonly string[];
  readonly allowedTools?: readonly string[];
  readonly disallowedTools?: readonly string[];
  readonly permissionMode?: string;
  // Directive 029 finding: `tools` alone does not close an arm. Run on
  // the host (not the container — see the report), `tools: ["Read",
  // "Bash"]` still left this *host's own* project/user-configured MCP
  // servers (Google Drive, context7) in system/init's tools array —
  // `tools` gates built-in names, not MCP-server exposure, which is
  // controlled separately. `settingSources: []` stops the SDK from
  // reading this host's user/project/local settings at all (so those
  // servers, and any permission/hook config living there, are never
  // loaded); `strictMcpConfig: true` additionally restricts MCP servers
  // to exactly what `mcpServers` names (empty, here). Both default on
  // for a closed arm — see tool-lists.ts.
  readonly settingSources?: readonly string[];
  readonly strictMcpConfig?: boolean;
  // Directive 045 §3: what carries a skill into an arm. With
  // settingSources [] the SDK reads no skill from this host, so a
  // plugin directory — the SDK's own shape, passed through verbatim —
  // is the one explicit carrier, and `skills` names which of the
  // discovered skills the model is shown (the SDK hides the rest from
  // its listing). Both absent: the arm has what 029 gave it and
  // nothing more.
  readonly plugins?: readonly SdkPluginConfig[];
  readonly skills?: readonly string[];
  // Passed through verbatim to the SDK's `sandbox` option. Left untyped
  // here (not re-declared) so this file does not drift from the SDK's own
  // SandboxSettings shape — see harness.ts's import instead.
  readonly sandbox?: unknown;
  readonly maxTurns?: number;
  // §3: what corpus this run's cwd is checked out from. `commit` is null
  // when the cwd is not a git repository (a plain fixture, e.g. a smoke
  // task) — recorded as null, not guessed or omitted.
  readonly corpus: {
    readonly root: string;
    readonly commit: string | null;
  };
}

// §1/§3: everything the harness itself observed about a run, taken from
// the SDK's own init and result messages — never from anything the agent
// said about itself.
export interface RunRecord {
  readonly runId: string;
  readonly label: string;
  readonly startedAt: string;
  readonly endedAt: string;
  readonly sdkVersion: string;
  readonly claudeCodeVersion: string | null;
  readonly apiKeySource: string | null;

  readonly requested: {
    readonly prompt: string;
    readonly cwd: string;
    readonly model: string | undefined;
    readonly tools: readonly string[] | undefined;
    readonly allowedTools: readonly string[] | undefined;
    readonly disallowedTools: readonly string[] | undefined;
    readonly permissionMode: string | undefined;
    readonly settingSources: readonly string[] | undefined;
    readonly strictMcpConfig: boolean | undefined;
    readonly plugins: readonly SdkPluginConfig[] | undefined;
    readonly skills: readonly string[] | undefined;
    readonly sandbox: unknown;
    readonly corpus: RunConfig["corpus"];
  };

  // What the SDK's own "system"/"init" message reported back — the
  // ground truth for what the session actually got, as opposed to what
  // was asked for above. Absent (null) only if no init message arrived.
  readonly actual: {
    readonly cwd: string | null;
    readonly model: string | null;
    readonly tools: readonly string[] | null;
    readonly permissionMode: string | null;
    // Directive 045 §3: the skills the session lists and the plugins it
    // loaded, as init reports them — 029's rule, verify against what the
    // model was given. A skill's body still only reaches the model when
    // it asks for it (a Skill tool call in the transcript); this says it
    // was offered.
    readonly skills: readonly string[] | null;
    readonly plugins: readonly { name: string; path: string }[] | null;
  };

  // Straight from the SDK's final "result" message. Never computed by
  // asking the agent, and never adjusted by the harness.
  //
  // Directive 029 §3: there is no fixed per-launch overhead to subtract —
  // 028 measured 36,591 tokens for a two-turn task and 48,153 for a
  // four-turn one, so it scales with turns. totalTokens and toolUseCount
  // are reported for side-by-side comparison, not as a baseline to net
  // out. Neither is a bare field on the result message: totalTokens sums
  // rawUsage's own token fields (still 100% SDK-reported data, just
  // arithmetic the harness does instead of the caller); toolUseCount
  // counts tool_use content blocks across the assistant messages this run
  // streamed, since the result message carries no such count directly —
  // still SDK message data, not anything the agent said about itself.
  readonly usage: {
    readonly numTurns: number | null;
    readonly durationMs: number | null;
    readonly durationApiMs: number | null;
    readonly totalCostUsd: number | null;
    readonly totalTokens: number | null;
    readonly toolUseCount: number;
    readonly rawUsage: unknown;
    readonly permissionDenials: unknown;
    readonly subtype: string | null;
    readonly isError: boolean | null;
  };

  readonly transcriptPath: string;
  readonly transcriptMessageCount: number;
  readonly resultText: string | null;
}
