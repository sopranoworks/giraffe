// Directive 028: shared shapes for the measurement harness. No classes —
// this harness is data plus functions over it (oop.md's class-based rules
// are scoped to class-based TypeScript; a plain functional module sidesteps
// them by construction, the same instinct behind giraffe's own Go style).

export interface RunConfig {
  readonly runId: string;
  readonly label: string;
  readonly prompt: string;
  readonly cwd: string;
  readonly model?: string;
  readonly allowedTools?: readonly string[];
  readonly disallowedTools?: readonly string[];
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
    readonly allowedTools: readonly string[] | undefined;
    readonly disallowedTools: readonly string[] | undefined;
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
  };

  // Straight from the SDK's final "result" message. Never computed by
  // asking the agent, and never adjusted by the harness.
  readonly usage: {
    readonly numTurns: number | null;
    readonly durationMs: number | null;
    readonly durationApiMs: number | null;
    readonly totalCostUsd: number | null;
    readonly rawUsage: unknown;
    readonly permissionDenials: unknown;
    readonly subtype: string | null;
    readonly isError: boolean | null;
  };

  readonly transcriptPath: string;
  readonly transcriptMessageCount: number;
  readonly resultText: string | null;
}
