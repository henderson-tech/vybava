// The capture mechanics: context per shot, safe-area emulation, console and
// network observation, the recipe runner, settle, build health, motion
// freeze, viewport + full shots, the glass probe and the per-shot record.
// capture.spec.ts wires them; graduated from Reservine's ui-audit and voke's
// responsive capture.

import * as fs from 'node:fs';
import * as path from 'node:path';

import { type BrowserContextOptions, devices, type Page, type Request } from '@playwright/test';

import { type LintResult, type LintRule, lintCounts } from './lint';
import type { Open, RecipeContext, Screen, ScreenKind, ScreenState, Step, Theme } from './manifest';
import { fill, fromRoot } from './run';
import { DPR, hasInsets, insetsOf, type Viewport } from './viewports';

export const RECORD_VERSION = 1;

// ── Context ───────────────────────────────────────────────────────────────────────────────────

const PHONE_UA = devices['iPhone 15'].userAgent;
const TABLET_UA = devices['iPad Pro 11'].userAgent;

export function contextOptions(
  viewport: Viewport,
  screen: Screen,
  theme: Theme,
  baseURL: string,
  storageState: string | undefined,
): BrowserContextOptions {
  const size = { width: viewport.width, height: viewport.height };
  const options: BrowserContextOptions = {
    baseURL,
    viewport: size,
    screen: size,
    deviceScaleFactor: DPR,
    isMobile: viewport.mobile === true,
    hasTouch: viewport.mobile === true,
    colorScheme: theme,
    reducedMotion: 'reduce',
    timezoneId: screen.context?.timezoneId ?? 'Europe/Prague',
  };
  if (viewport.mobile) options.userAgent = Math.min(viewport.width, viewport.height) < 600 ? PHONE_UA : TABLET_UA;
  if (screen.context?.locale) options.locale = screen.context.locale;
  if (storageState) options.storageState = storageState;
  return options;
}

/** The screen's `context.localStorage`, written before the app boots on every navigation. */
export async function seedLocalStorage(page: Page, screen: Screen): Promise<void> {
  const entries = Object.entries(screen.context?.localStorage ?? {});
  if (!entries.length) return;
  await page.addInitScript((pairs: Array<[string, string | null]>) => {
    try {
      for (const [key, value] of pairs) {
        if (value === null) window.localStorage.removeItem(key);
        else window.localStorage.setItem(key, value);
      }
    } catch {
      // Opaque origins (about:blank) have no storage — nothing to seed there.
    }
  }, entries);
}

/** Default theme read-back: `<html data-theme>`, else a `dark`/`light` class, else the colour scheme. */
export async function defaultReadTheme(page: Page): Promise<Theme | null> {
  return page.evaluate(() => {
    const root = document.documentElement;
    const data = root.dataset['theme'];
    if (data === 'dark' || data === 'light') return data;
    if (root.classList.contains('dark')) return 'dark';
    if (root.classList.contains('light')) return 'light';
    return window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
  });
}

// ── Safe area ─────────────────────────────────────────────────────────────────────────────────

export interface SafeAreaState {
  path: 'cdp' | 'none' | 'unsupported';
  error: string | null;
}

/** `Emulation.setSafeAreaInsetsOverride` is newer than Playwright's bundled protocol typings. */
interface RawCdpSession {
  send(method: string, params: Record<string, unknown>): Promise<unknown>;
}

/**
 * Emulates the viewport's insets through CDP so the real `env(safe-area-inset-*)`
 * resolves in-page. A Chromium without the method is recorded, never faked.
 */
export async function emulateSafeArea(page: Page, viewport: Viewport): Promise<SafeAreaState> {
  if (!hasInsets(viewport)) return { path: 'none', error: null };
  const session = (await page.context().newCDPSession(page)) as unknown as RawCdpSession;
  try {
    await session.send('Emulation.setSafeAreaInsetsOverride', { insets: insetsOf(viewport) });
    return { path: 'cdp', error: null };
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error);
    if (!/not found|wasn't found|Unknown method/i.test(message)) throw error;
    return { path: 'unsupported', error: message.split('\n')[0] ?? message };
  }
}

// ── Console + network observation ─────────────────────────────────────────────────────────────

const MAX_CONSOLE_ERRORS = 50;

export function collectConsoleErrors(page: Page): string[] {
  const errors: string[] = [];
  const push = (text: string): void => {
    if (errors.length < MAX_CONSOLE_ERRORS) errors.push(text.slice(0, 300));
  };
  page.on('console', (message) => {
    if (message.type() !== 'error') return;
    const text = message.text();
    // 'Failed to load resource: … 403' names no request; its location is the resource (path only).
    const resource = text.startsWith('Failed to load resource') ? message.location().url.split('?')[0] : '';
    push(`console: ${text}${resource ? ` — ${resource}` : ''}`);
  });
  page.on('pageerror', (error) => push(`pageerror: ${error.message}`));
  return errors;
}

/** Streams, dev-server sockets and telemetry never "finish" — they must not block quiet. */
const UNTRACKED_URL = /(sockjs|ng-cli-ws|livereload|hot-update|__webpack_hmr|@vite\/client|umami|sentry|broadcasting)/i;

export class NetworkTracker {
  private readonly inflight = new Set<Request>();

  constructor(page: Page) {
    page.on('request', (request) => {
      const type = request.resourceType();
      if (type === 'eventsource' || type === 'websocket' || UNTRACKED_URL.test(request.url())) return;
      this.inflight.add(request);
    });
    page.on('requestfinished', (request) => this.inflight.delete(request));
    page.on('requestfailed', (request) => this.inflight.delete(request));
  }

  /** True once nothing tracked is in flight for `idleMs`, false at `timeoutMs`. */
  async quiet(idleMs = 500, timeoutMs = 6_000): Promise<boolean> {
    const deadline = Date.now() + timeoutMs;
    let idleSince = this.inflight.size === 0 ? Date.now() : 0;
    while (Date.now() < deadline) {
      if (this.inflight.size === 0) {
        if (!idleSince) idleSince = Date.now();
        if (Date.now() - idleSince >= idleMs) return true;
      } else {
        idleSince = 0;
      }
      await new Promise((resolve) => setTimeout(resolve, 100));
    }
    return false;
  }

  pending(): string[] {
    return Array.from(this.inflight, (request) => request.url().split('?')[0] ?? '').slice(0, 5);
  }
}

// ── Recipe runner ─────────────────────────────────────────────────────────────────────────────

const STEP_TIMEOUT = 15_000;
export const NAV_TIMEOUT = 45_000;

export class UnresolvedParam extends Error {
  constructor(readonly names: string[]) {
    super(`unresolved param ${names.map((n) => `{${n}}`).join(', ')}`);
  }
}

function filled(value: string, params: Readonly<Record<string, string>>): string {
  const { value: out, missing } = fill(value, params);
  if (missing.length) throw new UnresolvedParam(missing);
  return out;
}

/** Absolute manifest URLs keep their path but are served from this run's origin. */
export function resolveUrl(raw: string, baseUrl: string): string {
  if (/^https?:\/\//i.test(raw)) {
    const url = new URL(raw);
    return new URL(`${url.pathname}${url.search}${url.hash}`, baseUrl).toString();
  }
  return new URL(raw, baseUrl).toString();
}

function visibleFirst(page: Page, selector: string) {
  return page.locator(selector).filter({ visible: true }).first();
}

/** Topmost overlay first: an open dialog's control wins over the same label behind it. */
async function clickRole(page: Page, role: string, name: string, exact: boolean): Promise<void> {
  type Role = Parameters<Page['getByRole']>[0];
  const overlays = page.locator('[role=dialog], [role=alertdialog], dialog[open], [role=menu], [role=listbox], .cdk-overlay-pane').filter({ visible: true });
  const count = await overlays.count();
  for (let i = count - 1; i >= 0; i--) {
    const inOverlay = overlays.nth(i).getByRole(role as Role, { name, exact }).filter({ visible: true }).first();
    if (await inOverlay.count()) {
      await inOverlay.click({ timeout: STEP_TIMEOUT });
      return;
    }
  }
  await page.getByRole(role as Role, { name, exact }).filter({ visible: true }).first().click({ timeout: STEP_TIMEOUT });
}

async function clickText(page: Page, text: string): Promise<void> {
  const named = { name: text, exact: true };
  const exact = page
    .getByRole('button', named)
    .or(page.getByRole('link', named))
    .or(page.getByRole('tab', named))
    .or(page.getByRole('menuitem', named))
    .or(page.getByRole('option', named))
    .or(page.getByText(text, { exact: true }))
    .filter({ visible: true })
    .first();
  try {
    await exact.click({ timeout: STEP_TIMEOUT });
  } catch (error) {
    if (!(error instanceof Error) || error.name !== 'TimeoutError') throw error;
    // No exact label within the timeout — a substring match is the last resort.
    await page.getByText(text).filter({ visible: true }).first().click({ timeout: 3_000 });
  }
}

export async function runStep(page: Page, step: Step, params: Readonly<Record<string, string>>, baseUrl: string): Promise<void> {
  const f = (v: string): string => filled(v, params);
  if ('goto' in step) {
    await page.goto(resolveUrl(f(step.goto), baseUrl), { waitUntil: 'load', timeout: NAV_TIMEOUT });
  } else if ('click' in step) {
    await visibleFirst(page, f(step.click)).click({ timeout: STEP_TIMEOUT });
  } else if ('clickText' in step) {
    await clickText(page, f(step.clickText));
  } else if ('clickRole' in step) {
    await clickRole(page, step.clickRole.role, f(step.clickRole.name), step.clickRole.exact ?? true);
  } else if ('fill' in step) {
    const host = visibleFirst(page, f(step.fill.selector));
    await host.waitFor({ state: 'visible', timeout: STEP_TIMEOUT });
    const editable = await host.evaluate((el) => el.matches('input, textarea, select, [contenteditable]'));
    const field = editable ? host : host.locator('input, textarea, [contenteditable]').first();
    await field.fill(f(step.fill.value), { timeout: STEP_TIMEOUT });
  } else if ('waitFor' in step) {
    await visibleFirst(page, f(step.waitFor)).waitFor({ state: 'visible', timeout: STEP_TIMEOUT });
  } else if ('press' in step) {
    await page.keyboard.press(step.press);
  } else if ('hover' in step) {
    await visibleFirst(page, f(step.hover)).hover({ timeout: STEP_TIMEOUT });
  } else if ('dblclick' in step) {
    await visibleFirst(page, f(step.dblclick)).dblclick({ timeout: STEP_TIMEOUT });
  } else if ('longPress' in step) {
    // Held past the usual 500 ms long-press threshold.
    await visibleFirst(page, f(step.longPress)).hover({ timeout: STEP_TIMEOUT });
    await page.mouse.down();
    await page.waitForTimeout(700);
    await page.mouse.up();
  } else if ('drag' in step) {
    await visibleFirst(page, f(step.drag.from)).dragTo(visibleFirst(page, f(step.drag.to)), { timeout: STEP_TIMEOUT });
  } else if ('evaluate' in step) {
    await page.evaluate(fill(step.evaluate, params, true).value);
  } else if ('upload' in step) {
    const chooser = page.waitForEvent('filechooser', { timeout: STEP_TIMEOUT });
    await visibleFirst(page, f(step.upload.trigger)).click({ timeout: STEP_TIMEOUT });
    await (await chooser).setFiles(fromRoot(step.upload.file));
  } else {
    const unknown: never = step;
    throw new Error(`unknown recipe step ${JSON.stringify(unknown)}`);
  }
}

export interface RecipeFailure {
  step: string | null;
  stepIndex: number | null;
  error: string;
}

const firstLine = (error: unknown): string =>
  (error instanceof Error ? (error.message.split('\n')[0] ?? error.message) : String(error)).slice(0, 500);

/**
 * Runs `open` — steps one by one, or the recipe function. A failing step is the
 * screen's result, never the run's: it is returned so the shot records
 * `recipe-failed` and still shoots what rendered.
 */
export async function runOpen(page: Page, open: Open | undefined, ctx: RecipeContext): Promise<RecipeFailure | null> {
  if (!open) return null;
  if (typeof open === 'function') {
    try {
      await open(page, ctx);
      return null;
    } catch (error) {
      if (error instanceof UnresolvedParam) throw error;
      return { step: 'open()', stepIndex: null, error: firstLine(error) };
    }
  }
  for (const [index, step] of open.entries()) {
    try {
      await runStep(page, step, ctx.params, ctx.baseUrl);
    } catch (error) {
      if (error instanceof UnresolvedParam) throw error;
      return { step: JSON.stringify(step), stepIndex: index, error: firstLine(error) };
    }
  }
  return null;
}

/** Every `{PARAM}` the screen's route and steps need, so a missing one is `unreachable` before the browser starts. */
export function missingParams(screen: Screen, params: Readonly<Record<string, string>>): string[] {
  const strings: string[] = [screen.route];
  if (Array.isArray(screen.open)) {
    for (const step of screen.open as readonly Step[]) {
      if ('evaluate' in step || 'press' in step) continue;
      strings.push(...Object.values(step).flatMap((v) => (typeof v === 'string' ? [v] : Object.values(v as object).filter((x): x is string => typeof x === 'string'))));
    }
  }
  if (typeof screen.ready === 'string') strings.push(screen.ready);
  return [...new Set(strings.flatMap((s) => fill(s, params).missing))];
}

// ── Settle ────────────────────────────────────────────────────────────────────────────────────

async function twoFrames(page: Page): Promise<void> {
  await page.evaluate(() => new Promise<void>((resolve) => requestAnimationFrame(() => requestAnimationFrame(() => resolve()))));
}

/**
 * Quiet-ish settle — never `networkidle`, a live app never settles: load, fonts,
 * tracked requests idle (capped), finite animations done (capped), two frames.
 * Returns what did NOT settle.
 */
export async function settle(page: Page, network: NetworkTracker): Promise<string[]> {
  const notes: string[] = [];
  await page.waitForLoadState('load', { timeout: NAV_TIMEOUT });
  await page.evaluate(() => document.fonts.ready.then(() => undefined));
  if (!(await network.quiet())) notes.push(`network busy: ${network.pending().join(', ')}`);
  await page.waitForTimeout(300);
  const animating = await page
    .evaluate(() => {
      const running = document
        .getAnimations()
        .filter((a) => a.playState === 'running')
        .filter((a) => Number.isFinite(a.effect?.getComputedTiming().iterations ?? 1));
      const done = Promise.all(running.map((a) => a.finished.catch(() => undefined)));
      const cap = new Promise<'timeout'>((resolve) => setTimeout(() => resolve('timeout'), 1500));
      return Promise.race([done.then(() => running.length), cap]);
    })
    .catch((error: unknown) => `navigated: ${firstLine(error)}`);
  if (animating === 'timeout') notes.push('animations still running after 1.5s');
  else if (typeof animating === 'string') notes.push(`animation wait skipped (${animating})`);
  await twoFrames(page);
  return notes;
}

// ── Build health: a red shared dev server must never yield an `ok` shot ─────────────────────────

export interface BuildHealth {
  red: boolean;
  error: string | null;
}

/** Red = a dev-server error overlay is painted (Vite, and the Angular/esbuild dev server that uses it; webpack's). */
export async function readBuildHealth(page: Page): Promise<BuildHealth> {
  return page
    .evaluate(() => {
      const overlay = document.querySelector('vite-error-overlay');
      if (overlay) {
        const root = overlay.shadowRoot;
        const message = root?.querySelector('.message-body')?.textContent ?? root?.querySelector('.message')?.textContent ?? overlay.textContent ?? '';
        const line = message.split('\n').map((l) => l.trim()).find(Boolean);
        return { red: true, error: (line ?? 'Vite error overlay').slice(0, 300) };
      }
      const webpack = document.querySelector('#webpack-dev-server-client-overlay');
      if (webpack) return { red: true, error: 'webpack dev-server error overlay' };
      return { red: false, error: null };
    })
    .catch((error: unknown) => ({ red: false, error: `health unreadable: ${firstLine(error)}` }));
}

const SERVER_DOWN = /net::ERR_(CONNECTION_REFUSED|CONNECTION_RESET|EMPTY_RESPONSE|CONNECTION_CLOSED)/;

/** Reloads every 15 s until the build is green or `maxSeconds` pass; returns the last health. */
export async function waitForGreenBuild(page: Page, maxSeconds: number): Promise<BuildHealth> {
  const deadline = Date.now() + maxSeconds * 1000;
  let health = await readBuildHealth(page);
  while (health.red && Date.now() < deadline) {
    await page.waitForTimeout(15_000);
    try {
      await page.reload({ waitUntil: 'load', timeout: NAV_TIMEOUT });
    } catch (error) {
      if (!(error instanceof Error)) throw error;
      if (SERVER_DOWN.test(error.message)) {
        health = { red: true, error: `dev server unreachable: ${firstLine(error)}` };
        continue;
      }
      if (error.name !== 'TimeoutError') throw error;
    }
    health = await readBuildHealth(page);
  }
  return health;
}

// ── Shots ─────────────────────────────────────────────────────────────────────────────────────

const KILL_MOTION_CSS =
  '*,*::before,*::after{animation-duration:0s!important;animation-delay:0s!important;' +
  'transition-duration:0s!important;transition-delay:0s!important;caret-color:transparent!important}';

const FADE_PROPS = ['--scroll-fade-top', '--scroll-fade-bottom', '--scroll-fade-start', '--scroll-fade-end'];

/**
 * Scroll-driven animations (scroll fades) end at a percentage, so `animations:
 * 'disabled'` cancels them and no shot would show a fade: each element's
 * current value is pinned inline before the freeze.
 */
export async function freezeMotion(page: Page): Promise<void> {
  await page.evaluate((props) => {
    for (const el of Array.from(document.querySelectorAll<HTMLElement>('*'))) {
      const style = getComputedStyle(el);
      if (!style.getPropertyValue('animation-timeline').includes('scroll')) continue;
      for (const p of props) {
        const v = style.getPropertyValue(p).trim();
        if (v) el.style.setProperty(p, v);
      }
    }
  }, FADE_PROPS);
  await page.addStyleTag({ content: KILL_MOTION_CSS });
  await twoFrames(page);
}

export async function shootViewport(page: Page, file: string): Promise<void> {
  await page.screenshot({ path: file, animations: 'disabled', caret: 'hide', scale: 'device', timeout: 30_000 });
}

export const FULL_SHOT_CAP = 6_000;

export interface FullShot {
  target: string;
  scrollHeight: number;
  clientHeight: number;
  capped: boolean;
}

/**
 * The full companion, only when the page scrolls: the largest inner scroll
 * container (inside the topmost open dialog when there is one, never nav/aside)
 * is grown to its scrollHeight (≤ 6000 CSS px), its ancestors stop clipping, it
 * is element-shot, and every touched inline style is restored. A document that
 * scrolls itself gets a clipped full-page shot with its fixed chrome hidden.
 */
export async function shootFull(page: Page, file: string): Promise<FullShot | null> {
  const target = await page.evaluate((cap) => {
    const rendered = (el: Element): boolean => {
      const rect = el.getBoundingClientRect();
      return rect.width > 0 && rect.height > 0 && getComputedStyle(el).visibility !== 'hidden';
    };
    const parentOf = (el: Element): Element | null => {
      if (el.parentElement) return el.parentElement;
      const root = el.getRootNode();
      return root instanceof ShadowRoot ? root.host : null;
    };
    const deep = (root: Element): Element[] => {
      const out: Element[] = [root];
      const walk = (node: Element | ShadowRoot): void => {
        for (const el of Array.from(node.querySelectorAll('*'))) {
          out.push(el);
          if (el.shadowRoot) walk(el.shadowRoot);
        }
      };
      if (root.shadowRoot) walk(root.shadowRoot);
      walk(root);
      return out;
    };
    const closestDeep = (el: Element, selector: string): Element | null => {
      for (let cur: Element | null = el; cur; cur = parentOf(cur)) if (cur.matches(selector)) return cur;
      return null;
    };
    const saved: Array<[HTMLElement, string]> = [];
    const save = (el: HTMLElement): void => {
      saved.push([el, el.getAttribute('style') ?? '']);
    };
    const restore = (): void => {
      for (const [el, style] of saved.reverse()) {
        if (style) el.setAttribute('style', style);
        else el.removeAttribute('style');
        el.removeAttribute('data-ui-loop-full');
      }
    };
    (window as unknown as { __uiLoopRestore?: () => void }).__uiLoopRestore = restore;

    const dialogs = Array.from(document.querySelectorAll('[role="dialog"], [role="alertdialog"], dialog[open]')).filter(rendered);
    const dialog = dialogs[dialogs.length - 1];
    const pool = dialog
      ? deep(dialog)
      : deep(document.body).filter((el) => closestDeep(el, 'main, [role=main], [role=dialog]') || el.querySelector('main, [role=main]'));
    const scrollers = pool.filter((el) => el.scrollHeight > el.clientHeight + 40 && !closestDeep(el, 'nav, aside') && rendered(el) && getComputedStyle(el).overflowY !== 'visible');
    const wide = scrollers.filter((el) => el.getBoundingClientRect().width >= window.innerWidth / 2);
    const candidate = (wide.length ? wide : scrollers).sort((a, b) => b.scrollHeight - a.scrollHeight)[0];
    const doc = document.scrollingElement ?? document.documentElement;
    if (!(candidate instanceof HTMLElement)) {
      if (doc.scrollHeight <= window.innerHeight + 40) return null;
      // A full-page shot keeps fixed chrome at its viewport spot: hide what sits in the lower half.
      for (const el of Array.from(document.querySelectorAll<HTMLElement>('body *'))) {
        if (getComputedStyle(el).position !== 'fixed' || el.getBoundingClientRect().top < window.innerHeight / 2) continue;
        save(el);
        el.style.setProperty('visibility', 'hidden', 'important');
      }
      return { mode: 'document' as const, target: 'document', scrollHeight: doc.scrollHeight, clientHeight: window.innerHeight };
    }
    // Un-clipping changes a flex item's automatic min-width: pin the current width.
    const pin = (el: HTMLElement): void => {
      save(el);
      const width = `${el.getBoundingClientRect().width}px`;
      el.style.setProperty('box-sizing', 'border-box', 'important');
      for (const prop of ['width', 'min-width', 'max-width']) el.style.setProperty(prop, width, 'important');
    };
    pin(candidate);
    const { scrollHeight, clientHeight } = candidate;
    candidate.style.setProperty('height', `${Math.min(scrollHeight, cap)}px`, 'important');
    candidate.style.setProperty('max-height', 'none', 'important');
    candidate.style.setProperty('overflow', 'visible', 'important');
    for (let parent = parentOf(candidate); parent && parent !== document.documentElement; parent = parentOf(parent)) {
      if (getComputedStyle(parent).overflow !== 'visible' && parent instanceof HTMLElement) {
        pin(parent);
        parent.style.setProperty('overflow', 'visible', 'important');
      }
    }
    candidate.setAttribute('data-ui-loop-full', '');
    const tag = candidate.tagName.toLowerCase();
    const testId = candidate.getAttribute('data-testid');
    return {
      mode: 'element' as const,
      target: testId ? `${tag}[data-testid="${testId}"]` : `${tag}.${Array.from(candidate.classList).slice(0, 2).join('.')}`,
      scrollHeight,
      clientHeight,
    };
  }, FULL_SHOT_CAP);

  if (!target) return null;
  try {
    await twoFrames(page);
    if (target.mode === 'document') {
      const width = page.viewportSize()?.width ?? 0;
      await page.screenshot({
        path: file,
        fullPage: true,
        animations: 'disabled',
        caret: 'hide',
        timeout: 30_000,
        clip: { x: 0, y: 0, width, height: Math.min(target.scrollHeight, FULL_SHOT_CAP) },
      });
    } else {
      await page.locator('[data-ui-loop-full]').screenshot({ path: file, animations: 'disabled', caret: 'hide', timeout: 30_000 });
    }
  } finally {
    await page.evaluate(() => {
      const holder = window as unknown as { __uiLoopRestore?: () => void };
      holder.__uiLoopRestore?.();
      delete holder.__uiLoopRestore;
    });
  }
  return { target: target.target, scrollHeight: target.scrollHeight, clientHeight: target.clientHeight, capped: target.scrollHeight > FULL_SHOT_CAP };
}

// ── Glass probe: is the backdrop filter actually rendered? ────────────────────────────────────

const GLASS_PROBE_CAP = 2;
const GLASS_PROBE_INSET = 12;
const GLASS_PROBE_BANDS = 6;
/** A page edge: an unfiltered neighbour difference of at least this (0–255). */
const GLASS_EDGE = 24;
/** A band with fewer page edges than this is not judged. */
const GLASS_MIN_EDGES = 60;
/** A band fails when more than this share of its page edge detail survives the filter. */
const GLASS_BAND_MAX_RATIO = 0.2;

export interface GlassBlurHit {
  path: string;
  backdropFilter: string;
  worstRatio: number;
  bands: Array<number | null>;
}

/**
 * Every visible, unoccluded overlay or fixed painter with a backdrop filter is
 * shot twice the way the viewport shot was taken — made transparent with its
 * children hidden, once with its filter and once without — and the page detail
 * under it is compared per band. Detail that survives the filter means the page
 * reads sharp through the glass: the headless shell's software compositor does
 * exactly that, which is why the capture launches on the GPU path. This probe
 * keeps a regression loud.
 */
export async function probeGlassBlur(page: Page): Promise<GlassBlurHit[]> {
  const painters = await page.evaluate(
    ({ cap, inset }) => {
      const vw = window.innerWidth;
      const vh = window.innerHeight;
      const out: Array<{ index: number; path: string; backdropFilter: string; clip: { x: number; y: number; width: number; height: number } }> = [];
      const overlay = '[role=dialog], [role=alertdialog], dialog, [role=menu], [role=listbox], [popover], .cdk-overlay-pane';
      for (const el of Array.from(document.querySelectorAll<HTMLElement>('body *'))) {
        if (out.length >= cap) break;
        const style = getComputedStyle(el);
        if (!style.backdropFilter || style.backdropFilter === 'none' || style.visibility !== 'visible') continue;
        if (!el.matches(overlay) && !el.closest(overlay) && style.position !== 'fixed') continue;
        if (el.closest('[inert]')) continue;
        const r = el.getBoundingClientRect();
        const hit = document.elementFromPoint((r.left + r.right) / 2, (r.top + r.bottom) / 2);
        if (!hit || !el.contains(hit)) continue;
        const insetX = Math.max(inset, parseFloat(style.borderTopLeftRadius) || 0);
        const x = Math.max(0, r.left) + insetX;
        const y = Math.max(0, r.top) + inset;
        const width = Math.min(vw, r.right) - insetX - x;
        const height = Math.min(vh, r.bottom) - inset - y;
        if (width < 48 || height < 24) continue;
        const index = out.length;
        el.setAttribute('data-ui-loop-glass', String(index));
        const cls = Array.from(el.classList).slice(0, 2).join('.');
        out.push({ index, path: `${el.tagName.toLowerCase()}${cls ? `.${cls}` : ''}`, backdropFilter: style.backdropFilter, clip: { x, y, width, height } });
      }
      return out;
    },
    { cap: GLASS_PROBE_CAP, inset: GLASS_PROBE_INSET },
  );
  const hits: GlassBlurHit[] = [];
  try {
    for (const painter of painters) {
      const target = `[data-ui-loop-glass="${painter.index}"]`;
      const bare = await page.addStyleTag({
        content: `${target}{background-color:transparent!important;background-image:none!important;box-shadow:none!important}${target} *{visibility:hidden!important}`,
      });
      await twoFrames(page);
      const own = await page.screenshot({ animations: 'disabled', caret: 'hide', scale: 'device' });
      const off = await page.addStyleTag({ content: `${target}{backdrop-filter:none!important;-webkit-backdrop-filter:none!important}` });
      await twoFrames(page);
      const none = await page.screenshot({ animations: 'disabled', caret: 'hide', scale: 'device' });
      await off.evaluate((el) => (el as Element).remove());
      await bare.evaluate((el) => (el as Element).remove());
      const bands = await bandLeaks(page, own, none, painter.clip);
      const worstRatio = Math.max(0, ...bands.filter((b): b is number => b !== null));
      if (worstRatio > GLASS_BAND_MAX_RATIO) hits.push({ path: painter.path, backdropFilter: painter.backdropFilter, worstRatio, bands });
    }
  } finally {
    await page.evaluate(() => {
      for (const el of Array.from(document.querySelectorAll('[data-ui-loop-glass]'))) el.removeAttribute('data-ui-loop-glass');
    });
    await twoFrames(page);
  }
  return hits;
}

/**
 * Per band: the share of the unfiltered shot's edge detail that survives the filter
 * (decoded in the page; no Node PNG dependency). Pixels identical in both shots count
 * as fully leaked: a compositor that ignores the filter altogether leaves every pixel
 * unchanged, and skipping those made the probe blind to exactly that failure.
 */
async function bandLeaks(page: Page, own: Buffer, none: Buffer, rect: { x: number; y: number; width: number; height: number }): Promise<Array<number | null>> {
  return page.evaluate(
    async ({ ownB64, noneB64, rect, bands, edge, minEdges }) => {
      const decode = async (b64: string): Promise<ImageData> => {
        const bytes = Uint8Array.from(atob(b64), (c) => c.charCodeAt(0));
        const blob = new Blob([bytes], { type: 'image/png' });
        const whole = await createImageBitmap(blob);
        const scale = whole.width / window.innerWidth;
        whole.close();
        const [sx, sy, sw, sh] = [rect.x, rect.y, rect.width, rect.height].map((v) => Math.round(v * scale)) as [number, number, number, number];
        const bitmap = await createImageBitmap(blob, sx, sy, sw, sh);
        const canvas = new OffscreenCanvas(sw, sh);
        const ctx = canvas.getContext('2d');
        if (!ctx) throw new Error('glass probe: no 2d context');
        ctx.drawImage(bitmap, 0, 0);
        return ctx.getImageData(0, 0, sw, sh);
      };
      const [a, b] = await Promise.all([decode(ownB64), decode(noneB64)]);
      const { width, height } = b;
      const grad = (d: Uint8ClampedArray, i: number): number =>
        (Math.abs(d[i]! - d[i - 4]!) + Math.abs(d[i + 1]! - d[i - 3]!) + Math.abs(d[i + 2]! - d[i - 2]!)) / 3;
      const out: Array<number | null> = [];
      for (let band = 0; band < bands; band++) {
        let leaked = 0;
        let detail = 0;
        let edges = 0;
        for (let y = Math.floor((band * height) / bands); y < Math.floor(((band + 1) * height) / bands); y++) {
          for (let x = 1; x < width; x++) {
            const i = (y * width + x) * 4;
            const gb = grad(b.data, i);
            if (gb < edge) continue;
            leaked += grad(a.data, i);
            detail += gb;
            edges++;
          }
        }
        out.push(edges >= minEdges ? Math.round((leaked / detail) * 100) / 100 : null);
      }
      return out;
    },
    { ownB64: own.toString('base64'), noneB64: none.toString('base64'), rect, bands: GLASS_PROBE_BANDS, edge: GLASS_EDGE, minEdges: GLASS_MIN_EDGES },
  );
}

/** Folds the glass probe into the lint result as the `glass-blur` rule. */
export function addGlassBlur(result: LintResult, hits: readonly GlassBlurHit[], off: readonly string[], cap: number): void {
  const rule: LintRule = 'glass-blur';
  if (off.includes(rule) || !hits.length) return;
  result.rules[rule] = {
    count: hits.length,
    items: hits.slice(0, cap).map((h) => ({
      path: h.path,
      detail: `${Math.round(h.worstRatio * 100)}% of the page detail survives ${h.backdropFilter}`,
      rect: [0, 0, 0, 0],
    })),
  };
}

// ── Per-shot record ───────────────────────────────────────────────────────────────────────────

export type ShotStatus = 'ok' | 'recipe-failed' | 'theme-mismatch' | 'build-error' | 'unreachable' | 'error';

/** Statuses a `--resume` run keeps; anything else is retaken. */
export const FINAL_STATUSES: readonly ShotStatus[] = ['ok', 'unreachable'];

export const OVERLAY_KINDS: readonly ScreenKind[] = ['dialog', 'layer', 'drawer', 'sheet', 'popover'];

/**
 * One shot's record: `<passDir>/shots/<id>/<viewport>.<theme>.json`. Its field
 * names are a contract — vybava's split, publish and scoreboard read them
 * (internal/uiloop/record.go); change both or bump RECORD_VERSION.
 */
export interface ShotRecord {
  v: number;
  pass: number;
  order: number;
  id: string;
  app: string;
  area: string;
  kind: ScreenKind;
  state: ScreenState | null;
  title: string;
  /** The route template. */
  route: string;
  /** Where the shot ended up. */
  url: string | null;
  parentId: string | null;
  variantOf: string | null;
  as: string | null;
  viewport: string;
  size: { width: number; height: number };
  theme: Theme;
  themeActual: string | null;
  status: ShotStatus;
  failure: RecipeFailure | null;
  /** File names beside the record. */
  files: { viewport: string | null; full: string | null };
  full: FullShot | null;
  safeArea: SafeAreaState & { emulated: boolean | null };
  lint: { defects: Record<string, number>; info: Record<string, number>; result: LintResult } | null;
  consoleErrors: string[];
  settleNotes: string[];
  timings: Record<string, number>;
  sourceFiles: string[];
  knownIssues: string[];
  destructive: boolean;
  capturedAt: string;
}

export function lintSummary(result: LintResult): NonNullable<ShotRecord['lint']> {
  return { ...lintCounts(result), result };
}

export function writeRecord(record: ShotRecord, file: string): void {
  fs.mkdirSync(path.dirname(file), { recursive: true });
  const tmp = `${file}.tmp-${process.pid}`;
  fs.writeFileSync(tmp, `${JSON.stringify(record, null, 2)}\n`, 'utf-8');
  fs.renameSync(tmp, file);
}

export function readRecord(file: string): ShotRecord | null {
  if (!fs.existsSync(file)) return null;
  try {
    return JSON.parse(fs.readFileSync(file, 'utf-8')) as ShotRecord;
  } catch (error) {
    // A record cut off mid-write by a killed run is retaken.
    if (error instanceof SyntaxError) return null;
    throw error;
  }
}
