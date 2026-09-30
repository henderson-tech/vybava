// The pass summary: report.json (machine) and report.md (human) beside the
// shots, built from every record in the pass directory — never from the
// current run's selection, so a filtered re-shoot still summarizes the pass.
// The per-shot records stay the source of truth: `vybava ui-loop split`,
// `publish` and `scoreboard` read those, not this file.

import * as fs from 'node:fs';
import * as path from 'node:path';

import type { ShotRecord, ShotStatus } from './capture';
import type { LintKey } from './lint';
import { type RunFile, passPath, writeJson } from './run';

export interface ReportRow {
  id: string;
  app: string;
  area: string;
  kind: string;
  state: string | null;
  title: string;
  viewport: string;
  theme: string;
  status: ShotStatus;
  failure: string | null;
  /** Paths relative to the pass directory. */
  files: string[];
  defects: number;
  lint: Record<string, number>;
  consoleErrors: number;
}

export interface Totals {
  shots: number;
  byStatus: Record<string, number>;
  /** Defect hits per lint rule, summed over shots (one sidebar defect counts once per screen). */
  lint: Record<string, number>;
  /** Distinct defects per lint rule across the shots: rule + element path + detail, counted once. */
  lintUnique: Record<string, number>;
  /** Informational hits per lint rule (truncation, contained scrollers, repeats). */
  lintInfo: Record<string, number>;
  defects: number;
  /** Sum of lintUnique. */
  defectsUnique: number;
  consoleErrors: number;
}

/** One defect repeated across screens: fix it once, in the primitive or chrome it lives in. */
export interface Offender {
  rule: string;
  path: string;
  detail: string;
  /** Distinct screen ids it was seen on. */
  screens: number;
  /** Shots (screen × viewport × theme) it was seen on. */
  shots: number;
}

export interface PassReport {
  v: number;
  pass: number;
  generatedAt: string;
  totals: Totals;
  areas: Array<{ area: string } & Totals>;
  /** The most repeated defects (on 2+ screens), most screens first. */
  offenders: Offender[];
  rows: ReportRow[];
}

export function collectRecords(passDir: string): ShotRecord[] {
  const shots = path.join(passDir, 'shots');
  if (!fs.existsSync(shots)) return [];
  const records: ShotRecord[] = [];
  for (const dir of fs.readdirSync(shots).sort()) {
    const full = path.join(shots, dir);
    if (!fs.statSync(full).isDirectory()) continue;
    for (const file of fs.readdirSync(full).sort()) {
      if (!file.endsWith('.json')) continue;
      try {
        records.push(JSON.parse(fs.readFileSync(path.join(full, file), 'utf-8')) as ShotRecord);
      } catch (error) {
        // A record a killed run cut off mid-write: the next --resume retakes it.
        if (!(error instanceof SyntaxError)) throw error;
      }
    }
  }
  return records.sort((a, b) => a.order - b.order || a.id.localeCompare(b.id) || a.viewport.localeCompare(b.viewport) || a.theme.localeCompare(b.theme));
}

function emptyTotals(): Totals {
  return { shots: 0, byStatus: {}, lint: {}, lintUnique: {}, lintInfo: {}, defects: 0, defectsUnique: 0, consoleErrors: 0 };
}

function add(t: Totals, r: ShotRecord): void {
  t.shots++;
  t.byStatus[r.status] = (t.byStatus[r.status] ?? 0) + 1;
  for (const [rule, n] of Object.entries(r.lint?.defects ?? {})) {
    t.lint[rule] = (t.lint[rule] ?? 0) + n;
    t.defects += n;
  }
  for (const [rule, n] of Object.entries(r.lint?.info ?? {})) t.lintInfo[rule] = (t.lintInfo[rule] ?? 0) + n;
  t.consoleErrors += r.consoleErrors.length;
}

const keyOf = (rule: string, k: LintKey): string => `${rule}\n${k.path}\n${k.detail}`;

/** Distinct rule + path + detail across the records, into t.lintUnique / t.defectsUnique. */
function addUnique(t: Totals, records: readonly ShotRecord[]): void {
  const seen = new Set<string>();
  for (const r of records) {
    for (const [rule, keys] of Object.entries(r.lint?.distinct ?? {})) {
      for (const k of keys) {
        const key = keyOf(rule, k);
        if (seen.has(key)) continue;
        seen.add(key);
        t.lintUnique[rule] = (t.lintUnique[rule] ?? 0) + 1;
        t.defectsUnique++;
      }
    }
  }
}

/** The defects seen on the most screens (2 or more), at most `limit`. */
export function repeatedOffenders(records: readonly ShotRecord[], limit = 20): Offender[] {
  const by = new Map<string, { o: Offender; ids: Set<string> }>();
  for (const r of records) {
    for (const [rule, keys] of Object.entries(r.lint?.distinct ?? {})) {
      for (const k of keys) {
        const key = keyOf(rule, k);
        const hit = by.get(key) ?? { o: { rule, path: k.path, detail: k.detail, screens: 0, shots: 0 }, ids: new Set<string>() };
        hit.o.shots++;
        hit.ids.add(r.id);
        hit.o.screens = hit.ids.size;
        by.set(key, hit);
      }
    }
  }
  return [...by.values()]
    .map((h) => h.o)
    .filter((o) => o.screens > 1)
    .sort((a, b) => b.screens - a.screens || b.shots - a.shots || a.rule.localeCompare(b.rule) || a.path.localeCompare(b.path) || a.detail.localeCompare(b.detail))
    .slice(0, limit);
}

export function buildReport(run: RunFile, records: readonly ShotRecord[]): PassReport {
  const totals = emptyTotals();
  const byArea = new Map<string, Totals>();
  const rows: ReportRow[] = [];
  for (const r of records) {
    add(totals, r);
    const area = byArea.get(r.area) ?? emptyTotals();
    add(area, r);
    byArea.set(r.area, area);
    const dir = path.posix.join('shots', r.id);
    rows.push({
      id: r.id,
      app: r.app,
      area: r.area,
      kind: r.kind,
      state: r.state,
      title: r.title,
      viewport: r.viewport,
      theme: r.theme,
      status: r.status,
      failure: r.failure ? `${r.failure.step ? `${r.failure.step}: ` : ''}${r.failure.error}` : null,
      files: [r.files.viewport, r.files.full].filter((f): f is string => !!f).map((f) => path.posix.join(dir, f)),
      defects: Object.values(r.lint?.defects ?? {}).reduce((a, b) => a + b, 0),
      lint: r.lint?.defects ?? {},
      consoleErrors: r.consoleErrors.length,
    });
  }
  const order = (area: string): number => {
    const i = run.areas.indexOf(area);
    return i < 0 ? run.areas.length : i;
  };
  addUnique(totals, records);
  for (const [area, t] of byArea) addUnique(t, records.filter((r) => r.area === area));
  const areas = [...byArea.entries()].sort(([a], [b]) => order(a) - order(b) || a.localeCompare(b)).map(([area, t]) => ({ area, ...t }));
  return { v: 1, pass: run.pass, generatedAt: new Date().toISOString(), totals, areas, offenders: repeatedOffenders(records), rows };
}

const cell = (v: string | number): string => String(v).replace(/\|/g, '\\|').replace(/\n/g, ' ');

export function toMarkdown(report: PassReport): string {
  const statuses = ['ok', 'recipe-failed', 'theme-mismatch', 'build-error', 'unreachable', 'error'];
  const out: string[] = [
    `# UI loop · pass ${report.pass}`,
    '',
    `${report.totals.shots} shots · ${report.totals.defects} lint defects (${report.totals.defectsUnique} unique) · ${report.totals.consoleErrors} console errors · generated ${report.generatedAt}`,
    '',
    `| Area | Shots | ${statuses.join(' | ')} | Lint defects | Unique | Console errors |`,
    `|---|--:|${statuses.map(() => '--:').join('|')}|--:|--:|--:|`,
  ];
  for (const a of [...report.areas, { area: '**all**', ...report.totals }]) {
    out.push(`| ${cell(a.area)} | ${a.shots} | ${statuses.map((s) => a.byStatus[s] ?? 0).join(' | ')} | ${a.defects} | ${a.defectsUnique} | ${a.consoleErrors} |`);
  }
  const rules = Object.entries(report.totals.lint).sort(([, a], [, b]) => b - a);
  out.push('', '## Lint defects by rule', '');
  out.push(...(rules.length ? rules.map(([rule, n]) => `- \`${rule}\`: ${n} (${report.totals.lintUnique[rule] ?? 0} unique)`) : ['None.']));
  const info = Object.entries(report.totals.lintInfo).sort(([, a], [, b]) => b - a);
  if (info.length) out.push('', 'For judgement (informational rules and allowlisted hits): ' + info.map(([rule, n]) => `\`${rule}\` ${n}`).join(' · '));
  if (report.offenders.length) {
    out.push('', '## Repeated offenders', '', 'The same defect on several screens: fix it once, where it lives.', '');
    out.push(...report.offenders.map((o) => `- \`${o.rule}\` ${cell(o.detail)} · \`${cell(o.path)}\` · ${o.screens} screens, ${o.shots} shots`));
  }
  const broken = report.rows.filter((r) => r.status !== 'ok');
  out.push('', '## Shots that are not ok', '');
  out.push(
    ...(broken.length
      ? broken.map((r) => `- \`${r.id}\` @${r.viewport} ${r.theme} — **${r.status}**${r.failure ? `: ${cell(r.failure)}` : ''}`)
      : ['None.']),
  );
  const worst = report.rows.filter((r) => r.defects > 0).sort((a, b) => b.defects - a.defects).slice(0, 25);
  if (worst.length) {
    out.push('', '## Most lint defects', '');
    out.push(...worst.map((r) => `- \`${r.id}\` @${r.viewport} ${r.theme}: ${r.defects} (${Object.entries(r.lint).map(([k, n]) => `${k} ${n}`).join(', ')})`));
  }
  return `${out.join('\n')}\n`;
}

export function writeReport(run: RunFile): PassReport {
  const report = buildReport(run, collectRecords(passPath(run)));
  writeJson(passPath(run, 'report.json'), report);
  fs.writeFileSync(passPath(run, 'report.md'), toMarkdown(report), 'utf-8');
  return report;
}
