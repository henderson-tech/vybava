// The pass summary: report.json (machine) and report.md (human) beside the
// shots, built from every record in the pass directory — never from the
// current run's selection, so a filtered re-shoot still summarizes the pass.
// The per-shot records stay the source of truth: `vybava ui-loop split`,
// `publish` and `scoreboard` read those, not this file.

import * as fs from 'node:fs';
import * as path from 'node:path';

import type { ShotRecord, ShotStatus } from './capture';
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
  /** Defect hits per lint rule. */
  lint: Record<string, number>;
  /** Informational hits per lint rule (truncation, contained scrollers, repeats). */
  lintInfo: Record<string, number>;
  defects: number;
  consoleErrors: number;
}

export interface PassReport {
  v: number;
  pass: number;
  generatedAt: string;
  totals: Totals;
  areas: Array<{ area: string } & Totals>;
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
  return { shots: 0, byStatus: {}, lint: {}, lintInfo: {}, defects: 0, consoleErrors: 0 };
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
  const areas = [...byArea.entries()].sort(([a], [b]) => order(a) - order(b) || a.localeCompare(b)).map(([area, t]) => ({ area, ...t }));
  return { v: 1, pass: run.pass, generatedAt: new Date().toISOString(), totals, areas, rows };
}

const cell = (v: string | number): string => String(v).replace(/\|/g, '\\|').replace(/\n/g, ' ');

export function toMarkdown(report: PassReport): string {
  const statuses = ['ok', 'recipe-failed', 'theme-mismatch', 'build-error', 'unreachable', 'error'];
  const out: string[] = [
    `# UI loop · pass ${report.pass}`,
    '',
    `${report.totals.shots} shots · ${report.totals.defects} lint defects · ${report.totals.consoleErrors} console errors · generated ${report.generatedAt}`,
    '',
    `| Area | Shots | ${statuses.join(' | ')} | Lint defects | Console errors |`,
    `|---|--:|${statuses.map(() => '--:').join('|')}|--:|--:|`,
  ];
  for (const a of [...report.areas, { area: '**all**', ...report.totals }]) {
    out.push(`| ${cell(a.area)} | ${a.shots} | ${statuses.map((s) => a.byStatus[s] ?? 0).join(' | ')} | ${a.defects} | ${a.consoleErrors} |`);
  }
  const rules = Object.entries(report.totals.lint).sort(([, a], [, b]) => b - a);
  out.push('', '## Lint defects by rule', '');
  out.push(...(rules.length ? rules.map(([rule, n]) => `- \`${rule}\`: ${n}`) : ['None.']));
  const info = Object.entries(report.totals.lintInfo).sort(([, a], [, b]) => b - a);
  if (info.length) out.push('', 'For judgement: ' + info.map(([rule, n]) => `\`${rule}\` ${n}`).join(' · '));
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
