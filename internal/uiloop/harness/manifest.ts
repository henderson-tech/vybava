// The screen manifest's vocabulary — the contract every repo's ui-loop map is
// written against. A repo owns its entries (ui-loop/screens/<area>.ts) and its
// glue (ui-loop/project.ts); this file, like the rest of vendor/, is synced by
// `vybava ui-loop sync` and drift-checked by `vybava ui-loop check`. Never edit
// the vendored copy: change it in Výbava.
//
// Field names follow the Reservine UI-audit manifest and voke's responsive
// manifest (kind · parentId · sourceFiles · knownIssues · unreachable ·
// viewports · theme · destructive), so both move onto it as a mapping.

import type { Page } from '@playwright/test';

/**
 * What a screen is. `page` = a route · `tab` = a routed or local view switch of
 * a page · `wizard-step` · `panel` = docked/side panel · `layer` = a URL-routed
 * overlay (pwf-ui layers, Reservine app.layers) · `dialog` · `drawer` · `sheet`
 * (a phone bottom sheet) · `popover` (menus included) · `state` = its parent
 * screen in one data or UI state.
 */
export const SCREEN_KINDS = [
  'page',
  'tab',
  'wizard-step',
  'panel',
  'layer',
  'dialog',
  'drawer',
  'sheet',
  'popover',
  'state',
] as const;
export type ScreenKind = (typeof SCREEN_KINDS)[number];

/**
 * The state a `kind: 'state'` screen shows. empty / error / loading are reached
 * with request mocks in `before` (states.ts) — never by mutating seed data.
 * `denied` = the user lacks the right · `read-only` = can see, cannot act ·
 * `long-text` = the stress data (long names, subjects, locales).
 */
export const SCREEN_STATES = [
  'empty',
  'error',
  'loading',
  'filtered',
  'unsaved',
  'stacked',
  'selected',
  'expanded',
  'maximized',
  'denied',
  'read-only',
  'long-text',
] as const;
export type ScreenState = (typeof SCREEN_STATES)[number];

export type Theme = 'light' | 'dark';

/**
 * One declarative recipe step. `{PARAM}` placeholders in any string are filled
 * from the project's params (project.ts `params`). Selectors are Playwright
 * selectors; every locator step acts on the first VISIBLE match.
 * `clickRole` resolves the topmost overlay first (an open dialog wins over the
 * list behind it), which `clickText` does not.
 */
export type Step =
  | { goto: string }
  | { click: string }
  | { clickText: string }
  | { clickRole: { role: string; name: string; exact?: boolean } }
  | { fill: { selector: string; value: string } }
  | { waitFor: string }
  | { press: string }
  | { hover: string }
  | { dblclick: string }
  | { longPress: string }
  | { drag: { from: string; to: string } }
  | { evaluate: string }
  | { upload: { trigger: string; file: string } };

/** What a recipe function is handed. */
export interface RecipeContext {
  /** Params resolved for this run (ids harvested from the running app, fixtures). */
  params: Readonly<Record<string, string>>;
  /** The screen's app base URL for this run. */
  baseUrl: string;
  app: string;
  viewport: string;
  theme: Theme;
}

export type Recipe = (page: Page, ctx: RecipeContext) => Promise<void>;

/** A recipe is data (steps, rendered into the app map verbatim) or a Playwright function. */
export type Open = readonly Step[] | Recipe;

export interface Screen<Area extends string = string, As extends string = string> {
  /** Stable kebab-case id; the capture file name and the board card key derive from it. */
  id: string;
  /** Which app of the repo serves it (config `uiLoop.apps`); omitted = the only app. */
  app?: string;
  area: Area;
  kind: ScreenKind;
  /** Required exactly when `kind` is 'state'. */
  state?: ScreenState;
  title: string;
  /** The route template, with `{PARAM}` placeholders: `#/task/teamleader/{TASK_ACCEPTANCE}`. */
  route: string;
  /** The screen this one opens from; omitted for a root. The app map nests by it. */
  parentId?: string;
  /** The base screen this entry re-shoots in another persona/locale — folded into its base in the map. */
  variantOf?: string;
  /** Who is signed in (project.ts `login`); omitted = the project's default identity. */
  as?: As;
  /** Viewport ids (config `uiLoop.viewports`) this screen exists at; omitted = its app's set. */
  viewports?: readonly string[];
  /** Themes it is shot in; omitted = its app's set. One entry = the page forces that scheme. */
  themes?: readonly Theme[];
  /** Runs on the fresh page before navigation: request mocks, storage, routing (states.ts). */
  before?: Recipe;
  /** After navigation settled: drives into the overlay/state and ends WAITING for it, so a pass proves it got there. */
  open?: Open;
  /** Content that loads after the network settles: a selector to wait for, or a recipe. */
  ready?: string | Recipe;
  /** Context options: timezone (default Europe/Prague), locale, localStorage written before boot (`null` removes). */
  context?: {
    timezoneId?: string;
    locale?: string;
    localStorage?: Readonly<Record<string, string | null>>;
  };
  /** false: no full-page companion shot (an infinite list has no end). */
  full?: false;
  /** The implementing file plus the 1–2 components that own its look (repo-relative). Fix lanes route by these. */
  sourceFiles: readonly string[];
  /** Defects seen while mapping. A recipe that had to use a text/role selector adds `missing testid: …`. */
  knownIssues?: readonly string[];
  /** Why the dev stack cannot reach it — recorded as `unreachable`, never faked, never shot. */
  unreachable?: string;
  /** The recipe writes data (claims, sends, creates): shot last and only with `--destructive`. */
  destructive?: true;
}

/** Per-area open questions and cross-cutting findings, rendered into the app map. */
export interface ManifestNotes {
  areas?: Readonly<Record<string, readonly string[]>>;
  crossCutting?: readonly string[];
  decisions?: readonly string[];
}

/** Identity helper that keeps literal ids and areas inferred (`as const` without the ceremony). */
export function defineScreens<const S extends readonly Screen[]>(screens: S): S {
  return screens;
}

/**
 * Structural checks the capture and the app-map renderer both run: unique ids,
 * a known parent and variantOf, `state` exactly on state screens. Returns
 * problems; an empty list is a valid manifest.
 */
export function validateScreens(screens: readonly Screen[]): string[] {
  const problems: string[] = [];
  const ids = new Set<string>();
  for (const s of screens) {
    if (ids.has(s.id)) problems.push(`${s.id}: duplicate id`);
    ids.add(s.id);
    if (!/^[a-z0-9]+(-[a-z0-9]+)*$/.test(s.id)) problems.push(`${s.id}: id is not kebab-case`);
    if ((s.kind === 'state') !== (s.state !== undefined))
      problems.push(`${s.id}: \`state\` belongs on kind 'state' screens only, and they need it`);
    if (!s.sourceFiles.length && !s.unreachable) problems.push(`${s.id}: no sourceFiles`);
  }
  for (const s of screens) {
    if (s.parentId && !ids.has(s.parentId)) problems.push(`${s.id}: unknown parentId ${s.parentId}`);
    if (s.variantOf && !ids.has(s.variantOf)) problems.push(`${s.id}: unknown variantOf ${s.variantOf}`);
  }
  return problems;
}
