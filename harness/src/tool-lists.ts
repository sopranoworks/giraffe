// Directive 029 §1: both arms' tool lists, closed by construction — "deny
// everything, then allow exactly what the arm is meant to have" — rather
// than the ambient default minus a few names. 028 found the ambient
// default is 41 tools inherited from this host's own Claude Code
// configuration (SendMessage, Workflow, Google Drive, context7, ...); an
// arm that only has a few of those *disallowed* still holds the rest.
//
// Two layers, not one (docs, queried this directive): `tools` is the
// availability layer — a bare name omitted from it never reaches the
// model's context at all, which is what "closed" means here. Bash's own
// sub-command scoping is a *permission*-layer concern: `allowedTools`
// scoped patterns plus `permissionMode: "dontAsk"` approve exactly the
// listed command shapes and deny everything else outright, with no
// prompt to fall through to. A broad `disallowedTools` deny (e.g.
// "Bash(*)") was considered and rejected: deny rules are checked before
// allow rules in the SDK's fixed evaluation order, so a general Bash deny
// would shadow every scoped allow underneath it and nothing would ever
// run.

// Both arms get exactly Read and Bash at the availability layer. Nothing
// else is in context to be attempted, regardless of what the permission
// layer below would do with it.
export const CLOSED_TOOLS: readonly string[] = ["Read", "Bash"];

// `tools: CLOSED_TOOLS` alone is not enough — found this directive,
// empirically, the hard way: run on this host (not inside the docker
// guard, where no ambient MCP servers were even reachable), it still
// left this session's own project/user-configured MCP servers (Google
// Drive, context7 — 12 tools) in system/init's own tools array. `tools`
// gates built-in names; MCP-server exposure is a separate mechanism,
// controlled by settingSources/mcpServers/strictMcpConfig, not by
// `tools`. Both belong on every arm's config alongside CLOSED_TOOLS —
// see harness.ts's query() call and RunConfig in types.ts.
export const CLOSED_SETTING_SOURCES: readonly string[] = [];
export const STRICT_MCP_CONFIG = true;

// Unaided: grep/find/ls/sed -n/cat/wc, and nothing else — the directive's
// own list, verbatim.
export const UNAIDED_BASH_PATTERNS: readonly string[] = [
  "Bash(grep *)",
  "Bash(find *)",
  "Bash(ls *)",
  "Bash(sed -n *)",
  "Bash(cat *)",
  "Bash(wc *)",
];

// Aided: the same, plus the built giraffe binary — supplied by the
// caller, since its path depends on where the corpus/binary were staged
// for a given run (see stage-corpus.ts).
export function aidedBashPatterns(giraffeBinaryPath: string): readonly string[] {
  return [...UNAIDED_BASH_PATTERNS, `Bash(${giraffeBinaryPath} *)`];
}

export const PERMISSION_MODE = "dontAsk";
