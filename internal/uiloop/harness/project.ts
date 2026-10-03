// A repo's glue for the ui-loop harness: ui-loop/project.ts default-exports
// defineProject({...}). Everything app-specific — how to sign in, where ids
// come from, how the app is told its theme, when it is idle — lives there;
// the vendored harness never learns a repo's names.

import type { BrowserContext, Page } from '@playwright/test';
import type { ManifestNotes, Screen, Theme } from './manifest';

export interface RunContext {
  /** Repo-relative ui-loop directory (config `uiLoop.dir`). */
  dir: string;
  /** This run's output directory (captures, report.json, params.json). */
  out: string;
  /** App name → base URL for this run (config, overridden by each app's env var). */
  baseUrls: Readonly<Record<string, string>>;
}

export interface Project<Area extends string = string, As extends string = string> {
  screens: readonly Screen<Area, As>[];
  notes?: ManifestNotes;
  /** The identity a screen without `as` is shot as. */
  defaultAs?: As;
  /**
   * Resolves `{PARAM}` placeholders once per run (read ids from the running
   * app, a fixture file, an env var). Cached in `<out>/params.json`; a screen
   * whose route or steps name an unresolved param is recorded `unreachable`.
   */
  params?(run: RunContext): Promise<Record<string, string>>;
  /** Data preparation before a run (a reseed, a re-date). Must be idempotent and never destroy data. */
  prepare?(run: RunContext): Promise<void>;
  /**
   * Signs `as` in once per run, on `app`. Either drive the login page and let
   * the harness save the storage state, or return a storage-state file path.
   */
  login?(page: Page, as: As, app: string, run: RunContext): Promise<void | string>;
  /**
   * True when the page shows the sign-in screen instead of the screen (an
   * expired session). A signed-in shot that lands there is `recipe-failed`,
   * never `ok` — a `--resume` signs in again and re-takes it.
   */
  signedOut?(page: Page, as: As, app: string): Promise<boolean>;
  /** Puts the app into `theme` before navigation (a localStorage key, a class, a query flag). Default: emulate prefers-color-scheme. */
  theme?(context: BrowserContext, theme: Theme, app: string): Promise<void>;
  /** Reads the theme the page actually rendered, so a mismatch is recorded, never shot silently. */
  readTheme?(page: Page): Promise<Theme | null>;
  /** App-specific idle: spinners gone, lazy chunks in. Runs after the harness's own network settle. */
  settle?(page: Page, screen: Screen<Area, As>): Promise<void>;
  /** Selectors of app chrome the lint treats as painters of their own background (sticky bars, docks). */
  chrome?: readonly string[];
  /**
   * Freezes `Date` at run.json's `clock` (Playwright `page.clock.setFixedTime`; timers keep
   * running), so a screen that shows the time shoots the same pixels every pass and an
   * unmoved screen carries its review. Opt-in: the app sees that instant as now.
   */
  freezeClock?: boolean;
}

export function defineProject<const Area extends string, const As extends string>(
  project: Project<Area, As>,
): Project<Area, As> {
  return project;
}
