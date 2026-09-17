// A task's landing points, and whether a piece of text arrives at them.
//
// Directive 045 put these in analyze-arrival.ts (code-manners D6: 040,
// 041 and 044 had each scored arrival with a one-off) and 046 made a
// point a place rather than a regex. Directive 049 §1 asks a second
// question of the same places — at which tool call a run first put one
// on the page — from account-output.ts, so the places and the forms an
// answer may use to name them live here, once, and both scripts import
// them (D5).

import { readFileSync } from "node:fs";

export interface Arrival {
  readonly hit: number;
  readonly of: number;
  readonly missed: readonly string[];
}

// A landing point is a place in the corpus: the file at path, or, with
// lines, the range [from, to] of it (1-based, inclusive — a
// declaration's whole extent as giraffe prints it). symbols are the
// declarations at the place, as the corpus's own index lists them:
// a bare name for a type, a func, a var or a const; Type.name for a
// struct field; Type.name() for a method. For a file the symbols are
// its top-level declarations — the names an answer uses to say "this
// file"; a member belongs to its type, and a method name alone (Start,
// Port) says nothing about which file. For a range, everything
// declared in it, members included: the member is the place.
export interface LandingPoint {
  readonly path: string;
  readonly lines?: readonly [number, number];
  readonly symbols?: readonly string[];
}

export type LandingPoints = Record<string, LandingPoint>;

// The points of one task, as harness/landing/<task>.json holds them.
export function readLandingPoints(file: string): LandingPoints {
  return JSON.parse(readFileSync(file, "utf8")) as LandingPoints;
}

function escapeRegExp(s: string): string {
  return s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

// namesPath: the path itself, any suffix of it from a directory
// boundary, or its basename — as a whole word, so metrics.go does not
// match metrics_test.go or a longer name ending the same way.
export function namesPath(text: string, path: string): boolean {
  const segments = path.split("/");
  return segments.some((_, i) => new RegExp(`(?<![\\w/])${escapeRegExp(segments.slice(i).join("/"))}\\b`).test(text));
}

// namesLine: basename:N or basename:N-M (a hyphen or an en dash) with
// the reference overlapping the range.
export function namesLine(text: string, path: string, [from, to]: readonly [number, number]): boolean {
  const base = escapeRegExp(path.split("/").pop() ?? path);
  for (const m of text.matchAll(new RegExp(`(?<!\\w)${base}:(\\d+)(?:[-–](\\d+))?`, "g"))) {
    const a = Number(m[1]);
    const b = m[2] === undefined ? a : Number(m[2]);
    if (a <= to && b >= from) return true;
  }
  return false;
}

// namesSymbol: a bare name as a whole word; a field as Type.name or
// v.name through a receiver variable; a method as Type.name, v.name( or
// the call name( — the parenthesis is what tells a call of the method
// from a type of the same name (core.MetricsSnapshot is the type,
// MetricsSnapshot() the method). Two of 045's runs wrote exactly
// "MetricsSnapshot() at line 4244".
function namesSymbol(text: string, symbol: string): boolean {
  const dot = symbol.indexOf(".");
  if (dot < 0) return new RegExp(`\\b${escapeRegExp(symbol)}\\b`).test(text);
  const type = escapeRegExp(symbol.slice(0, dot));
  const isMethod = symbol.endsWith("()");
  const member = escapeRegExp(isMethod ? symbol.slice(dot + 1, -2) : symbol.slice(dot + 1));
  const viaReceiver = isMethod ? `\\b(?:[a-z_]\\w*\\.)?${member}\\(` : `\\b[a-z_]\\w*\\.${member}\\b`;
  return new RegExp(`\\b${type}\\.${member}\\b|${viaReceiver}`).test(text);
}

function arrivesAt(text: string, p: LandingPoint): boolean {
  const there = p.lines ? namesLine(text, p.path, p.lines) : namesPath(text, p.path);
  return there || (p.symbols ?? []).some((s) => namesSymbol(text, s));
}

// The points an answer arrives at, and the ones it misses, by name.
export function scoreArrival(finalText: string, points: LandingPoints): Arrival {
  const missed = Object.entries(points)
    .filter(([, point]) => !arrivesAt(finalText, point))
    .map(([name]) => name);
  const of = Object.keys(points).length;
  return { hit: of - missed.length, of, missed };
}
