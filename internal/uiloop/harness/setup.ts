// Playwright globalSetup: once per run, before any shot — the repo's data
// preparation, the `{PARAM}` values, and one signed-in storage state per
// (app, identity) the selected screens use. A `--resume` run reuses the pass's
// params and never prepares again, but always signs in fresh: sessions expire,
// and a resume hours later reusing them shot the sign-in page instead.

import * as fs from 'node:fs';
import * as path from 'node:path';

import { chromium } from '@playwright/test';

import project from '../project';
import type { RunContext, Project } from './project';
import {
  AUTH_FILE,
  type AuthMap,
  PARAMS_FILE,
  authKey,
  baseUrlOf,
  identities,
  loadRun,
  passPath,
  plannedShots,
  readJson,
  repoRoot,
  safeId,
  writeJson,
} from './run';

export function runContext(): RunContext {
  const run = loadRun();
  return {
    dir: run.dir,
    out: passPath(run),
    baseUrls: Object.fromEntries(Object.keys(run.apps).map((app) => [app, baseUrlOf(run, app)])),
  };
}

export default async function setup(): Promise<void> {
  const run = loadRun();
  const p: Project = project;
  if (p.freezeClock && !run.clock) {
    throw new Error(`project.freezeClock is set, but ${run.passDir}/run.json records no clock: write it with \`vybava ui-loop run\` (0.36 or later)`);
  }
  const ctx = runContext();
  fs.mkdirSync(ctx.out, { recursive: true });
  const shots = plannedShots(p, run);
  const resume = run.selection.resume;

  if (!resume && p.prepare) await p.prepare(ctx);

  const paramsFile = passPath(run, PARAMS_FILE);
  let params = resume ? readJson<Record<string, string>>(paramsFile) : null;
  if (!params) {
    params = p.params ? await p.params(ctx) : {};
    writeJson(paramsFile, params);
  }

  const authFile = passPath(run, AUTH_FILE);
  const auth: AuthMap = {};
  const needed = identities(p, shots);
  if (p.login && needed.size) {
    const browser = await chromium.launch();
    try {
      for (const [app, set] of needed) {
        for (const as of set) {
          const key = authKey(app, as);
          const context = await browser.newContext({ baseURL: baseUrlOf(run, app) });
          try {
            const page = await context.newPage();
            const given = await p.login(page, as, app, ctx);
            let file: string;
            if (typeof given === 'string') {
              file = path.resolve(repoRoot(), given);
            } else {
              file = passPath(run, '.auth', `${safeId(app)}.${safeId(as)}.json`);
              fs.mkdirSync(path.dirname(file), { recursive: true });
              await context.storageState({ path: file });
            }
            auth[key] = file;
          } finally {
            await context.close();
          }
        }
      }
    } finally {
      await browser.close();
    }
  }
  writeJson(authFile, auth);
  console.log(
    `ui-loop: pass ${run.pass} · ${shots.length} shots · params ${Object.keys(params).length} · sessions ${Object.keys(auth).length}`,
  );
}
