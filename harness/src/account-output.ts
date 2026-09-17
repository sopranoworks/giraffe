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
  // What the next giraffe call did with the two sections that exist to
  // steer it.
  readonly truncated: Steer;
  readonly tryNext: Steer;
}

// A steering section's fate on one call. `values` is how many things it
// named that a caller could type; `hadNext` says whether there was a
// later giraffe call at all, since a section printed on a run's last
// call had no chance to be taken up and is not evidence either way.
interface Steer {
  readonly sections: number;
  readonly values: number;
  readonly hadNext: boolean;
  readonly followed: boolean;
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

// The values a steering section named, in the form a caller would type
// them.
//
// Directive 048 §2 counts how often ## try next is taken up across every
// transcript on disk, so this has to know what that section actually
// offers. It is not the whole-command form the first version assumed:
// query.Suggest and query.DiagnoseFilter write prose lines that carry
// their values at the end.
//
//   narrow by path: internal/core (5) actor (4)          ## truncated
//   names in the index starting the same way: A, B, C     ## try next
//   names in the index sharing part of the word: D, E     ## try next
//   N entries match "w" in project p ... search again from within p
//   "kind:fnc" is wrong - it is in [const, field, func]
//
// Two of ## try next's lines name nothing to type at all — `no such
// kind: "fnc"` and `every constraint in this query matches something in
// the index on its own; their combination does not`. A section with no
// value in it is counted apart rather than scored as ignored: there was
// nothing there to take up.
function offered(section: Section): string[] {
  const out: string[] = [];
  for (const line of section.text.split("\n")) {
    for (const m of line.matchAll(/([\w./-]+)\s*\((\d+)\)/g)) out.push(m[1] as string);
    for (const m of line.matchAll(/--query\s+'([^']+)'/g)) out.push(m[1] as string);
    for (const m of line.matchAll(/--page\s+(\d+)/g)) out.push(`--page ${m[1]}`);
    const names = /^names in the index [^:]*: (.+)$/.exec(line.trim());
    if (names !== null) out.push(...(names[1] as string).split(","));
    const project = /search again from within (\S+)$/.exec(line.trim());
    if (project !== null) out.push(project[1] as string);
    const wrong = /is wrong - it is in \[([^\]]*)\]/.exec(line);
    if (wrong !== null) out.push(...(wrong[1] as string).split(","));
  }
  return out.map((v) => v.trim()).filter((v) => v !== "");
}

// A value is taken up when the next call types it — present, and not as
// a fragment of a longer identifier. The boundary is the whole point of
// the test: an agent offered `Actor` that types `ActorCount` has
// answered the miss with its own next guess, which is what 047 saw it
// do, and a plain substring test would score that as having taken the
// offer.
function takenUp(command: string, value: string): boolean {
  const identifier = /[A-Za-z0-9_]/;
  for (let at = command.indexOf(value); at !== -1; at = command.indexOf(value, at + 1)) {
    const before = command[at - 1] ?? " ";
    const after = command[at + value.length] ?? " ";
    if (!identifier.test(before) && !identifier.test(after)) return true;
  }
  return false;
}

// What one steering section offered on one call, and what became of it.
function steer(sections: readonly Section[], nextGiraffe: string | undefined): Steer {
  const values = sections.flatMap(offered);
  return {
    sections: sections.length,
    values: values.length,
    hadNext: nextGiraffe !== undefined,
    followed: nextGiraffe !== undefined && values.some((v) => takenUp(nextGiraffe, v)),
  };
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
      truncated: steer(byName.get("## truncated") ?? [], nextGiraffe),
      tryNext: steer(byName.get("## try next") ?? [], nextGiraffe),
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
    const took = [call.truncated.followed ? "truncated" : "", call.tryNext.followed ? "try next" : ""].filter((f) => f !== "");
    if (took.length > 0) console.log(`    next call took up: ${took.join(", ")}`);
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
  const every: Call[] = [];
  for (const path of paths) {
    const { file, calls } = accountFor(path);
    every.push(...calls);
    const all = calls.flatMap((c) => c.results);
    const usedCount = all.filter((r) => used(r.use)).length;
    const allBytes = all.reduce((s, r) => s + r.result.bytes, 0);
    const usedBytes = all.filter((r) => used(r.use)).reduce((s, r) => s + r.result.bytes, 0);
    console.log(
      `${file.replace(".transcript.jsonl", "").padEnd(32)} ${String(calls.length).padStart(5)} ${String(calls.reduce((s, c) => s + c.bytes, 0)).padStart(8)} ${String(all.length).padStart(8)} ${String(usedCount).padStart(5)} ${String(allBytes).padStart(13)} ${String(usedBytes).padStart(11)}`
    );
  }
  printUptake(paths.length, every);
}

// Directive 048 §2/§3: over many runs at once, what became of the two
// sections that steer the next call and of the one that prints a
// declaration's text. Printed against followed, with the cases that
// could not have been followed held apart from the cases that were not.
function printUptake(runs: number, calls: readonly Call[]) {
  const printedBytes = calls.reduce((s, c) => s + c.bytes, 0);
  console.log(`\n${runs} runs, ${calls.length} giraffe calls, ${printedBytes} bytes printed`);

  for (const [name, of] of [
    ["## truncated", (c: Call) => c.truncated],
    ["## try next", (c: Call) => c.tryNext],
  ] as const) {
    const shown = calls.filter((c) => of(c).sections > 0);
    const sections = shown.reduce((s, c) => s + of(c).sections, 0);
    const nothingToType = shown.filter((c) => of(c).values === 0);
    const noNextCall = shown.filter((c) => of(c).values > 0 && !of(c).hadNext);
    const could = shown.filter((c) => of(c).values > 0 && of(c).hadNext);
    const followed = could.filter((c) => of(c).followed);
    console.log(`\n${name}: ${sections} sections on ${shown.length} calls`);
    console.log(`  named nothing a caller could type    ${String(nothingToType.length).padStart(5)}`);
    console.log(`  no later giraffe call to take it up  ${String(noNextCall.length).padStart(5)}`);
    console.log(`  could be taken up                    ${String(could.length).padStart(5)}`);
    console.log(`    the next call took a value it named ${String(followed.length).padStart(4)}`);
  }

  const srcs = calls.flatMap((c) => c.sources);
  const bytesOf = (xs: typeof srcs) => xs.reduce((s, x) => s + x.source.bytes, 0);
  const reread = srcs.filter((x) => x.use.reread);
  const openedElsewhere = srcs.filter((x) => !x.use.reread && x.use.opened);
  const citedOnly = srcs.filter((x) => !x.use.reread && !x.use.opened && x.use.cited);
  const skipped = srcs.filter((x) => !x.use.reread && !x.use.opened && !x.use.cited);
  console.log(`\n## source: ${srcs.length} blocks, ${bytesOf(srcs)} bytes`);
  console.log(`  the same range read again anyway  ${String(reread.length).padStart(5)}  ${String(bytesOf(reread)).padStart(7)} bytes`);
  console.log(`  the file opened, another part     ${String(openedElsewhere.length).padStart(5)}  ${String(bytesOf(openedElsewhere)).padStart(7)} bytes`);
  console.log(`  cited without opening             ${String(citedOnly.length).padStart(5)}  ${String(bytesOf(citedOnly)).padStart(7)} bytes`);
  console.log(`  nothing followed                  ${String(skipped.length).padStart(5)}  ${String(bytesOf(skipped)).padStart(7)} bytes`);

  const all = calls.flatMap((c) => c.results);
  const strong = all.filter((r) => usedStrongly(r.use));
  const weak = all.filter((r) => !usedStrongly(r.use) && used(r.use));
  const over = all.filter((r) => !used(r.use));
  const rb = (rs: typeof all) => rs.reduce((s, r) => s + r.result.bytes, 0);
  console.log(`\nresults: ${all.length} printed, ${rb(all)} bytes`);
  console.log(`  opened / cited / re-queried  ${String(strong.length).padStart(5)}  ${String(rb(strong)).padStart(7)} bytes`);
  console.log(`  name appears in later prose  ${String(weak.length).padStart(5)}  ${String(rb(weak)).padStart(7)} bytes`);
  console.log(`  passed over                  ${String(over.length).padStart(5)}  ${String(rb(over)).padStart(7)} bytes  ${((100 * rb(over)) / rb(all)).toFixed(1)}% of result bytes`);

  const queried = new Map<string, number>();
  for (const call of calls) {
    for (const m of call.command.matchAll(/([a-z]+):/g)) queried.set(m[1] as string, (queried.get(m[1] as string) ?? 0) + 1);
  }
  console.log(`\nattributes typed into a query: ${[...queried].sort((a, b) => b[1] - a[1]).map(([k, n]) => `${k} (${n})`).join(", ")}`);
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
