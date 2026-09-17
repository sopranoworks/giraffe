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
// Usage: tsx src/account-output.ts [--landing <points.json>] <transcript.jsonl>...
// One transcript: the call-by-call account. Several: the totals per run.
//
// Directive 049 §1–§3 asks a question of every tool call, not only
// giraffe's: what each one was — a giraffe call whose answer was used
// or was not, a read of a file giraffe had named or had not, a search,
// a listing — what it cost in tokens, and at which call the run first
// put one of the task's landing points on the page. --landing names the
// points, as analyze-arrival.ts takes them; without it the landing
// columns are blank.

import { basename } from "node:path";
import { readTranscript, finalText, toolCallCosts, type ToolUse, type Transcript } from "./transcript.js";
import { namesLine, namesPath, readLandingPoints, type LandingPoint, type LandingPoints } from "./landing.js";

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
  const transcript = readTranscript(path);
  const { calls, finalAnswer } = giraffeCalls(transcript);
  return { file: basename(path), calls, finalAnswer };
}

// Every giraffe call of a run, with what became of what it printed.
function giraffeCalls(transcript: Transcript): { calls: Call[]; finalAnswer: string } {
  const { turns, results: resultText, final } = transcript;
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
  return { calls: out, finalAnswer: answer };
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

// The three tables both accounts end with. printOne answers them for
// one run and printUptake for a round or for all eight; they are the
// same tables over the same shape, so they are written once (D6 — the
// cross-run account was the second copy).
function printSectionBytes(calls: readonly Call[]) {
  const printed = calls.reduce((s, c) => s + c.bytes, 0);
  const bySection = new Map<string, number>();
  for (const call of calls) {
    for (const sec of call.sections) bySection.set(sec.name, (bySection.get(sec.name) ?? 0) + sec.bytes);
  }
  console.log(`\nsection                bytes   share`);
  for (const [name, bytes] of [...bySection].sort((a, b) => b[1] - a[1])) {
    console.log(`  ${name.padEnd(18)} ${String(bytes).padStart(9)}   ${((100 * bytes) / printed).toFixed(1)}%`);
  }
}

function printSourceTotals(calls: readonly Call[]) {
  const srcs = calls.flatMap((c) => c.sources);
  const bytesOf = (xs: typeof srcs) => xs.reduce((s, x) => s + x.source.bytes, 0);
  const reread = srcs.filter((x) => x.use.reread);
  const openedElsewhere = srcs.filter((x) => !x.use.reread && x.use.opened);
  const citedOnly = srcs.filter((x) => !x.use.reread && !x.use.opened && x.use.cited);
  const skipped = srcs.filter((x) => !x.use.reread && !x.use.opened && !x.use.cited);
  console.log(`\n## source: ${srcs.length} blocks, ${bytesOf(srcs)} bytes`);
  for (const [label, xs] of [
    ["the same range read again anyway", reread],
    ["the file opened, another part", openedElsewhere],
    ["cited without opening", citedOnly],
    ["nothing followed", skipped],
  ] as const) {
    console.log(`  ${label.padEnd(34)}${String(xs.length).padStart(5)}  ${String(bytesOf(xs)).padStart(7)} bytes`);
  }
}

function printResultTotals(calls: readonly Call[]) {
  const all = calls.flatMap((c) => c.results);
  const bytesOf = (rs: typeof all) => rs.reduce((s, r) => s + r.result.bytes, 0);
  const strong = all.filter((r) => usedStrongly(r.use));
  const weak = all.filter((r) => !usedStrongly(r.use) && used(r.use));
  const over = all.filter((r) => !used(r.use));
  console.log(`\nresults: ${all.length} printed, ${bytesOf(all)} bytes`);
  console.log(`  opened / cited / re-queried  ${String(strong.length).padStart(5)}  ${String(bytesOf(strong)).padStart(7)} bytes`);
  console.log(`  name appears in later prose  ${String(weak.length).padStart(5)}  ${String(bytesOf(weak)).padStart(7)} bytes`);
  console.log(`  passed over                  ${String(over.length).padStart(5)}  ${String(bytesOf(over)).padStart(7)} bytes  ${((100 * bytesOf(over)) / bytesOf(all)).toFixed(1)}% of result bytes`);
}

// ---------------------------------------------------------------------
// Directive 049 §1–§3: every call of a run, named by what it did.

// What a tool call was. The giraffe kinds are decided by what became of
// the results it printed (§1: "a giraffe call whose results were used by
// none"); a read is named or unnamed by whether an earlier giraffe call
// had printed the file's path on a result line or a ## source header.
type StepKind =
  | "skill"
  | "giraffe help"
  | "giraffe nothing found"
  | "giraffe used"
  | "giraffe none used"
  | "read named"
  | "read unnamed"
  | "search"
  | "list"
  | "other";

// How a read was shaped: the whole file, a range of it, or a grep
// through it.
type ReadShape = "whole" | "ranged" | "grep";

interface Step {
  readonly index: number;
  readonly kind: StepKind;
  // The call with the noise taken out — the binary's path, the pipes
  // through head, the echo separators — so two runs' calls can be
  // compared for being the same call.
  readonly signature: string;
  readonly bytes: number;
  readonly added: number;
  readonly carried: number;
  readonly files: readonly string[];
  readonly shape: ReadShape | null;
  // A whole-file read of a file some earlier giraffe result had printed
  // a line: range for — 043's finding, the range on the page and the
  // file read whole regardless.
  readonly rangeOnPage: boolean;
  readonly landed: readonly string[];
  // For a giraffe call: how many results it printed, and whether it was
  // truncated. Zero results with no truncation is a miss.
  readonly resultCount: number;
  readonly truncated: boolean;
  // The two shapes of leaving giraffe for grep: a search whose pattern
  // is a name giraffe had already printed (its callers — --help says
  // "not indexed: call sites - grep for those"), and a read of a file
  // giraffe never named but a search's output had.
  readonly follows: "a printed name" | "a search hit" | null;
}

interface Run {
  readonly file: string;
  readonly tokens: number;
  readonly steps: readonly Step[];
  // The index of the first step that put any landing point on the page,
  // and of the step by which every point had been; null when never.
  readonly firstLanding: number | null;
  readonly allLanding: number | null;
  // Per landing point, the first step that put it on the page.
  readonly landings: ReadonlyMap<string, Step>;
}

// The file extensions a command's arguments are read as files by. A
// grep pattern like c.actors would pass a looser test, and so would the
// _test.go of a `grep -v "_test.go"`, which is why a name starts with a
// letter or a digit.
const FILE_EXTENSIONS = ["go", "md", "json", "yaml", "yml", "toml", "txt", "proto", "sh", "mod", "sum", "html", "ts"];
const FILE_TOKEN = new RegExp(`(?<![\\w./=*-])(/?(?:[\\w.-]+/)*[A-Za-z0-9][\\w-]*(?:\\.[\\w-]+)*\\.(?:${FILE_EXTENSIONS.join("|")}))(?![\\w/-])`, "g");

// The corpus is mounted at /corpus in the container and giraffe prints
// paths relative to it; the agents type both.
function relative(path: string): string {
  return path.replace(/^\/corpus\//, "").replace(/^\.\//, "");
}

function samePath(a: string, b: string): boolean {
  const x = relative(a);
  const y = relative(b);
  return x === y || x.endsWith("/" + y) || y.endsWith("/" + x);
}

function filesRead(u: ToolUse): string[] {
  if (u.name === "Read") return [relative(u.command)];
  if (u.name !== "Bash") return [];
  return [...new Set([...u.command.matchAll(FILE_TOKEN)].map((m) => relative(m[1] as string)))];
}

function overlaps(a: readonly [number, number], b: readonly [number, number]): boolean {
  return a[0] <= b[1] && a[1] >= b[0];
}

// The line ranges a read asked for: Read's offset and limit, sed's A,Bp,
// head -N from the top. Empty for a whole-file read.
function readRanges(u: ToolUse): (readonly [number, number])[] {
  if (u.name === "Read") {
    const offset = typeof u.input.offset === "number" ? u.input.offset : 1;
    const limit = typeof u.input.limit === "number" ? u.input.limit : null;
    return u.input.offset === undefined && limit === null ? [] : [[offset, limit === null ? Number.MAX_SAFE_INTEGER : offset + limit - 1]];
  }
  const out: (readonly [number, number])[] = [];
  for (const m of u.command.matchAll(/(\d+),\s*(\d+)p/g)) out.push([Number(m[1]), Number(m[2])]);
  for (const m of u.command.matchAll(/head -n? ?(\d+)/g)) out.push([1, Number(m[1])]);
  return out;
}

function readShape(u: ToolUse): ReadShape | null {
  if (u.name === "Read") return readRanges(u).length === 0 ? "whole" : "ranged";
  if (u.name !== "Bash") return null;
  const c = u.command;
  if (/\b(sed|head|tail|awk)\b/.test(c)) return "ranged";
  if (/\bcat\b/.test(c)) return "whole";
  if (/\b(grep|rg)\b/.test(c)) return "grep";
  return null;
}

function stepKindOf(u: ToolUse, call: Call | undefined, files: readonly string[], named: boolean): StepKind {
  if (u.name === "Skill") return "skill";
  if (call !== undefined) {
    if (call.results.some((r) => usedStrongly(r.use))) return "giraffe used";
    if (call.results.length > 0) return "giraffe none used";
    if (call.tryNext.sections > 0) return "giraffe nothing found";
    return "giraffe help";
  }
  if (files.length > 0) return named ? "read named" : "read unnamed";
  const first = u.command.replace(/^cd \S+ *(&&|;) */, "").split(/\s+/)[0] ?? "";
  if (first === "grep" || first === "rg" || first === "find") return "search";
  if (first === "ls" || first === "wc" || first === "git" || first === "tree") return "list";
  return "other";
}

function signatureOf(u: ToolUse, kind: StepKind, files: readonly string[]): string {
  if (kind.startsWith("giraffe")) {
    return u.command
      .replace(/\S*giraffe\b/g, "giraffe")
      .replace(/cd \S+ *(&&|;) */g, "")
      .replace(/2>&1/g, "")
      .replace(/\|\s*(head|sed|grep|tail)[^;|]*/g, "")
      .replace(/echo\s+("[^"]*"|'[^']*'|\S+)\s*(;|&&)?/g, "")
      .replace(/\s*;\s*/g, " ; ")
      .replace(/\s+/g, " ")
      .replace(/ ; $/, "")
      .trim();
  }
  if (kind.startsWith("read")) return `${kind} ${files.join(" ")}`;
  if (kind === "search") {
    const pattern = /(?:grep|rg)\s+(?:-\S+\s+)*("[^"]*"|'[^']*'|\S+)/.exec(u.command);
    return `grep ${pattern?.[1] ?? ""}`;
  }
  return kind;
}

// The landing points a step put on the page. A giraffe call names one
// by printing its path (and, for a range, an overlapping line:); a read
// by reading the file (the whole of it, or lines overlapping the
// range); a search by an output line that names the place the way an
// answer would. Listings and the skill load name nothing.
function landedOn(u: ToolUse, kind: StepKind, call: Call | undefined, files: readonly string[], output: string, points: LandingPoints): string[] {
  const out: string[] = [];
  const atPlace = (path: string, range: readonly [number, number] | null, p: LandingPoint) =>
    samePath(path, p.path) && (p.lines === undefined || (range !== null && overlaps(range, p.lines)));
  for (const [name, p] of Object.entries(points)) {
    let there = false;
    if (call !== undefined) {
      // A result line or a ## source header at the place — or, since
      // directive 050, a callers: line naming it as path:line, which
      // the same test a search's output gets recognises.
      there =
        call.results.some((r) => atPlace(r.result.path, r.result.range, p)) ||
        call.sources.some((s) => atPlace(s.source.path, s.source.range, p)) ||
        (p.lines === undefined ? namesPath(output, p.path) : namesLine(output, p.path, p.lines));
    } else if (kind.startsWith("read")) {
      const ranges = readRanges(u);
      const whole = readShape(u) === "whole";
      there = files.some((f) => samePath(f, p.path)) && (p.lines === undefined || whole || ranges.some((r) => overlaps(r, p.lines!)) || namesLine(output, p.path, p.lines));
    } else if (kind === "search" || kind === "other") {
      there = p.lines === undefined ? namesPath(output, p.path) : namesLine(output, p.path, p.lines);
    }
    if (there) out.push(name);
  }
  return out;
}

function runOf(file: string, transcript: Transcript, calls: readonly Call[], landing: LandingPoints | null): Run {
  const costs = toolCallCosts(transcript.turns, transcript.results);
  const byIndex = new Map(calls.map((c) => [c.index, c]));
  const uses = transcript.turns.flatMap((t) => t.toolUses);
  const printed = new Set<string>();
  const ranged = new Set<string>();
  const names = new Set<string>();
  let lastSearchOutput = "";
  const steps: Step[] = [];
  const landings = new Map<string, Step>();
  let firstLanding: number | null = null;
  let allLanding: number | null = null;
  for (const [i, u] of uses.entries()) {
    const index = i + 1;
    const call = byIndex.get(index);
    const cost = costs[i];
    const files = call === undefined ? filesRead(u) : [];
    const named = files.some((f) => [...printed].some((p) => samePath(f, p)));
    const kind = stepKindOf(u, call, files, named);
    const shape = kind.startsWith("read") ? readShape(u) : null;
    const rangeOnPage = shape === "whole" && files.some((f) => [...ranged].some((p) => samePath(f, p)));
    const output = transcript.results.get(u.id) ?? "";
    const landed = landing === null ? [] : landedOn(u, kind, call, files, output, landing);
    const follows =
      kind === "search" && [...names].some((n) => n.length > 2 && new RegExp(`\\b${escapeRegExp(n)}\\b`).test(u.command))
        ? "a printed name"
        : kind === "read unnamed" && files.some((f) => namesPath(lastSearchOutput, f))
          ? "a search hit"
          : null;
    if (kind === "search") lastSearchOutput = output;
    if (landed.length > 0 && firstLanding === null) firstLanding = index;
    if (call !== undefined) {
      for (const r of call.results) {
        printed.add(r.result.path);
        names.add(r.result.name);
        if (r.result.range !== null) ranged.add(r.result.path);
      }
      for (const s of call.sources) {
        printed.add(s.source.path);
        if (s.source.range !== null) ranged.add(s.source.path);
      }
    }
    const step: Step = {
      index,
      kind,
      signature: signatureOf(u, kind, files),
      bytes: output.length,
      added: cost?.added ?? 0,
      carried: cost?.carried ?? 0,
      files,
      shape,
      rangeOnPage,
      landed,
      resultCount: call?.results.length ?? 0,
      truncated: (call?.truncated.sections ?? 0) > 0,
      follows,
    };
    steps.push(step);
    for (const name of landed) if (!landings.has(name)) landings.set(name, step);
    if (landing !== null && allLanding === null && landings.size === Object.keys(landing).length) allLanding = index;
  }
  const f = transcript.final as { usage?: Record<string, number> } | null;
  const usage = f?.usage ?? {};
  const tokens = (usage.input_tokens ?? 0) + (usage.output_tokens ?? 0) + (usage.cache_creation_input_tokens ?? 0) + (usage.cache_read_input_tokens ?? 0);
  return { file, tokens, steps, firstLanding, allLanding, landings };
}

const KINDS: readonly StepKind[] = ["skill", "giraffe help", "giraffe nothing found", "giraffe used", "giraffe none used", "read named", "read unnamed", "search", "list", "other"];
const KIND_CODE: Record<StepKind, string> = {
  skill: "S",
  "giraffe help": "H",
  "giraffe nothing found": "G0",
  "giraffe used": "G+",
  "giraffe none used": "G-",
  "read named": "Rn",
  "read unnamed": "Ru",
  search: "Q",
  list: "L",
  other: "O",
};

function mean(xs: readonly number[]): number {
  return xs.length === 0 ? 0 : xs.reduce((s, x) => s + x, 0) / xs.length;
}

function count(steps: readonly Step[], kind: StepKind): number {
  return steps.filter((s) => s.kind === kind).length;
}

function wholeReads(run: Run): number {
  return run.steps.filter((s) => s.shape === "whole").length;
}

// §1: the per-call classification, per run and pooled; the calls before
// and after the first landing point; the cheapest third of runs against
// the dearest, by calls; and at each call position, how far the runs
// still agree on what to ask.
function printSteps(runs: readonly Run[]) {
  const codes = KINDS.map((k) => KIND_CODE[k]);
  console.log(`\n# every call, by what it did (${runs.length} runs)`);
  console.log(`  ${codes.map((c) => c.padStart(3)).join("")}  S skill, H giraffe --help, G0 giraffe found nothing, G+ giraffe result used, G- giraffe nothing used,`);
  console.log(`  ${"".padStart(3 * codes.length)}  Rn read of a file giraffe named, Ru read of one it had not, Q search, L listing, O other; * marks the first landing point`);
  console.log(`\nrun                            tokens calls ${codes.map((c) => c.padStart(3)).join("")} whole  first  all  sequence`);
  for (const r of runs) {
    const seq = r.steps.map((s) => KIND_CODE[s.kind] + (s.index === r.firstLanding ? "*" : "")).join(" ");
    console.log(
      `${r.file.replace(".transcript.jsonl", "").padEnd(28)} ${String(r.tokens).padStart(8)} ${String(r.steps.length).padStart(5)} ${KINDS.map((k) => String(count(r.steps, k)).padStart(3)).join("")} ${String(wholeReads(r)).padStart(5)}  ${String(r.firstLanding ?? "-").padStart(5)} ${String(r.allLanding ?? "-").padStart(4)}  ${seq}`
    );
  }

  printPooledKinds(runs);
  printLanding(runs);
  printThirds(runs);
  printPositions(runs);
}

function printPooledKinds(runs: readonly Run[]) {
  const all = runs.flatMap((r) => r.steps);
  const carriedAll = all.reduce((s, x) => s + x.carried, 0) || 1;
  console.log(`\npooled: ${all.length} calls over ${runs.length} runs`);
  console.log(`  kind                     calls  per run    added tokens  carried tokens  share`);
  for (const k of KINDS) {
    const xs = all.filter((s) => s.kind === k);
    const carried = xs.reduce((s, x) => s + x.carried, 0);
    console.log(`  ${k.padEnd(24)} ${String(xs.length).padStart(5)}  ${mean(runs.map((r) => count(r.steps, k))).toFixed(1).padStart(7)}  ${String(xs.reduce((s, x) => s + x.added, 0)).padStart(14)}  ${String(carried).padStart(14)}  ${((100 * carried) / carriedAll).toFixed(1).padStart(5)}%`);
  }
}

// Where the landing points were first put on the page, and by what.
function printLanding(runs: readonly Run[]) {
  const codes = KINDS.map((k) => KIND_CODE[k]);
  const landed = runs.filter((r) => r.firstLanding !== null);
  if (landed.length > 0) {
    console.log(`\nfirst landing point on the page: ${landed.length} of ${runs.length} runs`);
    console.log(`  calls before and including it   mean ${mean(landed.map((r) => r.firstLanding!)).toFixed(1)}   median ${median(landed.map((r) => r.firstLanding!))}`);
    console.log(`  calls after it                  mean ${mean(landed.map((r) => r.steps.length - r.firstLanding!)).toFixed(1)}   median ${median(landed.map((r) => r.steps.length - r.firstLanding!))}`);
    const every = runs.filter((r) => r.allLanding !== null);
    console.log(`  every point on the page         ${every.length} runs; at call mean ${mean(every.map((r) => r.allLanding!)).toFixed(1)}, with mean ${mean(every.map((r) => r.steps.length - r.allLanding!)).toFixed(1)} calls after`);
    const names = [...new Set(runs.flatMap((r) => [...r.landings.keys()]))];
    console.log(`\n  point                 runs   first on the page at call (median)   put there by`);
    for (const name of names) {
      const at = runs.filter((r) => r.landings.has(name)).map((r) => r.landings.get(name)!);
      const by = new Map<string, number>();
      for (const s of at) by.set(s.kind, (by.get(s.kind) ?? 0) + 1);
      console.log(`  ${name.padEnd(20)} ${String(at.length).padStart(5)}   ${String(median(at.map((s) => s.index))).padStart(6)}                              ${[...by].sort((a, b) => b[1] - a[1]).map(([k, n]) => `${k} ${n}`).join(", ")}`);
    }
    const byKind = (rs: readonly Run[], pick: (r: Run) => readonly Step[]) => KINDS.map((k) => mean(rs.map((r) => count(pick(r), k))).toFixed(1).padStart(5)).join("");
    console.log(`\n  per run, mean calls          ${codes.map((c) => c.padStart(5)).join("")}`);
    console.log(`  before the first point       ${byKind(landed, (r) => r.steps.slice(0, r.firstLanding!))}`);
    console.log(`  after it                     ${byKind(landed, (r) => r.steps.slice(r.firstLanding!))}`);
  }
}

// The cheapest third of runs against the dearest, by calls, and the
// class of calls that leaves giraffe for grep.
function printThirds(runs: readonly Run[]) {
  const codes = KINDS.map((k) => KIND_CODE[k]);
  const all = runs.flatMap((r) => r.steps);
  const sorted = [...runs].sort((a, b) => a.steps.length - b.steps.length);
  const third = Math.floor(runs.length / 3);
  if (third > 0) {
    console.log(`\ncheapest third against dearest third, by calls (${third} runs each)`);
    console.log(`  third      calls   tokens  ${codes.map((c) => c.padStart(5)).join("")}  whole  first  after`);
    for (const [label, rs] of [
      ["cheapest", sorted.slice(0, third)],
      ["middle", sorted.slice(third, runs.length - third)],
      ["dearest", sorted.slice(runs.length - third)],
    ] as const) {
      const first = rs.filter((r) => r.firstLanding !== null);
      console.log(
        `  ${label.padEnd(9)} ${mean(rs.map((r) => r.steps.length)).toFixed(1).padStart(6)} ${Math.round(mean(rs.map((r) => r.tokens))).toString().padStart(8)}  ${KINDS.map((k) => mean(rs.map((r) => count(r.steps, k))).toFixed(1).padStart(5)).join("")}  ${mean(rs.map(wholeReads)).toFixed(1).padStart(5)}  ${mean(first.map((r) => r.firstLanding!)).toFixed(1).padStart(5)}  ${mean(first.map((r) => r.steps.length - r.firstLanding!)).toFixed(1).padStart(5)}`
      );
    }
  }

  const topOf = (label: string, xs: readonly Step[], key: (s: Step) => string) => {
    const t = new Map<string, number>();
    for (const x of xs) t.set(key(x), (t.get(key(x)) ?? 0) + 1);
    console.log(`\n${label} (${xs.length}): ${[...t].sort((a, b) => b[1] - a[1]).slice(0, 14).map(([k, n]) => `${k} ${n}`).join(", ")}`);
  };
  topOf("searches, by pattern", all.filter((s) => s.kind === "search"), (s) => s.signature.replace(/^grep /, ""));
  topOf("reads of files giraffe had not named, by file", all.filter((s) => s.kind === "read unnamed"), (s) => s.files.join(" "));
  const hunt = (rs: readonly Run[]) => {
    const xs = rs.flatMap((r) => r.steps);
    const searches = xs.filter((s) => s.kind === "search");
    const forName = searches.filter((s) => s.follows === "a printed name");
    const unnamed = xs.filter((s) => s.kind === "read unnamed");
    const fromHit = unnamed.filter((s) => s.follows === "a search hit");
    const carried = [...forName, ...fromHit].reduce((s, x) => s + x.carried, 0);
    const inRuns = rs.filter((r) => r.steps.some((x) => x.follows === "a printed name")).length;
    return `searches for a name giraffe had printed ${forName.length} of ${searches.length} (in ${inRuns} of ${rs.length} runs); unnamed reads of a file a search had hit ${fromHit.length} of ${unnamed.length}; together ${forName.length + fromHit.length} calls, ${(
      (forName.length + fromHit.length) / rs.length
    ).toFixed(1)} a run, ${carried} tokens carried (${((100 * carried) / (xs.reduce((s, x) => s + x.carried, 0) || 1)).toFixed(1)}%)`;
  };
  console.log(`\nleaving giraffe for grep: ${hunt(runs)}`);
  if (third > 0) {
    console.log(`  cheapest third: ${hunt(sorted.slice(0, third))}`);
    console.log(`  dearest third:  ${hunt(sorted.slice(runs.length - third))}`);
  }
}

// At each call position, how far the runs still agree on what to ask.
function printPositions(runs: readonly Run[]) {
  console.log(`\nat each call position: the calls the runs made, most common first (runs still at that position)`);
  for (let at = 0; at < 12; at++) {
    const here = runs.filter((r) => r.steps.length > at);
    if (here.length === 0) break;
    const tally = new Map<string, number>();
    for (const r of here) {
      const sig = r.steps[at]!.signature;
      tally.set(sig, (tally.get(sig) ?? 0) + 1);
    }
    const top = [...tally].sort((a, b) => b[1] - a[1]).slice(0, 3);
    console.log(`  call ${String(at + 1).padStart(2)} (${here.length} runs, ${tally.size} distinct): ${top.map(([sig, n]) => `${n}× ${sig.slice(0, 60)}`).join(" | ")}`);
  }
}

// §2: the giraffe calls whose answer nothing followed from — none of
// their results opened, cited or re-queried — and the ones that found
// nothing at all; what they cost, and what they have in common.
function printUnused(runs: readonly Run[]) {
  const all = runs.flatMap((r) => r.steps);
  const giraffe = all.filter((s) => s.kind.startsWith("giraffe"));
  const carriedAll = all.reduce((s, x) => s + x.carried, 0) || 1;
  const tokensAll = runs.reduce((s, r) => s + r.tokens, 0) || 1;
  console.log(`\n# giraffe calls nothing followed from (${giraffe.length} giraffe calls in ${runs.length} runs)`);
  for (const kind of ["giraffe none used", "giraffe nothing found"] as const) {
    const xs = giraffe.filter((s) => s.kind === kind);
    const carried = xs.reduce((s, x) => s + x.carried, 0);
    console.log(`\n${kind}: ${xs.length} calls (${((100 * xs.length) / giraffe.length).toFixed(1)}% of giraffe calls), ${xs.reduce((s, x) => s + x.bytes, 0)} bytes, ${xs.reduce((s, x) => s + x.added, 0)} tokens added, ${carried} carried (${((100 * carried) / carriedAll).toFixed(1)}% of all carried, ${((100 * carried) / tokensAll).toFixed(1)}% of the runs' tokens)`);
    const shape = (s: Step) => {
      const invocations = s.signature.split(" ; ").length;
      const bare = /^giraffe find (?:count )?["']?[\w.-]+["']?$/.test(s.signature);
      const query = s.signature.includes("--query");
      return invocations > 1 ? "chained invocations" : bare ? "one bare word" : query && /find --query/.test(s.signature) ? "query only, no word" : query ? "word narrowed by a query" : "other";
    };
    const tally = (label: string, key: (s: Step) => string) => {
      const t = new Map<string, number>();
      for (const s of xs) t.set(key(s), (t.get(key(s)) ?? 0) + 1);
      console.log(`  ${label}: ${[...t].sort((a, b) => b[1] - a[1]).map(([k, n]) => `${k} ${n}`).join(", ")}`);
    };
    if (xs.length === 0) continue;
    tally("shape", shape);
    tally("results printed", (s) => (s.resultCount === 0 ? "0" : s.resultCount <= 3 ? "1-3" : s.resultCount <= 7 ? "4-7" : "8+") + (s.truncated ? " truncated" : ""));
    tally("position among the run's giraffe calls", (s) => {
      const run = runs.find((r) => r.steps.includes(s))!;
      const at = run.steps.filter((x) => x.kind.startsWith("giraffe") && x.kind !== "giraffe help" && x.index <= s.index).length;
      const of = run.steps.filter((x) => x.kind.startsWith("giraffe") && x.kind !== "giraffe help").length;
      return at === 1 ? "first" : at === 2 ? "second" : at === of ? "last" : "between";
    });
    tally("the call after it", (s) => {
      const run = runs.find((r) => r.steps.includes(s))!;
      const next = run.steps[s.index];
      return next === undefined ? "none, the run's last call" : next.kind;
    });
    tally("before or after the first landing point", (s) => {
      const run = runs.find((r) => r.steps.includes(s))!;
      return run.firstLanding === null ? "no landing" : s.index < run.firstLanding ? "before" : s.index === run.firstLanding ? "at" : "after";
    });
    const words = new Map<string, number>();
    for (const s of xs) {
      for (const m of s.signature.matchAll(/find (?:count )?["']?([\w.-]+)["']?(?=\s|$)/g)) {
        if (m[1] !== "--query") words.set(m[1] as string, (words.get(m[1] as string) ?? 0) + 1);
      }
      for (const m of s.signature.matchAll(/--query '([^']+)'/g)) words.set(`[${m[1]}]`, (words.get(`[${m[1]}]`) ?? 0) + 1);
    }
    console.log(`  words and queries, most common: ${[...words].sort((a, b) => b[1] - a[1]).slice(0, 25).map(([w, n]) => `${w} ${n}`).join(", ")}`);
  }
}

// §3: the reads — whole-file against ranged, of files giraffe had named
// against not, and for the whole-file ones whether a range for that file
// was already on the page.
function printReads(runs: readonly Run[]) {
  const all = runs.flatMap((r) => r.steps);
  const reads = all.filter((s) => s.kind.startsWith("read"));
  const carriedAll = all.reduce((s, x) => s + x.carried, 0) || 1;
  console.log(`\n# reads (${reads.length} in ${runs.length} runs, ${(reads.length / runs.length).toFixed(1)} a run)`);
  console.log(`  shape    named?    calls   per run     bytes    added  carried   share   range on page`);
  for (const shape of ["whole", "ranged", "grep"] as const) {
    for (const kind of ["read named", "read unnamed"] as const) {
      const xs = reads.filter((s) => s.shape === shape && s.kind === kind);
      const carried = xs.reduce((s, x) => s + x.carried, 0);
      const onPage = shape === "whole" ? `${xs.filter((s) => s.rangeOnPage).length} of ${xs.length}` : "";
      console.log(`  ${shape.padEnd(8)} ${(kind === "read named" ? "named" : "unnamed").padEnd(8)} ${String(xs.length).padStart(6)}  ${(xs.length / runs.length).toFixed(2).padStart(7)}  ${String(xs.reduce((s, x) => s + x.bytes, 0)).padStart(8)}  ${String(xs.reduce((s, x) => s + x.added, 0)).padStart(7)}  ${String(carried).padStart(7)}  ${((100 * carried) / carriedAll).toFixed(1).padStart(5)}%   ${onPage}`);
    }
  }
  const whole = reads.filter((s) => s.shape === "whole");
  const files = new Map<string, number>();
  for (const s of whole) for (const f of s.files) files.set(f, (files.get(f) ?? 0) + 1);
  console.log(`  files read whole, most often: ${[...files].sort((a, b) => b[1] - a[1]).slice(0, 8).map(([f, n]) => `${f} ${n}`).join(", ")}`);
  const perRun = runs.map((r) => r.steps.filter((s) => s.shape === "whole").length);
  console.log(`  whole-file reads per run: median ${median(perRun)}, range ${Math.min(...perRun)}-${Math.max(...perRun)}; runs with none ${perRun.filter((n) => n === 0).length}`);
}

function median(xs: readonly number[]): number {
  const s = [...xs].sort((a, b) => a - b);
  const mid = Math.floor(s.length / 2);
  const at = (i: number) => s[i] ?? 0;
  return s.length % 2 === 1 ? at(mid) : Math.round((at(mid - 1) + at(mid)) / 2);
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
  printSectionBytes(calls);
  printSourceTotals(calls);
  printResultTotals(calls);
}

function printMany(paths: readonly string[], landing: LandingPoints | null) {
  console.log(`run                              calls  printed  results  used  result bytes  used bytes`);
  const every: Call[] = [];
  const runs: Run[] = [];
  for (const path of paths) {
    const transcript = readTranscript(path);
    const { calls } = giraffeCalls(transcript);
    const file = basename(path);
    runs.push(runOf(file, transcript, calls, landing));
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
  printSteps(runs);
  printUnused(runs);
  printReads(runs);
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

  printSectionBytes(calls);
  printSourceTotals(calls);
  printResultTotals(calls);

  const queried = new Map<string, number>();
  for (const call of calls) {
    for (const m of call.command.matchAll(/([a-z]+):/g)) queried.set(m[1] as string, (queried.get(m[1] as string) ?? 0) + 1);
  }
  console.log(`\nattributes typed into a query: ${[...queried].sort((a, b) => b[1] - a[1]).map(([k, n]) => `${k} (${n})`).join(", ")}`);
}

const argv = process.argv.slice(2);
const landingAt = argv.indexOf("--landing");
const landing = landingAt >= 0 ? readLandingPoints(argv[landingAt + 1] as string) : null;
const args = landingAt >= 0 ? argv.filter((_, i) => i !== landingAt && i !== landingAt + 1) : argv;
if (args.length === 0) {
  console.error("usage: tsx src/account-output.ts [--landing <points.json>] <transcript.jsonl>...");
  process.exitCode = 1;
} else if (args.length === 1) {
  printOne(args[0] as string);
} else {
  printMany(args, landing);
}
