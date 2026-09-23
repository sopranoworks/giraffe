// Directive 041 §2: where the tokens go. Reads the transcripts run-arrival.ts
// wrote (harness.ts's own message-per-line JSONL, nothing re-derived from
// what the agent said) and attributes tokens to tool calls.
//
// The SDK reports usage per API call: every assistant turn is billed for
// its whole context (input + cache_read + cache_creation) plus its own
// output, and totalTokens (harness.ts) is the sum of those over every
// turn. So a tool result costs tokens twice over: once as the bytes it
// adds to the context, and again on every later turn that re-reads that
// context. The two figures reported per call below are therefore:
//
//   added   — the growth of the next assistant turn's context over the
//             previous one, in tokens (the tool result plus whatever the
//             model itself emitted in between); the direct size
//   carried — added × the number of later turns, the number of times that
//             growth was re-read afterwards; what the call actually
//             contributed to totalTokens
//
// Usage: tsx src/analyze-arrival.ts [--landing <points.json>] <transcript.jsonl>...
// One transcript: the per-call table. Several: one line per run
// (tokens/turns/tool calls) and the median and range per arm, the
// distribution 041 §1 asks for.
//
// Directive 045 (code-manners D6: 040, 041 and 044 each scored arrival
// with a one-off script, and the third copy forces extraction):
// --landing names a JSON file of {"landing point": "regex"} — the
// task's own list, e.g. landing/gekka-actors.json — and each run's
// final answer is scored against it, hits out of points, the way 041
// did. The score is a grep of the agent's answer, not a judgement of
// it; the report says what the points are.
//
// Also 045: a Skill tool call is its own category, and the run line
// says whether the run loaded a skill and how many bytes it was — the
// directive's "count its cost".

import { readFileSync } from "node:fs";
import { basename } from "node:path";

interface Turn {
  readonly id: string;
  readonly context: number; // input + cache_read + cache_creation
  readonly output: number;
  readonly toolUses: readonly ToolUse[];
  readonly text: string;
}

interface ToolUse {
  readonly id: string;
  readonly name: string;
  readonly summary: string;
}

interface ToolCallCost {
  readonly index: number;
  readonly turn: number;
  readonly name: string;
  readonly summary: string;
  readonly resultBytes: number;
  readonly added: number;
  readonly carried: number;
}

interface RunSummary {
  readonly file: string;
  readonly arm: string;
  readonly totalTokens: number;
  readonly turns: number;
  readonly toolCalls: number;
  readonly costUsd: number | null;
  // The first skill the run loaded: the bytes of its body and the
  // tokens the load added to the next turn's context. Null when the run
  // made no Skill call. The body is not the tool result (that is one
  // line, "Launching skill: ...") — the SDK delivers it as a user
  // message of its own, prefixed "Base directory for this skill:".
  readonly skill: { bytes: number; tokens: number } | null;
  // Landing points hit, when --landing was given.
  readonly arrival: Arrival | null;
}

interface Arrival {
  readonly hit: number;
  readonly of: number;
  readonly missed: readonly string[];
}

type LandingPoints = Record<string, string>;

function scoreArrival(finalText: string, points: LandingPoints): Arrival {
  const missed = Object.entries(points)
    .filter(([, pattern]) => !new RegExp(pattern).test(finalText))
    .map(([name]) => name);
  const of = Object.keys(points).length;
  return { hit: of - missed.length, of, missed };
}

// The SDK streams one assistant message per content block, repeating the
// message id and usage on each — turns are deduplicated by id, and the
// tool_use blocks of every part with that id are collected together.
const SKILL_BODY_PREFIX = "Base directory for this skill:";

function readTranscript(path: string): { turns: Turn[]; results: Map<string, number>; skillBodies: number[]; final: unknown } {
  const turns: Turn[] = [];
  const byId = new Map<string, { context: number; output: number; toolUses: ToolUse[]; text: string }>();
  const results = new Map<string, number>();
  const skillBodies: number[] = [];
  let final: unknown = null;
  for (const line of readFileSync(path, "utf8").split("\n")) {
    if (line.trim() === "") continue;
    const m = JSON.parse(line) as { type: string; message?: { id?: string; usage?: Record<string, number>; content?: unknown } };
    if (m.type === "assistant" && m.message?.id) {
      const u = m.message.usage ?? {};
      let t = byId.get(m.message.id);
      if (!t) {
        t = {
          context: (u.input_tokens ?? 0) + (u.cache_read_input_tokens ?? 0) + (u.cache_creation_input_tokens ?? 0),
          output: u.output_tokens ?? 0,
          toolUses: [],
          text: "",
        };
        byId.set(m.message.id, t);
        turns.push({ id: m.message.id, ...t, toolUses: t.toolUses });
      }
      // output_tokens on a streamed part is the running figure; the last
      // part carries the turn's whole output.
      t.output = Math.max(t.output, u.output_tokens ?? 0);
      for (const block of Array.isArray(m.message.content) ? (m.message.content as Record<string, unknown>[]) : []) {
        if (block.type === "tool_use") {
          const input = block.input as Record<string, unknown>;
          const summary = typeof input.command === "string" ? input.command : typeof input.file_path === "string" ? input.file_path : JSON.stringify(input);
          t.toolUses.push({ id: block.id as string, name: block.name as string, summary: summary.replace(/\s+/g, " ").slice(0, 110) });
        }
        if (block.type === "text") t.text += block.text as string;
      }
    }
    if (m.type === "user") {
      for (const block of Array.isArray(m.message?.content) ? (m.message!.content as Record<string, unknown>[]) : []) {
        if (block.type === "tool_result") {
          const c = block.content;
          const text = typeof c === "string" ? c : Array.isArray(c) ? c.map((x: { text?: string }) => x.text ?? "").join("") : JSON.stringify(c);
          results.set(block.tool_use_id as string, text.length);
        }
        if (block.type === "text" && typeof block.text === "string" && block.text.startsWith(SKILL_BODY_PREFIX)) {
          skillBodies.push(block.text.length);
        }
      }
    }
    if (m.type === "result") final = m;
  }
  // Rebuild turns from the mutable accumulators so output/toolUses are final.
  const built: Turn[] = turns.map((t) => {
    const acc = byId.get(t.id)!;
    return { id: t.id, context: acc.context, output: acc.output, toolUses: acc.toolUses, text: acc.text };
  });
  return { turns: built, results, skillBodies, final };
}

function toolCallCosts(turns: readonly Turn[], results: Map<string, number>): ToolCallCost[] {
  const out: ToolCallCost[] = [];
  let index = 0;
  for (const [i, t] of turns.entries()) {
    if (t.toolUses.length === 0) continue;
    const next = turns[i + 1];
    // Growth between this turn's context and the next is attributed to
    // this turn's tool calls together; several in one turn split it by
    // their result sizes, since the SDK bills the turn, not the call.
    const added = next ? Math.max(0, next.context - t.context) : 0;
    const later = turns.length - (i + 1);
    const totalBytes = t.toolUses.reduce((s, u) => s + (results.get(u.id) ?? 0), 0);
    for (const u of t.toolUses) {
      index++;
      const bytes = results.get(u.id) ?? 0;
      const share = t.toolUses.length === 1 ? added : totalBytes === 0 ? added / t.toolUses.length : Math.round((added * bytes) / totalBytes);
      out.push({ index, turn: i + 1, name: u.name, summary: u.summary, resultBytes: bytes, added: share, carried: share * later });
    }
  }
  return out;
}

// category names a tool call by what the agent was doing with it — the
// axis 041 §2 asks the breakdown along ("if Read of a 5,000-line file
// dominates, the cost is not giraffe's output at all"). Decided from the
// command text alone; a compound command is named by its first
// command, since that is what the agent reached for.
function category(name: string, summary: string): string {
  if (name === "Skill") return "skill";
  if (summary.includes("giraffe")) return "giraffe";
  if (name === "Read") return "read";
  const first = summary.replace(/^cd \S+ && /, "").split(/\s+/)[0] ?? "";
  if (first === "cat" || first === "sed" || first === "head" || first === "tail") return "read";
  if (first === "grep") return "grep";
  if (first === "ls" || first === "find" || first === "wc" || first === "git") return "list";
  return "other";
}

function printCategories(costs: readonly ToolCallCost[]) {
  const totals = new Map<string, { calls: number; added: number; carried: number }>();
  for (const c of costs) {
    const k = category(c.name, c.summary);
    const t = totals.get(k) ?? { calls: 0, added: 0, carried: 0 };
    t.calls++;
    t.added += c.added;
    t.carried += c.carried;
    totals.set(k, t);
  }
  const carriedAll = costs.reduce((s, c) => s + c.carried, 0) || 1;
  console.log("category  calls   added  carried  share");
  for (const [k, t] of [...totals.entries()].sort((a, b) => b[1].carried - a[1].carried)) {
    console.log(`${k.padEnd(9)} ${String(t.calls).padStart(5)}  ${String(t.added).padStart(6)}  ${String(t.carried).padStart(7)}  ${((100 * t.carried) / carriedAll).toFixed(0).padStart(4)}%`);
  }
}

function summarize(path: string, landing: LandingPoints | null): RunSummary {
  const { turns, results, skillBodies, final } = readTranscript(path);
  const f = final as { num_turns?: number; usage?: Record<string, number>; total_cost_usd?: number; result?: string } | null;
  const u = f?.usage ?? {};
  const totalTokens = (u.input_tokens ?? 0) + (u.output_tokens ?? 0) + (u.cache_creation_input_tokens ?? 0) + (u.cache_read_input_tokens ?? 0);
  const name = basename(path);
  const arm = name.includes("-aided-") ? "aided" : name.includes("-unaided-") ? "unaided" : "?";
  const skillCall = toolCallCosts(turns, results).find((c) => c.name === "Skill");
  return {
    file: name,
    arm,
    totalTokens,
    turns: f?.num_turns ?? turns.length,
    toolCalls: turns.reduce((s, t) => s + t.toolUses.length, 0),
    costUsd: f?.total_cost_usd ?? null,
    skill: skillCall ? { bytes: skillBodies[0] ?? 0, tokens: skillCall.added } : null,
    arrival: landing ? scoreArrival(f?.result ?? "", landing) : null,
  };
}

function median(xs: number[]): number {
  const s = [...xs].sort((a, b) => a - b);
  const mid = Math.floor(s.length / 2);
  const at = (i: number) => s[i] ?? 0;
  return s.length % 2 === 1 ? at(mid) : Math.round((at(mid - 1) + at(mid)) / 2);
}

function arrivalText(a: Arrival | null): string {
  return a ? `${a.hit}/${a.of}${a.missed.length ? " missed " + a.missed.join(",") : ""}` : "";
}

function printOne(path: string, landing: LandingPoints | null) {
  const { turns, results } = readTranscript(path);
  const costs = toolCallCosts(turns, results);
  const s = summarize(path, landing);
  console.log(`${s.file}: ${s.totalTokens} tokens, ${s.turns} turns, ${s.toolCalls} tool calls, $${s.costUsd?.toFixed(3)}${s.skill ? `, skill ${s.skill.bytes} bytes / ${s.skill.tokens} tokens` : ""}${s.arrival ? `, arrival ${arrivalText(s.arrival)}` : ""}`);
  console.log("");
  console.log("call  turn  added  carried  bytes  tool  command");
  for (const c of costs) {
    console.log(`${String(c.index).padStart(4)}  ${String(c.turn).padStart(4)}  ${String(c.added).padStart(5)}  ${String(c.carried).padStart(7)}  ${String(c.resultBytes).padStart(5)}  ${c.name.padEnd(5)} ${c.summary}`);
  }
  console.log("");
  console.log("ranked by carried (contribution to total tokens):");
  for (const c of [...costs].sort((a, b) => b.carried - a.carried).slice(0, 12)) {
    console.log(`  ${String(c.carried).padStart(7)}  call ${c.index} (${c.name}) ${c.summary}`);
  }
  console.log("");
  printCategories(costs);
  const contextSum = turns.reduce((s, t) => s + t.context, 0);
  console.log("");
  console.log(`context re-read total ${contextSum}, first turn context ${turns[0]?.context ?? 0}, last turn context ${turns[turns.length - 1]?.context ?? 0}`);
}

function printMany(paths: string[], landing: LandingPoints | null) {
  const rows = paths.map((p) => summarize(p, landing));
  console.log("run  arm      tokens   turns  calls  cost    skill(bytes/tokens)  arrival");
  rows.forEach((r, i) =>
    console.log(
      `${String(i + 1).padStart(3)}  ${r.arm.padEnd(8)} ${String(r.totalTokens).padStart(7)}  ${String(r.turns).padStart(5)}  ${String(r.toolCalls).padStart(5)}  $${r.costUsd?.toFixed(3)}  ${(r.skill ? `${r.skill.bytes}/${r.skill.tokens}` : "-").padStart(19)}  ${arrivalText(r.arrival)}`
    )
  );
  console.log("");
  const arms: Record<string, RunSummary[]> = {};
  for (const r of rows) (arms[r.arm] ??= []).push(r);
  const med: Record<string, number> = {};
  for (const [arm, rs] of Object.entries(arms)) {
    const toks = rs.map((r) => r.totalTokens);
    const turns = rs.map((r) => r.turns);
    const calls = rs.map((r) => r.toolCalls);
    med[arm] = median(toks);
    console.log(`${arm}: n=${rs.length} tokens median ${median(toks)} range ${Math.min(...toks)}-${Math.max(...toks)}; turns median ${median(turns)} range ${Math.min(...turns)}-${Math.max(...turns)}; calls median ${median(calls)} range ${Math.min(...calls)}-${Math.max(...calls)}`);
    const loaded = rs.filter((r) => r.skill != null);
    if (loaded.length > 0) console.log(`${arm}: skill loaded in ${loaded.length} of ${rs.length}, median ${median(loaded.map((r) => r.skill!.tokens))} tokens added`);
    if (landing) {
      const of = Object.keys(landing).length;
      const all = rs.filter((r) => r.arrival?.hit === of).length;
      const missedCounts = new Map<string, number>();
      for (const r of rs) for (const m of r.arrival?.missed ?? []) missedCounts.set(m, (missedCounts.get(m) ?? 0) + 1);
      const missedText = [...missedCounts.entries()].map(([k, v]) => `${k} ${v}`).join(", ");
      console.log(`${arm}: arrival ${of}/${of} in ${all} of ${rs.length}${missedText ? "; missed: " + missedText : ""}`);
    }
  }
  if (med.aided != null && med.unaided != null) {
    console.log(`aided median / unaided median = ${((100 * med.aided) / med.unaided).toFixed(1)}%`);
  }
}

const argv = process.argv.slice(2);
const landingAt = argv.indexOf("--landing");
const landingFile = landingAt >= 0 ? argv[landingAt + 1] : undefined;
const landing: LandingPoints | null = landingFile ? (JSON.parse(readFileSync(landingFile, "utf8")) as LandingPoints) : null;
const args = landingAt >= 0 ? argv.filter((_, i) => i !== landingAt && i !== landingAt + 1) : argv;
const only = args.length === 1 ? args[0] : undefined;
if (only !== undefined) {
  printOne(only, landing);
} else if (args.length > 1) {
  printMany(args, landing);
} else {
  console.error("usage: tsx src/analyze-arrival.ts [--landing <points.json>] <transcript.jsonl>...");
  process.exitCode = 2;
}
