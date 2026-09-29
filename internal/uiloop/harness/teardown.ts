// Playwright globalTeardown: the pass summary, written after every run (a
// filtered or resumed run re-summarizes the whole pass directory).

import { writeReport } from './report';
import { loadRun } from './run';

export default async function teardown(): Promise<void> {
  const report = writeReport(loadRun());
  const failed = report.rows.length - (report.totals.byStatus['ok'] ?? 0);
  console.log(`ui-loop: report.json · ${report.rows.length} shots · ${failed} not ok · ${report.totals.defects} lint defects`);
}
