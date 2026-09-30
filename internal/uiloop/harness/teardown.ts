// Playwright globalTeardown: the pass summary, written after every run (a
// filtered or resumed run re-summarizes the whole pass directory), then the
// done marker.

import { writeReport } from './report';
import { loadRun, passPath, writeJson } from './run';

/**
 * `<pass>/done.json`: this run is over (internal/uiloop/follow.go DoneFile).
 * `vybava ui-loop publish --follow` stops on it, but only while `run` equals
 * run.json's `createdAt`, so the marker of an earlier run never ends a resume.
 * Written last, after report.json, so every shot record precedes it.
 */
export interface DoneFile {
  v: 1;
  pass: number;
  run: string;
  finishedAt: string;
  shots: number;
}

export default async function teardown(): Promise<void> {
  const run = loadRun();
  const report = writeReport(run);
  const failed = report.rows.length - (report.totals.byStatus['ok'] ?? 0);
  console.log(`ui-loop: report.json · ${report.rows.length} shots · ${failed} not ok · ${report.totals.defects} lint defects`);
  const done: DoneFile = { v: 1, pass: run.pass, run: run.createdAt, finishedAt: new Date().toISOString(), shots: report.rows.length };
  writeJson(passPath(run, 'done.json'), done);
}
