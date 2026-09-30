// run.json — the contract between `vybava ui-loop run` (which resolves the
// repo's uiLoop config, the selection and the pass directory) and the
// capture, which only reads it. The capture never needs the vybava binary: a
// Devbox container that holds the repo runs the printed command as is.
//
// Paths in run.json are repo-relative. The repo root is UILOOP_ROOT (the
// printed command sets it to "$PWD"), else the process's cwd.

import * as fs from 'node:fs';
import * as path from 'node:path';

import type { Screen, Theme } from './manifest';
import type { Project } from './project';
import type { Viewport } from './viewports';

export const RUN_VERSION = 1;

export interface AppConfig {
  baseUrl: string;
  /** Env var that overrides baseUrl; the capture machine's value wins. */
  env?: string;
  viewports: string[];
  themes: Theme[];
}

/** The part of the uiLoop config the TS side needs (check, map, capture). */
export interface LoopConfig {
  dir: string;
  appMap: string;
  areas: string[];
  apps: Record<string, AppConfig>;
  viewports: Record<string, Viewport>;
}

export interface LintConfig {
  grid: number;
  touchTarget: number;
  off: string[];
  ramp: number[];
  /** Rule id → selectors whose hits count as info (uiLoop.lint.allow); absent in older run.json. */
  allow?: Record<string, string[]>;
}

export interface Selection {
  /** Empty = every app. */
  apps: string[];
  /** Screen ids or area names; a trailing `*` is a prefix. Empty = everything. */
  only: string[];
  /** Empty = each screen's own set. */
  viewports: string[];
  themes: Theme[];
  destructive: boolean;
  resume: boolean;
}

export interface RunFile extends LoopConfig {
  v: number;
  pass: number;
  /** Repo-relative pass directory: <out>/pass-<n>. */
  passDir: string;
  /** Version of the vybava binary that wrote run.json. */
  vybava: string;
  createdAt: string;
  selection: Selection;
  lint: LintConfig;
  /** Seconds a shot waits for a red dev server to turn green. */
  buildWait: number;
  workers: number;
}

export function repoRoot(): string {
  return path.resolve(process.env['UILOOP_ROOT'] || process.cwd());
}

export function fromRoot(rel: string): string {
  return path.resolve(repoRoot(), rel);
}

let cached: RunFile | undefined;

export function loadRun(): RunFile {
  if (cached) return cached;
  const file = process.env['UILOOP_RUN'];
  if (!file) throw new Error('UILOOP_RUN is not set: run the capture through `vybava ui-loop run` (or its --print command)');
  const run = JSON.parse(fs.readFileSync(fromRoot(file), 'utf-8')) as RunFile;
  if (run.v !== RUN_VERSION) throw new Error(`${file}: run.json v${run.v}, this harness reads v${RUN_VERSION} — re-sync the harness (vybava ui-loop sync)`);
  cached = run;
  return run;
}

/** UILOOP_CONFIG: the JSON LoopConfig `vybava ui-loop check|map` hands check.ts and render-app-map.ts. */
export function loadConfigEnv(): LoopConfig {
  const raw = process.env['UILOOP_CONFIG'];
  if (!raw) throw new Error('UILOOP_CONFIG is not set: run this through `vybava ui-loop check` or `vybava ui-loop map`');
  return JSON.parse(raw) as LoopConfig;
}

export function passPath(run: RunFile, ...parts: string[]): string {
  return path.join(fromRoot(run.passDir), ...parts);
}

/** The base URL an app is shot on: its env var when set here, else run.json's. */
export function baseUrlOf(run: RunFile, app: string): string {
  const cfg = run.apps[app];
  if (!cfg) throw new Error(`unknown app "${app}"`);
  const override = cfg.env ? process.env[cfg.env] : undefined;
  return override || cfg.baseUrl;
}

// ── Params ────────────────────────────────────────────────────────────────────────────────────

const PLACEHOLDER = /\{([A-Za-z_][A-Za-z0-9_]*)\}/g;

export interface Filled {
  value: string;
  missing: string[];
}

/**
 * Fills `{PARAM}` placeholders. Unknown names are reported as missing and left
 * in place — except in loose mode (an `evaluate` step's JavaScript, where
 * `{x}` may be an object literal), which only substitutes known names.
 */
export function fill(value: string, params: Readonly<Record<string, string>>, loose = false): Filled {
  const missing: string[] = [];
  const out = value.replace(PLACEHOLDER, (token, name: string) => {
    const v = params[name];
    if (v !== undefined) return v;
    if (!loose) missing.push(name);
    return token;
  });
  return { value: out, missing };
}

// ── Selection: which screen × viewport × theme a run shoots ──────────────────────────────────

export interface PlannedShot {
  screen: Screen;
  /** Index in the manifest: report and publish order. */
  order: number;
  app: string;
  viewport: string;
  theme: Theme;
}

function matches(screen: Screen, tokens: readonly string[]): boolean {
  return tokens.some((token) => {
    if (token.endsWith('*')) {
      const prefix = token.slice(0, -1);
      return screen.id.startsWith(prefix) || screen.area.startsWith(prefix);
    }
    return screen.id === token || screen.area === token;
  });
}

export function appOf(screen: Screen, apps: Readonly<Record<string, AppConfig>>): string | null {
  if (screen.app) return apps[screen.app] ? screen.app : null;
  const names = Object.keys(apps);
  return names.length === 1 ? names[0]! : null;
}

/**
 * Every shot the run selects, destructive recipes last. Throws on a screen
 * whose app, viewport or theme the config does not know — `ui-loop check`
 * reports the same problems before a run.
 */
export function plannedShots(project: Project, run: RunFile): PlannedShot[] {
  const { selection } = run;
  const picked: PlannedShot[] = [];
  const late: PlannedShot[] = [];
  project.screens.forEach((screen, order) => {
    if (selection.only.length && !matches(screen, selection.only)) return;
    if (screen.destructive && !selection.destructive) return;
    const app = appOf(screen, run.apps);
    if (!app) throw new Error(`${screen.id}: no app (set \`app\`; the config has ${Object.keys(run.apps).join(', ')})`);
    if (selection.apps.length && !selection.apps.includes(app)) return;
    const cfg = run.apps[app]!;
    const viewports = (screen.viewports ?? cfg.viewports).filter(
      (v) => !selection.viewports.length || selection.viewports.includes(v),
    );
    const themes = (screen.themes ?? cfg.themes).filter(
      (t) => !selection.themes.length || selection.themes.includes(t),
    );
    shots: for (const viewport of viewports) {
      if (!run.viewports[viewport]) throw new Error(`${screen.id}: unknown viewport "${viewport}"`);
      for (const theme of themes) {
        (screen.destructive ? late : picked).push({ screen, order, app, viewport, theme });
        // `once`: the first viewport × theme the run selects, and no other.
        if (screen.once) break shots;
      }
    }
  });
  return [...picked, ...late];
}

/** The identities a run signs in: app → the `as` values its selected screens use. */
export function identities(project: Project, shots: readonly PlannedShot[]): Map<string, Set<string>> {
  const out = new Map<string, Set<string>>();
  for (const shot of shots) {
    const as = shot.screen.as ?? project.defaultAs;
    if (as === undefined) continue;
    const set = out.get(shot.app) ?? new Set<string>();
    set.add(as);
    out.set(shot.app, set);
  }
  return out;
}

// ── Pass layout ───────────────────────────────────────────────────────────────────────────────

/** Screen ids are kebab-case; this only guards a directory name. */
export function safeId(id: string): string {
  return id.replace(/[^a-zA-Z0-9._-]+/g, '_');
}

export interface ShotFiles {
  dir: string;
  base: string;
  png: string;
  full: string;
  json: string;
}

/** `<passDir>/shots/<id>/<viewport>.<theme>.{png,full.png,json}` */
export function shotFiles(run: RunFile, id: string, viewport: string, theme: Theme): ShotFiles {
  const dir = passPath(run, 'shots', safeId(id));
  const base = `${viewport}.${theme}`;
  return {
    dir,
    base,
    png: path.join(dir, `${base}.png`),
    full: path.join(dir, `${base}.full.png`),
    json: path.join(dir, `${base}.json`),
  };
}

export const PARAMS_FILE = 'params.json';
export const AUTH_FILE = 'auth.json';

/** `app/as` → storage-state path (absolute), written by setup.ts. */
export type AuthMap = Record<string, string>;

export function authKey(app: string, as: string): string {
  return `${app}/${as}`;
}

export function readJson<T>(file: string): T | null {
  if (!fs.existsSync(file)) return null;
  return JSON.parse(fs.readFileSync(file, 'utf-8')) as T;
}

export function writeJson(file: string, value: unknown): void {
  fs.mkdirSync(path.dirname(file), { recursive: true });
  const tmp = `${file}.tmp-${process.pid}`;
  fs.writeFileSync(tmp, `${JSON.stringify(value, null, 2)}\n`, 'utf-8');
  fs.renameSync(tmp, file);
}
