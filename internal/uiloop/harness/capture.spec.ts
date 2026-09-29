// The capture: one test per selected screen × viewport × theme, fully
// parallel. Each test opens its screen in a fresh DPR-2 context, drives it
// with the manifest recipe, shoots it, lints it and writes its record to
// `<passDir>/shots/<id>/<viewport>.<theme>.{png,full.png,json}`. A failing
// recipe is a RESULT (`recipe-failed` plus a shot of what rendered), never a
// failed test; only a harness error fails one.

import * as fs from 'node:fs';
import * as path from 'node:path';

import { type Browser, test } from '@playwright/test';

import project from '../project';
import {
  FINAL_STATUSES,
  NAV_TIMEOUT,
  NetworkTracker,
  OVERLAY_KINDS,
  type RecipeFailure,
  RECORD_VERSION,
  type ShotRecord,
  UnresolvedParam,
  addGlassBlur,
  collectConsoleErrors,
  contextOptions,
  defaultReadTheme,
  emulateSafeArea,
  freezeMotion,
  lintSummary,
  missingParams,
  probeGlassBlur,
  readBuildHealth,
  readRecord,
  resolveUrl,
  runOpen,
  seedLocalStorage,
  settle,
  shootFull,
  shootViewport,
  waitForGreenBuild,
  writeRecord,
} from './capture';
import { type LintOptions, lintPage } from './lint';
import type { RecipeContext } from './manifest';
import type { Project } from './project';
import {
  AUTH_FILE,
  type AuthMap,
  PARAMS_FILE,
  type PlannedShot,
  type RunFile,
  type ShotFiles,
  authKey,
  baseUrlOf,
  fill,
  loadRun,
  passPath,
  plannedShots,
  readJson,
  shotFiles,
} from './run';
import { insetsOf } from './viewports';

const run = loadRun();
const p: Project = project;

test.describe.configure({ mode: 'parallel' });

for (const shot of plannedShots(p, run)) {
  const files = shotFiles(run, shot.screen.id, shot.viewport, shot.theme);
  if (run.selection.resume) {
    const previous = readRecord(files.json);
    if (previous && FINAL_STATUSES.includes(previous.status)) continue;
  }
  test(`${shot.screen.id} @${shot.viewport} ${shot.theme}`, async ({ browser }) => {
    await capture(browser, shot, run, files);
  });
}

const firstLine = (error: unknown): string =>
  (error instanceof Error ? (error.message.split('\n')[0] ?? error.message) : String(error)).slice(0, 500);

async function capture(browser: Browser, shot: PlannedShot, run: RunFile, files: ShotFiles): Promise<void> {
  const { screen, app, theme } = shot;
  const viewport = run.viewports[shot.viewport]!;
  const started = Date.now();
  const timings: Record<string, number> = {};
  let since = started;
  const lap = (name: string): void => {
    timings[name] = Date.now() - since;
    since = Date.now();
  };
  const as = screen.as ?? p.defaultAs;
  const record: ShotRecord = {
    v: RECORD_VERSION,
    pass: run.pass,
    order: shot.order,
    id: screen.id,
    app,
    area: screen.area,
    kind: screen.kind,
    state: screen.state ?? null,
    title: screen.title,
    route: screen.route,
    url: null,
    parentId: screen.parentId ?? null,
    variantOf: screen.variantOf ?? null,
    as: as ?? null,
    viewport: shot.viewport,
    size: { width: viewport.width, height: viewport.height },
    theme,
    themeActual: null,
    status: 'ok',
    failure: null,
    files: { viewport: null, full: null },
    full: null,
    safeArea: { path: 'none', error: null, emulated: null },
    lint: null,
    consoleErrors: [],
    settleNotes: [],
    timings,
    sourceFiles: [...screen.sourceFiles],
    knownIssues: [...(screen.knownIssues ?? [])],
    destructive: screen.destructive === true,
    capturedAt: new Date(started).toISOString(),
  };
  const finish = (): void => {
    timings['total'] = Date.now() - started;
    writeRecord(record, files.json);
  };
  const fail = (status: ShotRecord['status'], error: string, failure: Partial<RecipeFailure> = {}): void => {
    record.status = status;
    record.failure = { step: failure.step ?? null, stepIndex: failure.stepIndex ?? null, error };
  };

  fs.mkdirSync(files.dir, { recursive: true });
  for (const stale of [files.png, files.full]) fs.rmSync(stale, { force: true });

  if (screen.unreachable) {
    fail('unreachable', screen.unreachable);
    finish();
    return;
  }
  const params = readJson<Record<string, string>>(passPath(run, PARAMS_FILE)) ?? {};
  const missing = missingParams(screen, params);
  if (missing.length) {
    fail('unreachable', `unresolved param ${missing.map((m) => `{${m}}`).join(', ')}`);
    finish();
    return;
  }
  const auth = readJson<AuthMap>(passPath(run, AUTH_FILE)) ?? {};
  const storage = as !== undefined ? auth[authKey(app, as)] : undefined;
  if (as !== undefined && p.login && !storage) {
    fail('error', `no signed-in session for ${app}/${as} — the run's setup did not produce one`);
    finish();
    throw new Error(record.failure!.error);
  }

  const baseUrl = baseUrlOf(run, app);
  const context = await browser.newContext(contextOptions(viewport, screen, theme, baseUrl, storage));
  try {
    if (p.theme) await p.theme(context, theme, app);
    const page = await context.newPage();
    await seedLocalStorage(page, screen);
    record.consoleErrors = collectConsoleErrors(page);
    const network = new NetworkTracker(page);
    const safeArea = await emulateSafeArea(page, viewport);
    record.safeArea = { ...safeArea, emulated: null };
    const ctx: RecipeContext = { params, baseUrl, app, viewport: shot.viewport, theme };
    if (screen.before) await screen.before(page, ctx);
    lap('setup');

    const settleInto = async (): Promise<void> => {
      try {
        record.settleNotes.push(...(await settle(page, network)));
        if (p.settle) await p.settle(page, screen);
      } catch (error) {
        // A page that never finishes loading is still shot — the note says why it looks so.
        record.settleNotes.push(`settle failed: ${firstLine(error)}`);
      }
    };
    const drive = async (): Promise<RecipeFailure | null> => {
      try {
        await page.goto(resolveUrl(fill(screen.route, params).value, baseUrl), { waitUntil: 'load', timeout: NAV_TIMEOUT });
      } catch (error) {
        return { step: `goto ${screen.route}`, stepIndex: null, error: firstLine(error) };
      }
      await settleInto();
      const failure = await runOpen(page, screen.open, ctx);
      if (screen.open) await settleInto();
      if (failure || !screen.ready) return failure;
      try {
        if (typeof screen.ready === 'string') {
          await page.locator(fill(screen.ready, params).value).filter({ visible: true }).first().waitFor({ timeout: 15_000 });
        } else {
          await screen.ready(page, ctx);
        }
      } catch (error) {
        return { step: 'ready', stepIndex: null, error: firstLine(error) };
      }
      await settleInto();
      return null;
    };

    let failure = await drive();
    // Many lanes edit one tree: a red dev server paints its overlay. Wait for green and
    // redo the recipe; never record an overlay as `ok`.
    let health = await readBuildHealth(page);
    if (health.red) {
      record.settleNotes.push(`build red, waiting up to ${run.buildWait} s: ${health.error}`);
      test.info().setTimeout(test.info().timeout + (run.buildWait + 120) * 1000);
      health = await waitForGreenBuild(page, run.buildWait);
      if (!health.red) {
        failure = await drive();
        health = await readBuildHealth(page);
      }
    }
    lap('recipe');
    if (health.red) fail('build-error', health.error ?? 'build red');
    else if (failure) fail('recipe-failed', failure.error, failure);
    record.url = page.url();
    record.themeActual = p.readTheme ? await p.readTheme(page) : await defaultReadTheme(page);
    await freezeMotion(page);
    lap('settle');

    await shootViewport(page, files.png);
    record.files.viewport = path.basename(files.png);
    lap('shot');
    const during = await readBuildHealth(page);
    if (during.red && record.status === 'ok') fail('build-error', `went red during the shot: ${during.error ?? 'build red'}`);

    if (record.status === 'ok') {
      const options: LintOptions = {
        insets: insetsOf(viewport),
        mobile: viewport.mobile === true,
        grid: run.lint.grid,
        touchTarget: run.lint.touchTarget,
        ramp: run.lint.ramp,
        off: run.lint.off,
        chrome: [...(p.chrome ?? [])],
        overlayOnly: OVERLAY_KINDS.includes(screen.kind),
        cap: 25,
      };
      const result = await page.evaluate(lintPage, options);
      addGlassBlur(result, await probeGlassBlur(page), run.lint.off, options.cap);
      record.lint = lintSummary(result);
      record.safeArea.emulated = result.insetsEmulated;
      lap('lint');
      if (screen.full !== false) {
        record.full = await shootFull(page, files.full);
        if (record.full) record.files.full = path.basename(files.full);
        lap('fullShot');
      }
      if (record.themeActual !== theme) fail('theme-mismatch', `asked for ${theme}, the page rendered ${record.themeActual ?? 'no readable theme'}`);
      const after = await readBuildHealth(page);
      if (after.red) fail('build-error', `went red during the full shot: ${after.error ?? 'build red'}`);
    }
  } catch (error) {
    if (error instanceof UnresolvedParam) {
      fail('unreachable', error.message);
      return;
    }
    // Harness-level failure (crashed page, broken shot): recorded AND rethrown.
    fail('error', firstLine(error));
    throw error;
  } finally {
    finish();
    await context.close();
  }
}
