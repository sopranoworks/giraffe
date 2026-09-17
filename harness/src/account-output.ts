// Directive 047 §2: account for every byte giraffe printed in one run.
//
// 046 measured that giraffe's output is 53% of an aided run's context
// and that `## results` is 53% of that. Nobody has established how much
// of a result line is read. This script takes the transcripts
// run-arrival.ts wrote, finds every giraffe call in them, splits what
// came back into its sections and its result entries, and asks of each
// entry and each attribute whether anything in the rest of the run
// followed from it.
//
// What "followed from it" means here is mechanical and narrow, on
// purpose — it is a search of the run's later text, not a judgement of
// the agent's reasoning:
//
//   opened     a later tool call reads the result's own path (Read, or
//              cat/sed/head/tail/wc/grep naming it)
//   re-queried a later giraffe call contains the result's name
//   quoted     the result's name, or its path, appears in a later
//              assistant message or in the final answer
//   cited      a later message names a line inside the result's range
//
// A result none of the four is true of was printed and passed over. The
// converse does not hold as strongly: an agent may read a line, decide
// against it, and say nothing — "passed over" means nothing in the run
// refers to it afterwards, which is the most a transcript can say.
//
// Usage: tsx src/account-output.ts <transcript.jsonl>...
// One transcript: the call-by-call account. Several: the totals per run.

import { basename } from "node:path";
import { readTranscript, finalText, type ToolUse } from "./transcript.js";

// giraffe's own section headers (specification §7).
const SECTIONS = ["## index", "## results", "## source", "## truncated", "## try next", "## warnings"] as const;

// A result entry's header line is attributes; the lines under it, when
// there are any, are the declaration's own line and then its doc
// excerpt, in that order — a file entry has neither.
const DECL_LINE = "decl";
const DOC_LINE = "doc";

interface Section {
  readonly name: string;
  readonly bytes: number;
  readonly text: string;
}

interface Result {
  readonly name: string;
  readonly path: string;
  readonly kind: string;
  readonly range: readonly [number, number] | null;
  readonly attrs: ReadonlyMap<string, string>;
  readonly under: readonly string[]; // the lines printed under the header
  readonly bytes: number;
}

interface Use {
  readonly opened: boolean;
  readonly requeried: boolean;
  readonly quoted: boolean;
  readonly cited: boolean;
}

// A ## source block: the declaration giraffe printed the text of, and
// the extent it printed.
interface Source {
  readonly name: string;
  readonly path: string;
  readonly range: readonly [number, number] | null;
  readonly bytes: number;
}

interface SourceUse {
  // A later command reads lines overlapping the extent giraffe printed —
  // 044's finding, the read giraffe's own text was meant to save.
  readonly reread: boolean;
  readonly opened: boolean;
  readonly cited: boolean;
}

interface Call {
  readonly index: number;
  readonly command: string;
  readonly bytes: number;
  readonly sections: readonly Section[];
  readonly results: readonly { result: Result; use: Use }[];
  readonly sources: readonly { source: Source; use: SourceUse }[];
  // What the next giraffe call did with the three sections that exist to
  // steer it. Null when there was no next giraffe call.
  readonly followed: { truncated: boolean; tryNext: boolean } | null;
}

function escapeRegExp(s: string): string {
  return s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

function isGiraffe(u: ToolUse): boolean {
  return u.name === "Bash" && u.command.includes("giraffe");
}

// The printed answer, split at its own headers. Bytes are the section's
// whole extent including its header line, so the sections sum to the
// text the model was given.
function sections(text: string): Section[] {
  const out: Section[] = [];
  let name = "";
  let buf: string[] = [];
  const flush = () => {
    if (name === "" && buf.join("").trim() === "") return;
    out.push({ name: name === "" ? "(before the first section)" : name, bytes: buf.join("\n").length, text: buf.join("\n") });
  };
  for (const line of text.split("\n")) {
    if ((SECTIONS as readonly string[]).includes(line.trim())) {
      flush();
      name = line.trim();
      buf = [line];
      continue;
    }
    buf.push(line);
  }
  flush();
  return out;
}

function attributes(header: string): Map<string, string> {
  const attrs = new Map<string, string>();
  for (const token of header.trim().split(/\s+/)) {
    const at = token.indexOf(":");
    if (at <= 0) continue;
    attrs.set(token.slice(0, at), token.slice(at + 1));
  }
  return attrs;
}

function parseRange(line: string | undefined): readonly [number, number] | null {
  if (line === undefined) return null;
  const m = /^(\d+)(?:-(\d+))?$/.exec(line);
  if (m === null) return null;
  const from = Number(m[1]);
  return [from, m[2] === undefined ? from : Number(m[2])];
}

// Entries in a ## results section: a line beginning name: starts one,
// and the indented lines under it belong to it.
function results(sections: readonly Section[]): Result[] {
  const out: Result[] = [];
  for (const section of sections) {
  let header: string | null = null;
  let under: string[] = [];
  const flush = () => {
    if (header === null) return;
    const attrs = attributes(header);
    out.push({
      name: attrs.get("name") ?? "",
      path: attrs.get("path") ?? "",
      kind: attrs.get("kind") ?? "",
      range: parseRange(attrs.get("line")),
      attrs,
      under,
      bytes: [header, ...under].join("\n").length,
    });
  };
  for (const line of section.text.split("\n")) {
    if (line.startsWith("name:")) {
      flush();
      header = line;
      under = [];
      continue;
    }
    if (header !== null && line.trim() !== "") under.push(line);
  }
  flush();
  header = null;
  }
  return out;
}

// The blocks of a ## source section. Each begins with a header line of
// three attributes and runs to the next header or the section's end.
function sources(sections: readonly Section[]): Source[] {
  const out: Source[] = [];
  for (const section of sections) {
  let header: Map<string, string> | null = null;
  let under: string[] = [];
  const flush = () => {
    if (header === null) return;
    out.push({
      name: header.get("name") ?? "",
      path: header.get("path") ?? "",
      range: parseRange(header.get("line")),
      bytes: under.join("\n").length,
    });
  };
  for (const line of section.text.split("\n")) {
    if (line.startsWith("name:") && line.includes("path:")) {
      flush();
      header = attributes(line);
      under = [line];
      continue;
    }
    if (header !== null) under.push(line);
  }
  flush();
  header = null;
  }
  return out;
}

// A later command that reads lines overlapping [from, to] of path: sed's
// own range form, which is what the agents use, and head -N counted from
// the top.
function readsRange(command: string, path: string, range: readonly [number, number] | null): boolean {
  if (range === null || !command.includes(basename(path))) return false;
  for (const m of command.matchAll(/(\d+),\s*(\d+)p/g)) {
    if (Number(m[1]) <= range[1] && Number(m[2]) >= range[0]) return true;
  }
  for (const m of command.matchAll(/head -n? ?(\d+)/g)) {
    if (Number(m[1]) >= range[0]) return true;
  }
  return false;
}

// A command that reads a named file: Read's own path, or one of the
// shell readers with the path as an argument.
function reads(command: string, path: string): boolean {
  const named = new RegExp(`(^|[\\s'"/])${escapeRegExp(basename(path))}(\\s|$|['"])`);
  if (!named.test(command)) return false;
  return /\b(cat|sed|head|tail|wc|grep|awk)\b/.test(command) || !command.includes(" ");
}

function namesWord(text: string, word: string): boolean {
  if (word === "") return false;
  return new RegExp(`\\b${escapeRegExp(word)}\\b`).test(text);
}

function citesRange(text: string, path: string, range: readonly [number, number] | null): boolean {
  if (range === null) return false;
  const file = escapeRegExp(basename(path));
  const re = new RegExp(`${file}[:#\\s]*(?:line[s]?\\s*)?(\\d+)`, "g");
  for (const m of text.matchAll(re)) {
    const n = Number(m[1]);
    if (n >= range[0] && n <= range[1]) return true;
  }
  return false;
}

function useOf(result: Result, laterCommands: readonly string[], laterGiraffe: readonly string[], laterText: string): Use {
  return {
    opened: laterCommands.some((c) => reads(c, result.path)) || laterCommands.some((c) => c.endsWith(result.path)),
    requeried: laterGiraffe.some((c) => namesWord(c, result.name)),
    quoted: namesWord(laterText, result.name) || laterText.includes(basename(result.path)),
    cited: citesRange(laterText, result.path, result.range),
  };
}

function used(u: Use): boolean {
  return u.opened || u.requeried || u.quoted || u.cited;
}

// Opening the path, citing a line in the range, or typing the name into
// another query are acts: they cannot happen by accident. A name that
// merely appears in later prose is weaker evidence — "metrics" is a word
// the task itself uses — so the two are counted apart and never summed
// into one "used" figure in the totals.
function usedStrongly(u: Use): boolean {
  return u.opened || u.requeried || u.cited;
}

// Did the next giraffe call take up what ## truncated and ## try next
// offered? ## truncated ends in a distribution ("narrow by path: x (3)
// ...") and ## try next in whole commands; both are taken up when the
// next call contains one of the values they named.
function offered(section: Section): string[] {
  const out: string[] = [];
  for (const m of section.text.matchAll(/([\w./-]+)\s*\((\d+)\)/g)) out.push(m[1] as string);
  for (const m of section.text.matchAll(/--query\s+'([^']+)'/g)) out.push(m[1] as string);
  for (const m of section.text.matchAll(/--page\s+(\d+)/g)) out.push(`--page ${m[1]}`);
  return out;
}

function accountFor(path: string): { file: string; calls: Call[]; finalAnswer: string } {
  const { turns, results: resultText, final } = readTranscript(path);
  const calls: ToolUse[] = [];
  const turnOf = new Map<string, number>();
  for (const [i, t] of turns.entries()) {
    for (const u of t.toolUses) {
      calls.push(u);
      turnOf.set(u.id, i);
    }
  }
  const answer = finalText(final);
  const out: Call[] = [];
  for (const [i, u] of calls.entries()) {
    if (!isGiraffe(u)) continue;
    const printed = resultText.get(u.id) ?? "";
    const later = calls.slice(i + 1);
    const laterCommands = later.map((c) => c.command);
    const laterGiraffe = later.filter(isGiraffe).map((c) => c.command);
    const from = (turnOf.get(u.id) ?? 0) + 1;
    const laterText = turns.slice(from).map((t) => t.text).join("\n") + "\n" + answer;
    const secs = sections(printed);
    // One Bash call can invoke giraffe more than once — the agents chain
    // them with `; echo ===; giraffe ...` — so a section name appears as
    // often as there were invocations, and all of them count.
    const byName = new Map<string, Section[]>();
    for (const sec of secs) byName.set(sec.name, [...(byName.get(sec.name) ?? []), sec]);
    const nextGiraffe = laterGiraffe[0];
    out.push({
      index: i + 1,
      command: u.command.replace(/\s+/g, " "),
      bytes: printed.length,
      sections: secs,
      results: results(byName.get("## results") ?? []).map((r) => ({ result: r, use: useOf(r, laterCommands, laterGiraffe, laterText) })),
      sources: sources(byName.get("## source") ?? []).map((src) => ({
        source: src,
        use: {
          reread: laterCommands.some((c) => readsRange(c, src.path, src.range)),
          opened: laterCommands.some((c) => reads(c, src.path)) || laterCommands.some((c) => c.endsWith(src.path)),
          cited: citesRange(laterText, src.path, src.range),
        },
      })),
      followed:
        nextGiraffe === undefined
          ? null
          : {
              truncated: (byName.get("## truncated") ?? []).flatMap(offered).some((v) => nextGiraffe.includes(v)),
              tryNext: (byName.get("## try next") ?? []).flatMap(offered).some((v) => nextGiraffe.includes(v)),
            },
    });
  }
  return { file: basename(path), calls: out, finalAnswer: answer };
}

function attributeBytes(calls: readonly Call[]): Map<string, { bytes: number; onUsed: number; count: number }> {
  const totals = new Map<string, { bytes: number; onUsed: number; count: number }>();
  const add = (key: string, bytes: number, onUsed: boolean) => {
    const t = totals.get(key) ?? { bytes: 0, onUsed: 0, count: 0 };
    totals.set(key, { bytes: t.bytes + bytes, onUsed: t.onUsed + (onUsed ? bytes : 0), count: t.count + 1 });
  };
  for (const call of calls) {
    for (const { result, use } of call.results) {
      for (const [k, v] of result.attrs) add(k, `${k}:${v} `.length, used(use));
      for (const [i, line] of result.under.entries()) add(i === 0 ? DECL_LINE : DOC_LINE, line.length + 1, used(use));
    }
  }
  return totals;
}

// Which attribute names the run itself typed into a later query — the
// one unambiguous evidence that an attribute was read, as opposed to
// merely printed.
function attributesQueried(calls: readonly Call[]): Set<string> {
  const queried = new Set<string>();
  for (const call of calls) {
    for (const m of call.command.matchAll(/([a-z]+):/g)) queried.add(m[1] as string);
  }
  return queried;
}

function printOne(path: string) {
  const { file, calls } = accountFor(path);
  console.log(`# ${file}`);
  console.log(`${calls.length} giraffe calls, ${calls.reduce((s, c) => s + c.bytes, 0)} bytes printed\n`);
  for (const call of calls) {
    console.log(`call ${call.index}: ${call.command}`);
    console.log(`  ${call.bytes} bytes: ${call.sections.map((s) => `${s.name.replace("## ", "")} ${s.bytes}`).join(", ")}`);
    for (const { result, use } of call.results) {
      const flags = [use.opened ? "opened" : "", use.cited ? "cited" : "", use.quoted ? "quoted" : "", use.requeried ? "re-queried" : ""].filter((f) => f !== "");
      console.log(`    ${String(result.bytes).padStart(4)}b  ${flags.length === 0 ? "PASSED OVER" : flags.join("+")}  name:${result.name} kind:${result.kind} path:${result.path}`);
    }
    for (const { source, use } of call.sources) {
      const flags = [use.reread ? "range re-read" : "", use.opened ? "file opened" : "", use.cited ? "cited" : ""].filter((f) => f !== "");
      console.log(`    ${String(source.bytes).padStart(4)}b  source ${flags.length === 0 ? "SKIPPED" : flags.join("+")}  name:${source.name} path:${source.path} line:${source.range?.join("-") ?? "-"}`);
    }
    if (call.followed !== null) {
      const took = [call.followed.truncated ? "truncated" : "", call.followed.tryNext ? "try next" : ""].filter((f) => f !== "");
      if (took.length > 0) console.log(`    next call took up: ${took.join(", ")}`);
    }
  }
  const totals = attributeBytes(calls);
  const queried = attributesQueried(calls);
  console.log(`\nattribute            bytes   on a used result   typed into a later query`);
  for (const [key, t] of [...totals].sort((a, b) => b[1].bytes - a[1].bytes)) {
    console.log(`  ${key.padEnd(14)} ${String(t.bytes).padStart(7)} ${String(t.onUsed).padStart(18)}   ${queried.has(key) ? "yes" : "no"}`);
  }
  const bySection = new Map<string, number>();
  for (const call of calls) {
    for (const sec of call.sections) bySection.set(sec.name, (bySection.get(sec.name) ?? 0) + sec.bytes);
  }
  console.log(`\nsection                bytes   share`);
  const printed = calls.reduce((s, c) => s + c.bytes, 0);
  for (const [name, bytes] of [...bySection].sort((a, b) => b[1] - a[1])) {
    console.log(`  ${name.padEnd(18)} ${String(bytes).padStart(7)}   ${((100 * bytes) / printed).toFixed(1)}%`);
  }

  const srcs = calls.flatMap((c) => c.sources);
  const srcBytes = (xs: typeof srcs) => xs.reduce((s, x) => s + x.source.bytes, 0);
  const rereadSrc = srcs.filter((x) => x.use.reread);
  const openedSrc = srcs.filter((x) => !x.use.reread && x.use.opened);
  const skippedSrc = srcs.filter((x) => !x.use.reread && !x.use.opened && !x.use.cited);
  console.log(`\n## source: ${srcs.length} blocks, ${srcBytes(srcs)} bytes`);
  console.log(`  the same range read again anyway  ${String(rereadSrc.length).padStart(3)}  ${String(srcBytes(rereadSrc)).padStart(5)} bytes`);
  console.log(`  the file opened, another part     ${String(openedSrc.length).padStart(3)}  ${String(srcBytes(openedSrc)).padStart(5)} bytes`);
  console.log(`  nothing followed                  ${String(skippedSrc.length).padStart(3)}  ${String(srcBytes(skippedSrc)).padStart(5)} bytes`);

  const all = calls.flatMap((c) => c.results);
  const strong = all.filter((r) => usedStrongly(r.use));
  const weak = all.filter((r) => !usedStrongly(r.use) && used(r.use));
  const over = all.filter((r) => !used(r.use));
  const bytesOf = (rs: typeof all) => rs.reduce((s, r) => s + r.result.bytes, 0);
  console.log(`\nresults: ${all.length} printed, ${bytesOf(all)} bytes`);
  console.log(`  opened / cited / re-queried  ${String(strong.length).padStart(3)}  ${String(bytesOf(strong)).padStart(5)} bytes`);
  console.log(`  name appears in later prose  ${String(weak.length).padStart(3)}  ${String(bytesOf(weak)).padStart(5)} bytes`);
  console.log(`  passed over                  ${String(over.length).padStart(3)}  ${String(bytesOf(over)).padStart(5)} bytes`);
}

function printMany(paths: readonly string[]) {
  console.log(`run                              calls  printed  results  used  result bytes  used bytes`);
  for (const path of paths) {
    const { file, calls } = accountFor(path);
    const all = calls.flatMap((c) => c.results);
    const usedCount = all.filter((r) => used(r.use)).length;
    const allBytes = all.reduce((s, r) => s + r.result.bytes, 0);
    const usedBytes = all.filter((r) => used(r.use)).reduce((s, r) => s + r.result.bytes, 0);
    console.log(
      `${file.replace(".transcript.jsonl", "").padEnd(32)} ${String(calls.length).padStart(5)} ${String(calls.reduce((s, c) => s + c.bytes, 0)).padStart(8)} ${String(all.length).padStart(8)} ${String(usedCount).padStart(5)} ${String(allBytes).padStart(13)} ${String(usedBytes).padStart(11)}`
    );
  }
}

const args = process.argv.slice(2);
if (args.length === 0) {
  console.error("usage: tsx src/account-output.ts <transcript.jsonl>...");
  process.exitCode = 1;
} else if (args.length === 1) {
  printOne(args[0] as string);
} else {
  printMany(args);
}
