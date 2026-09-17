// Directive 047 §2: reading one run in full needs what a tool call said
// and what came back, not just how long the answer was — so the reader
// analyze-arrival.ts had is here, returning the text, and both scripts
// use it (code-manners D5: the second reader would have been a copy of
// the first with one field changed).
//
// The shape is run-arrival.ts's own transcript: one SDK message per
// line, JSON, written as it arrived. Nothing here is re-derived from
// anything the agent said about itself — an assistant turn's usage and
// content blocks, and the tool results the SDK delivered back, are all
// the SDK's own record of the run.

import { readFileSync } from "node:fs";

export interface ToolUse {
  readonly id: string;
  readonly name: string;
  // The call as made: a Bash command, a Read's file_path, or the input
  // object for anything else. Verbatim and whole — a caller that wants
  // it short shortens it.
  readonly command: string;
}

export interface Turn {
  readonly id: string;
  readonly context: number; // input + cache_read + cache_creation
  readonly output: number;
  readonly toolUses: readonly ToolUse[];
  readonly text: string;
}

export interface Transcript {
  readonly turns: readonly Turn[];
  // tool_use id to the result text the model was given back. What the
  // model saw, which for a piped command is the pipeline's output and
  // not necessarily everything the program printed.
  readonly results: ReadonlyMap<string, string>;
  readonly skillBodies: readonly number[];
  readonly final: unknown;
}

// A skill's body does not arrive as a tool result (that is one line,
// "Launching skill: ..."); the SDK delivers it as a user message of its
// own, with this prefix.
const SKILL_BODY_PREFIX = "Base directory for this skill:";

export function readTranscript(path: string): Transcript {
  const turns: Turn[] = [];
  const byId = new Map<string, { context: number; output: number; toolUses: ToolUse[]; text: string }>();
  const results = new Map<string, string>();
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
          const command = typeof input.command === "string" ? input.command : typeof input.file_path === "string" ? input.file_path : JSON.stringify(input);
          t.toolUses.push({ id: block.id as string, name: block.name as string, command });
        }
        if (block.type === "text") t.text += block.text as string;
      }
    }
    if (m.type === "user") {
      for (const block of Array.isArray(m.message?.content) ? (m.message!.content as Record<string, unknown>[]) : []) {
        if (block.type === "tool_result") {
          const c = block.content;
          const text = typeof c === "string" ? c : Array.isArray(c) ? c.map((x: { text?: string }) => x.text ?? "").join("") : JSON.stringify(c);
          results.set(block.tool_use_id as string, text);
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

// The text of a run's final answer, as the SDK's result message carries
// it. Empty when the run ended without one (an error, or the turn
// ceiling).
export function finalText(final: unknown): string {
  const f = final as { result?: unknown } | null;
  return typeof f?.result === "string" ? f.result : "";
}
