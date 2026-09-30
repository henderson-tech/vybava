// The in-page layout lint: what a screenshot cannot prove on its own. `lintPage`
// runs inside the page through page.evaluate, so it must stay self-contained —
// no imports, no closures over module scope (types are erased and are fine).
//
// Generic rules only; a repo tunes them with uiLoop.lint (grid, touchTarget,
// ramp, off) and project.ts `chrome`. Graduated from Reservine's ui-audit lint
// and voke's responsive audit.

import type { Insets } from './viewports';

/** Rule id → whether a hit is a defect (counted in totals) or information for the reviewer. */
export const LINT_RULES = {
  /** A box past the viewport's left/right edge that nothing clips; the document scrolls sideways. */
  'h-scroll': 'defect',
  /** A region that scrolls sideways on its own (tables, chip rows) — listed for judgement. */
  'h-scroller': 'info',
  /** Text cut off by overflow without an ellipsis or line clamp. */
  'text-clipped': 'defect',
  /** Text running out of its own box or its parent's (unbreakable strings, nowrap). */
  'text-spill': 'defect',
  /** Ellipsis / line-clamp truncation — intentional, listed for judgement. */
  truncated: 'info',
  /** Spacing, control heights, icon sizes and line heights off the grid (uiLoop.lint.grid, default 4). */
  grid: 'defect',
  /** A computed font size off the type ramp (only with uiLoop.lint.ramp). */
  'type-ramp': 'defect',
  /** Coarse-pointer viewports: a tap target under uiLoop.lint.touchTarget (default 44), hit-slop pseudo-elements counted. */
  'touch-target': 'defect',
  /** Content of fixed/sticky chrome (or page content) under an emulated safe-area inset. */
  'safe-area': 'defect',
  /** The same text twice or more inside one header, section, panel or dialog. */
  'repeated-text': 'info',
  /** Text below WCAG AA contrast (4.5, 3 for large text) against the background it paints on. */
  contrast: 'defect',
  /** A backdrop-filter on content: glass belongs to chrome and overlays only. */
  'glass-on-content': 'defect',
  /** A card-like surface (fill/border/shadow + radius) inside another with no overlay between: depth 1 only. */
  'nested-surface': 'defect',
  /** An overlay's glass lets the page read sharp through it (capture.ts pixel probe; the GPU path regressed). */
  'glass-blur': 'defect',
} as const;

export type LintRule = keyof typeof LINT_RULES;

export interface Finding {
  /** Tag · data-testid · first classes of the element and up to two ancestors. */
  path: string;
  text?: string;
  detail: string;
  /** x, y, width, height in CSS px. */
  rect: [number, number, number, number];
}

export interface RuleResult {
  count: number;
  items: Finding[];
  /** Per-value tallies (grid, type-ramp): `marginTop:6 → 12`. */
  byValue?: Record<string, number>;
  /**
   * Every distinct path + detail the rule hit on this shot, uncapped up to
   * 1000 per rule: what the pass-level unique counts fold, so one sidebar
   * defect seen on 38 screens counts once.
   */
  distinct?: LintKey[];
}

/** One defect's identity across a pass: the element path and the detail. */
export interface LintKey {
  path: string;
  detail: string;
}

export interface LintOptions {
  insets: Insets;
  mobile: boolean;
  grid: number;
  touchTarget: number;
  ramp: number[];
  off: string[];
  /**
   * uiLoop.lint.allow: rule id → selectors. A hit on an element that matches
   * one, or sits inside one, lands in `allowed` (counted as info), never in
   * `rules` — a spec that allows half steps inside primitive recipes only.
   */
  allow: Record<string, string[]>;
  /** project.ts `chrome`: selectors that paint their own background (sticky bars, docks). */
  chrome: string[];
  /** The screen is an overlay: content checks are scoped to the topmost open overlay. */
  overlayOnly: boolean;
  /** Most items kept per rule (counts are never capped). */
  cap: number;
}

export interface LintResult {
  viewport: { width: number; height: number };
  /** Pixels the document scrolls sideways — must be 0. */
  docOverflowX: number;
  /** Whether env(safe-area-inset-*) resolved to the emulated insets; null without insets. */
  insetsEmulated: boolean | null;
  /** The overlay content checks were scoped to, when one was. */
  scope: string | null;
  rules: Partial<Record<LintRule, RuleResult>>;
  /** Hits the allowlist moved out of `rules`: counted as info under the same rule id. */
  allowed: Partial<Record<LintRule, RuleResult>>;
}

/** Defect and info counts per rule — what a shot record and the scoreboard carry. */
export function lintCounts(result: LintResult | null): { defects: Record<string, number>; info: Record<string, number> } {
  const defects: Record<string, number> = {};
  const info: Record<string, number> = {};
  if (!result) return { defects, info };
  for (const [rule, res] of Object.entries(result.rules)) {
    if (!res || !res.count) continue;
    const kind = (LINT_RULES as Record<string, string>)[rule] ?? 'defect';
    (kind === 'info' ? info : defects)[rule] = res.count;
  }
  for (const [rule, res] of Object.entries(result.allowed)) {
    if (res && res.count) info[rule] = (info[rule] ?? 0) + res.count;
  }
  return { defects, info };
}

/** Defect rule → the distinct path + detail keys it hit on the shot (the record's `lint.distinct`). */
export function lintDistinct(result: LintResult | null): Record<string, LintKey[]> {
  const out: Record<string, LintKey[]> = {};
  if (!result) return out;
  for (const [rule, res] of Object.entries(result.rules)) {
    if (!res || !res.count || (LINT_RULES as Record<string, string>)[rule] === 'info') continue;
    // A rule folded in outside lintPage (glass-blur) carries its items only.
    out[rule] = res.distinct ?? res.items.map((f) => ({ path: f.path, detail: f.detail }));
  }
  return out;
}

export function lintPage(o: LintOptions): LintResult {
  const vw = document.documentElement.clientWidth;
  const vh = window.innerHeight;
  const on = (rule: string): boolean => !o.off.includes(rule);
  const rules: Partial<Record<string, RuleResult>> = {};
  const allowedRules: Partial<Record<string, RuleResult>> = {};
  const bucketIn = (table: Partial<Record<string, RuleResult>>, rule: string): RuleResult => {
    let r = table[rule];
    if (!r) {
      r = { count: 0, items: [] };
      table[rule] = r;
    }
    return r;
  };
  const bucket = (rule: string): RuleResult => bucketIn(rules, rule);
  // Distinct path + detail per defect rule, kept apart from the capped items.
  const DISTINCT_CAP = 1000;
  const seen = new Map<RuleResult, Set<string>>();
  const noteDistinct = (b: RuleResult, path: string, detail: string): void => {
    let keys = seen.get(b);
    if (!keys) {
      keys = new Set<string>();
      seen.set(b, keys);
    }
    const key = `${path}\n${detail}`;
    if (keys.has(key) || keys.size >= DISTINCT_CAP) return;
    keys.add(key);
    (b.distinct = b.distinct ?? []).push({ path, detail });
  };
  const allowSel: Record<string, string> = {};
  for (const [rule, selectors] of Object.entries(o.allow)) {
    for (const sel of selectors) {
      try {
        document.createDocumentFragment().querySelector(sel);
      } catch {
        throw new Error(`uiLoop.lint.allow.${rule}: ${JSON.stringify(sel)} is not a valid selector`);
      }
    }
    if (selectors.length) allowSel[rule] = selectors.join(', ');
  }

  // ── Element helpers ──────────────────────────────────────────────────────────────────────
  const styles = new Map<Element, CSSStyleDeclaration>();
  const cs = (el: Element): CSSStyleDeclaration => {
    let s = styles.get(el);
    if (!s) {
      s = getComputedStyle(el);
      styles.set(el, s);
    }
    return s;
  };
  const px = (raw: string): number => parseFloat(raw) || 0;
  const parentOf = (el: Element): Element | null => {
    if (el.parentElement) return el.parentElement;
    const root = el.getRootNode();
    return root instanceof ShadowRoot ? root.host : null;
  };
  const closestDeep = (el: Element, selector: string): Element | null => {
    for (let cur: Element | null = el; cur; cur = parentOf(cur)) if (cur.matches(selector)) return cur;
    return null;
  };
  const deepElements = (root: Element): Element[] => {
    const out: Element[] = [];
    const walk = (node: Element | ShadowRoot): void => {
      for (const el of Array.from(node.children)) {
        out.push(el);
        if (el.shadowRoot) walk(el.shadowRoot);
        walk(el);
      }
    };
    if (root.shadowRoot) walk(root.shadowRoot);
    walk(root);
    return out;
  };
  const describe = (el: Element): string => {
    const parts: string[] = [];
    let e: Element | null = el;
    for (let i = 0; i < 3 && e && e !== document.body; i++) {
      const h = e as HTMLElement;
      let s = e.tagName.toLowerCase();
      const testId = h.getAttribute('data-testid');
      if (testId) s += `[data-testid=${testId}]`;
      const cls = Array.from(e.classList).slice(0, 4).join('.');
      if (cls) s += `.${cls}`;
      parts.unshift(s);
      if (testId) break;
      e = parentOf(e);
    }
    return parts.join(' > ');
  };
  const ownText = (el: Element): string => {
    let s = '';
    for (const n of Array.from(el.childNodes)) if (n.nodeType === Node.TEXT_NODE) s += n.textContent ?? '';
    return s.replace(/\s+/g, ' ').trim();
  };
  const rectOf = (r: DOMRect): Finding['rect'] => [Math.round(r.x), Math.round(r.y), Math.round(r.width), Math.round(r.height)];
  const finding = (el: Element, r: DOMRect, detail: string): Finding => {
    const text = (ownText(el) || (el as HTMLElement).innerText || '').replace(/\s+/g, ' ').slice(0, 80);
    return text ? { path: describe(el), text, detail, rect: rectOf(r) } : { path: describe(el), detail, rect: rectOf(r) };
  };
  // An allowlisted element's hit is counted in `allowed` (info), never as a defect.
  const target = (rule: string, el: Element): RuleResult => {
    const sel = allowSel[rule];
    return sel && closestDeep(el, sel) ? bucketIn(allowedRules, rule) : bucket(rule);
  };
  const hit = (rule: string, el: Element, r: DOMRect, detail: string): void => {
    if (!on(rule)) return;
    const b = target(rule, el);
    b.count++;
    if (b.items.length < o.cap) b.items.push(finding(el, r, detail));
    if (b === rules[rule]) noteDistinct(b, describe(el), detail);
  };
  const tally = (rule: string, el: Element, key: string): void => {
    if (!on(rule)) return;
    const b = target(rule, el);
    b.count++;
    if (b === rules[rule]) noteDistinct(b, describe(el), key);
    b.byValue = b.byValue ?? {};
    b.byValue[key] = (b.byValue[key] ?? 0) + 1;
    if (b.items.length < o.cap && !b.items.some((x) => x.detail === key && x.path === describe(el)))
      b.items.push(finding(el, el.getBoundingClientRect(), key));
  };

  const OVERLAY =
    '[role=dialog], [role=alertdialog], dialog, [role=menu], [role=listbox], [role=tooltip], [popover], .cdk-overlay-pane';
  const chromeSel = o.chrome.join(', ');
  const isChrome = (el: Element): boolean => !!chromeSel && closestDeep(el, chromeSel) !== null;
  const hidden = (el: Element, s: CSSStyleDeclaration): boolean =>
    s.display === 'none' || s.visibility === 'hidden' || s.opacity === '0' || closestDeep(el, '[aria-hidden=true], [inert]') !== null;

  // The overlay a dialog/sheet/popover screen is judged on: the last rendered one.
  let scope: Element = document.body;
  let scopeName: string | null = null;
  if (o.overlayOnly) {
    const open = deepElements(document.body).filter((el) => {
      if (!el.matches(OVERLAY)) return false;
      const r = el.getBoundingClientRect();
      const s = cs(el);
      return r.width > 0 && r.height > 0 && s.visibility !== 'hidden' && s.display !== 'none';
    });
    const top = open[open.length - 1];
    if (top) {
      scope = top;
      scopeName = describe(top);
    }
  }
  const elements = deepElements(scope);

  // ── Colours: any CSS colour syntax, read back through a 1×1 canvas ─────────────────────────
  type Rgba = [number, number, number, number];
  const colorCache = new Map<string, Rgba | null>();
  const canvas = document.createElement('canvas');
  canvas.width = 1;
  canvas.height = 1;
  const ctx2d = canvas.getContext('2d', { willReadFrequently: true });
  const parseColor = (value: string): Rgba | null => {
    if (!value || value === 'transparent' || value === 'rgba(0, 0, 0, 0)') return [0, 0, 0, 0];
    const cached = colorCache.get(value);
    if (cached !== undefined) return cached;
    let rgba: Rgba | null = null;
    if (ctx2d) {
      ctx2d.clearRect(0, 0, 1, 1);
      ctx2d.fillStyle = '#000';
      ctx2d.fillStyle = value;
      ctx2d.fillRect(0, 0, 1, 1);
      const d = ctx2d.getImageData(0, 0, 1, 1).data;
      rgba = [d[0]!, d[1]!, d[2]!, d[3]! / 255];
    }
    colorCache.set(value, rgba);
    return rgba;
  };
  const over = (top: Rgba, bottom: Rgba): Rgba => {
    const a = top[3] + bottom[3] * (1 - top[3]);
    if (a === 0) return [0, 0, 0, 0];
    const mix = (i: number): number => (top[i]! * top[3] + bottom[i]! * bottom[3] * (1 - top[3])) / a;
    return [mix(0), mix(1), mix(2), a];
  };
  const channel = (v: number): number => {
    const c = v / 255;
    return c <= 0.03928 ? c / 12.92 : Math.pow((c + 0.055) / 1.055, 2.4);
  };
  const luminance = (c: Rgba): number => 0.2126 * channel(c[0]) + 0.7152 * channel(c[1]) + 0.0722 * channel(c[2]);
  const contrast = (a: Rgba, b: Rgba): number => {
    const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x) as [number, number];
    return (hi + 0.05) / (lo + 0.05);
  };
  const canvasColor = (): Rgba => {
    const scheme = cs(document.documentElement).colorScheme;
    return /dark/.test(scheme) && !/light/.test(scheme) ? [18, 18, 18, 1] : [255, 255, 255, 1];
  };
  /** The colour painted behind el (its own fill included), or null when an image, gradient or glass is in the way. */
  const bgCache = new Map<Element, Rgba | null>();
  const backgroundOf = (el: Element): Rgba | null => {
    if (bgCache.has(el)) return bgCache.get(el)!;
    const s = cs(el);
    let res: Rgba | null;
    if (s.backgroundImage !== 'none' || (s.backdropFilter && s.backdropFilter !== 'none')) res = null;
    else {
      const own = parseColor(s.backgroundColor);
      if (!own) res = null;
      else if (own[3] >= 0.99) res = own;
      else {
        const parent = parentOf(el);
        const below = parent ? backgroundOf(parent) : canvasColor();
        res = below ? over(own, below) : null;
      }
    }
    bgCache.set(el, res);
    return res;
  };
  const opacityOf = (el: Element): number => {
    let op = 1;
    for (let cur: Element | null = el; cur; cur = parentOf(cur)) {
      const v = parseFloat(cs(cur).opacity);
      op *= Number.isFinite(v) ? v : 1;
    }
    return op;
  };

  // ── Geometry helpers ─────────────────────────────────────────────────────────────────────
  const onGrid = (v: number): boolean => Math.abs(v / o.grid - Math.round(v / o.grid)) <= 0.02;
  const insetValues = [o.insets.top, o.insets.right, o.insets.bottom, o.insets.left].filter((i) => i > 0);
  // A box padded by a safe-area inset measures inset + grid steps: the inset is the device's.
  const offGrid = (v: number): boolean => v > 0.05 && !onGrid(v) && !insetValues.some((i) => onGrid(Math.abs(v - i)));
  const grid = (el: Element, prop: string, raw: string): void => {
    const v = Math.abs(parseFloat(raw));
    if (!Number.isFinite(v) || !offGrid(v)) return;
    tally('grid', el, `${prop}:${Math.round(v * 100) / 100}`);
  };
  const autoMargin = (el: Element, prop: string): boolean => {
    const map = (el as Element & { computedStyleMap?: () => { get(p: string): { toString(): string } | undefined } }).computedStyleMap?.();
    return map?.get(prop)?.toString() === 'auto';
  };
  const viewportSized = (v: number): boolean => Math.abs(v - window.innerWidth) < 0.5 || Math.abs(v - window.innerHeight) < 0.5;
  const scaledCache = new Map<Element, boolean>();
  const scaled = (el: Element | null): boolean => {
    if (!el || el === document.body) return false;
    const known = scaledCache.get(el);
    if (known !== undefined) return known;
    const tf = cs(el).transform;
    let own = false;
    if (tf && tf !== 'none') {
      const m = new DOMMatrixReadOnly(tf);
      own = Math.abs(Math.hypot(m.a, m.b) - 1) > 0.001 || Math.abs(Math.hypot(m.c, m.d) - 1) > 0.001;
    }
    const res = own || scaled(parentOf(el));
    scaledCache.set(el, res);
    return res;
  };
  /** The part of a rect no clipping ancestor hides; null when nothing is left. */
  const visibleRect = (el: Element): DOMRect | null => {
    const r = el.getBoundingClientRect();
    let l = r.left;
    let t = r.top;
    let rt = r.right;
    let b = r.bottom;
    for (let a = parentOf(el); a && a !== document.body; a = parentOf(a)) {
      const s = cs(a);
      if ((s.overflowX === 'visible' && s.overflowY === 'visible') || s.display === 'inline' || s.display === 'contents') continue;
      const ar = a.getBoundingClientRect();
      const al = ar.left + a.clientLeft;
      const at = ar.top + a.clientTop;
      l = Math.max(l, al);
      t = Math.max(t, at);
      rt = Math.min(rt, al + a.clientWidth);
      b = Math.min(b, at + a.clientHeight);
      if (rt - l <= 0 || b - t <= 0) return null;
    }
    return new DOMRect(l, t, rt - l, b - t);
  };
  const insideClip = (el: Element): boolean => {
    for (let a = parentOf(el); a && a !== document.body; a = parentOf(a)) {
      if (cs(a).overflowX !== 'visible') {
        const ar = a.getBoundingClientRect();
        return ar.right <= vw + 1 && ar.left >= -1;
      }
    }
    return false;
  };
  const slop = (el: Element): { w: number; h: number } => {
    let w = 0;
    let h = 0;
    for (const p of ['::before', '::after'] as const) {
      const ps = getComputedStyle(el, p);
      if (ps.content === 'none' || ps.content === 'normal' || ps.display === 'none') continue;
      if (ps.position !== 'absolute' && ps.position !== 'fixed') continue;
      const size = (v: string): number => Math.max(px(v), ...Array.from(v.matchAll(/(\d+(?:\.\d+)?)px/g), (m) => Number(m[1])));
      w = Math.max(w, size(ps.width));
      h = Math.max(h, size(ps.height));
    }
    return { w, h };
  };

  // ── Safe-area emulation proof ────────────────────────────────────────────────────────────
  let insetsEmulated: boolean | null = null;
  if (insetValues.length) {
    const probe = document.createElement('div');
    probe.style.cssText =
      'position:fixed;visibility:hidden;padding:env(safe-area-inset-top,0px) env(safe-area-inset-right,0px) env(safe-area-inset-bottom,0px) env(safe-area-inset-left,0px)';
    document.body.append(probe);
    const ps = getComputedStyle(probe);
    insetsEmulated =
      Math.abs(px(ps.paddingTop) - o.insets.top) < 0.5 &&
      Math.abs(px(ps.paddingRight) - o.insets.right) < 0.5 &&
      Math.abs(px(ps.paddingBottom) - o.insets.bottom) < 0.5 &&
      Math.abs(px(ps.paddingLeft) - o.insets.left) < 0.5;
    probe.remove();
  }

  const se = document.scrollingElement ?? document.documentElement;
  const docOverflowX = Math.max(0, se.scrollWidth - se.clientWidth);

  // ── Per-element rules ────────────────────────────────────────────────────────────────────
  const CONTROL =
    'button, input, select, textarea, [role=tab], [role=button], [role=switch], [role=checkbox], [role=radio], [role=combobox]';
  const TARGET =
    'button, a[href], input:not([type=hidden]), select, textarea, summary, [role=button], [role=link], [role=tab], [role=radio], [role=checkbox], [role=switch], [role=menuitem], [role=menuitemcheckbox], [role=menuitemradio], [role=option], [role=slider]';
  const TEXT_FIELD =
    'textarea, input:not([type]), input:is([type=text], [type=search], [type=email], [type=password], [type=number], [type=tel], [type=url], [type=date], [type=time])';
  const REGION = 'header, section, aside, [role=dialog], [role=alertdialog], [role=region]';
  const LISTISH =
    'label, table, [role=table], [role=grid], [role=row], ul, ol, [role=list], [role=listbox], [role=menu], [role=tablist], [role=radiogroup], [role=tooltip], select';
  const offenders = new Set<Element>();
  const spilled = new Set<Element>();
  const targetSeen = new Set<Element>();
  const texts = new Map<Element, Map<string, Element[]>>();
  const surfaces = new Set<Element>();
  const textRange = document.createRange();

  const pastParent = (el: Element, s: CSSStyleDeclaration): number => {
    if (s.position === 'absolute' || s.position === 'fixed' || el instanceof SVGElement || scaled(el)) return 0;
    for (let a = parentOf(el); a; a = parentOf(a)) if (spilled.has(a)) return 0;
    let host = parentOf(el);
    while (host && host !== document.body && /^(inline|contents)$/.test(cs(host).display)) host = parentOf(host);
    if (!host || host === document.body) return 0;
    const hs = cs(host);
    if (hs.overflowX !== 'visible') return 0;
    let left = Infinity;
    let right = -Infinity;
    for (const n of Array.from(el.childNodes)) {
      if (n.nodeType !== Node.TEXT_NODE || !n.textContent?.trim()) continue;
      textRange.selectNodeContents(n);
      const tr = textRange.getBoundingClientRect();
      if (tr.width === 0) continue;
      left = Math.min(left, tr.left);
      right = Math.max(right, tr.right);
    }
    if (right === -Infinity) return 0;
    const hr = host.getBoundingClientRect();
    const boxLeft = hr.left + px(hs.borderLeftWidth) + px(hs.paddingLeft);
    const boxRight = hr.right - px(hs.borderRightWidth) - px(hs.paddingRight);
    const past = Math.max(right - boxRight, boxLeft - left);
    return past > 1 ? past : 0;
  };

  const surfaceBoundary = (el: Element): boolean => el.matches(OVERLAY) || (!!chromeSel && el.matches(chromeSel)) || cs(el).position === 'fixed';
  const isSurface = (el: Element, s: CSSStyleDeclaration, r: DOMRect): boolean => {
    if (r.width < 48 || r.height < 40 || el.matches(CONTROL) || el.matches('a, label, img, svg, video, canvas, iframe')) return false;
    const radius = Math.max(px(s.borderTopLeftRadius), px(s.borderTopRightRadius), px(s.borderBottomLeftRadius), px(s.borderBottomRightRadius));
    if (radius <= 0) return false;
    const sides = ['Top', 'Right', 'Bottom', 'Left'] as const;
    const bordered = sides.every((side) => {
      const width = px(s[`border${side}Width` as const]);
      const color = parseColor(s[`border${side}Color` as const]);
      return width >= 0.5 && s[`border${side}Style` as const] !== 'none' && !!color && color[3] > 0.05;
    });
    const shadowed = s.boxShadow !== 'none' && !/inset/.test(s.boxShadow);
    let filled = false;
    const own = parseColor(s.backgroundColor);
    if (own && own[3] > 0.02) {
      const parent = parentOf(el);
      const behind = parent ? backgroundOf(parent) : canvasColor();
      filled = !behind || contrast(over(own, behind), behind) > 1.02;
    }
    return bordered || shadowed || filled;
  };

  for (const el of elements) {
    const s = cs(el);
    if (hidden(el, s)) continue;
    const r = el.getBoundingClientRect();
    if (r.width <= 1 && r.height <= 1) continue;
    const inline = s.display === 'inline';
    const text = ownText(el);
    const onScreen = r.bottom > 0 && r.top < vh && r.right > 0 && r.left < vw;
    const srOnly =
      closestDeep(el, '.sr-only, .cdk-visually-hidden, .visually-hidden') !== null ||
      r.width <= 1 ||
      r.height <= 1 ||
      s.clip === 'rect(0px, 0px, 0px, 0px)' ||
      s.clipPath === 'inset(50%)';

    // h-scroll: sideways overflow of the page (whole document, whatever the scope)
    if ((r.right > vw + 1 || r.left < -1) && !insideClip(el) && s.position !== 'fixed') {
      const parent = parentOf(el);
      if (!parent || !offenders.has(parent)) hit('h-scroll', el, r, `x ${Math.round(r.left)}…${Math.round(r.right)} of ${vw}`);
      offenders.add(el);
    }
    // h-scroller: contained sideways scroll
    if ((s.overflowX === 'auto' || s.overflowX === 'scroll') && el.scrollWidth > el.clientWidth + 1)
      hit('h-scroller', el, r, `scrolls ${el.scrollWidth - el.clientWidth}px sideways`);

    // text-clipped / text-spill / truncated
    if (text && !inline && !srOnly) {
      const clampedX = s.overflowX === 'hidden' || s.overflowX === 'clip';
      const clampedY = s.overflowY === 'hidden' || s.overflowY === 'clip';
      const lineClamp = s.getPropertyValue('-webkit-line-clamp');
      const sl = slop(el);
      const widerX = el.scrollWidth > el.clientWidth + 1 + Math.max(0, (sl.w - el.clientWidth) / 2);
      const tallerY = el.scrollHeight > el.clientHeight + 1 + Math.max(0, (sl.h - el.clientHeight) / 2);
      if (clampedX && widerX) {
        if (s.textOverflow === 'ellipsis') hit('truncated', el, r, 'ellipsis');
        else hit('text-clipped', el, r, `clipped ${el.scrollWidth - el.clientWidth}px wide`);
      } else if (clampedY && tallerY && el.clientHeight > 0) {
        if (lineClamp && lineClamp !== 'none') hit('truncated', el, r, `line-clamp ${lineClamp}`);
        else hit('text-clipped', el, r, `clipped ${el.scrollHeight - el.clientHeight}px tall`);
      } else if (s.overflowX === 'visible' && widerX) {
        hit('text-spill', el, r, `text runs ${el.scrollWidth - el.clientWidth}px past its box`);
        spilled.add(el);
      } else if (s.overflowX === 'visible') {
        const past = pastParent(el, s);
        if (past > 0) {
          hit('text-spill', el, r, `text runs ${Math.round(past)}px past its parent's box`);
          spilled.add(el);
        }
      }
    }

    if (!srOnly && onScreen && !scaled(el)) {
      // grid — only what is on screen, so the tally matches the shot
      const inset = (side: 'Top' | 'Right' | 'Bottom' | 'Left'): void => {
        const pad = px(s[`padding${side}` as const]);
        const border = px(s[`border${side}Width` as const]);
        // Bordered boxes compensate their border: the inset is padding + border, and a
        // fixed-height control whose padding alone is on the grid is forgiven.
        if (border > 0 && !offGrid(pad)) return;
        grid(el, `inset${side}`, `${pad + border}`);
      };
      // A block padding its own negative margin grows a touch target, not the layout.
      const growY = px(s.paddingTop) > 0 && px(s.paddingTop) === -px(s.marginTop) && px(s.paddingBottom) === -px(s.marginBottom);
      for (const side of ['Top', 'Right', 'Bottom', 'Left'] as const) if (!growY || side === 'Right' || side === 'Left') inset(side);
      if (!growY && !autoMargin(el, 'margin-top')) grid(el, 'marginTop', s.marginTop);
      if (!growY && !autoMargin(el, 'margin-bottom')) grid(el, 'marginBottom', s.marginBottom);
      if (s.marginLeft !== s.marginRight) {
        if (!autoMargin(el, 'margin-left')) grid(el, 'marginLeft', s.marginLeft);
        if (!autoMargin(el, 'margin-right')) grid(el, 'marginRight', s.marginRight);
      }
      if (s.display.includes('flex') || s.display.includes('grid')) {
        grid(el, 'rowGap', s.rowGap);
        grid(el, 'columnGap', s.columnGap);
      }
      if (el.matches(CONTROL) && !viewportSized(r.height) && !el.matches('input[type=checkbox], input[type=radio]')) grid(el, 'height', `${r.height}`);
      if (el.tagName.toLowerCase() === 'svg' && !(el.getAttribute('width') ?? '').endsWith('%') && !(parentOf(el) instanceof SVGElement)) {
        if (!viewportSized(r.width)) grid(el, 'iconW', `${r.width}`);
        if (!viewportSized(r.height)) grid(el, 'iconH', `${r.height}`);
      }
      if (text && s.lineHeight.endsWith('px')) grid(el, 'lineHeight', s.lineHeight);

      if (text && o.ramp.length) {
        const fs = px(s.fontSize);
        if (fs && !o.ramp.some((step) => Math.abs(fs - step) < 0.05)) tally('type-ramp', el, `fontSize:${Math.round(fs * 100) / 100}`);
      }

      // repeated-text: one readout per attribute per region
      if (text && text.length >= 3 && /\p{L}/u.test(text)) {
        const region = el.closest(REGION);
        const list = el.closest(LISTISH);
        if (region && !(list && region.contains(list))) {
          const byText = texts.get(region) ?? new Map<string, Element[]>();
          texts.set(region, byText);
          byText.set(text, [...(byText.get(text) ?? []), el]);
        }
      }

      // contrast (disabled controls are exempt, as WCAG says)
      if (text && !closestDeep(el, ':disabled, [aria-disabled=true]') && s.backgroundClip !== 'text' && s.getPropertyValue('-webkit-background-clip') !== 'text') {
        const fg = parseColor(s.color);
        const bg = backgroundOf(el);
        if (fg && bg && fg[3] > 0) {
          const alpha = fg[3] * opacityOf(el);
          if (alpha > 0.05) {
            const painted = over([fg[0], fg[1], fg[2], alpha], bg);
            const ratio = contrast(painted, bg);
            const size = px(s.fontSize);
            const large = size >= 24 || (size >= 18.66 && (parseInt(s.fontWeight, 10) || 400) >= 700);
            const need = large ? 3 : 4.5;
            if (ratio < need) hit('contrast', el, r, `${Math.round(ratio * 100) / 100}:1 < ${need}:1`);
          }
        }
      }

      if (isSurface(el, s, r)) surfaces.add(el);
    }

    // touch-target (coarse pointer only)
    if (o.mobile && el.matches(TARGET) && !el.matches(TEXT_FIELD)) {
      const box = el.closest('label') ?? el;
      if (!targetSeen.has(box)) {
        targetSeen.add(box);
        const skip =
          s.pointerEvents === 'none' ||
          el.matches(':disabled, [aria-disabled=true]') ||
          (el.tagName === 'A' && inline) ||
          (!!el.parentElement?.closest(TARGET) && !el.matches('input, select, textarea'));
        const vr = skip ? null : visibleRect(box);
        if (vr && vr.bottom > 0 && vr.top < vh && vr.right > 0 && vr.left < vw) {
          const br = box.getBoundingClientRect();
          const clips = (e: Element): boolean => cs(e).overflowX !== 'visible' || cs(e).overflowY !== 'visible';
          const sa = clips(el) ? { w: 0, h: 0 } : slop(el);
          const sb = box === el || clips(box) ? { w: 0, h: 0 } : slop(box);
          const w = Math.max(br.width, sa.w, sb.w);
          const h = Math.max(br.height, sa.h, sb.h);
          const min = o.touchTarget - 0.5;
          if (w < min || h < min) hit('touch-target', box, br, `${Math.round(w)}×${Math.round(h)} target`);
        }
      }
    }

    // safe-area: fixed/sticky chrome must keep its content out of the insets
    if (insetValues.length && (s.position === 'fixed' || s.position === 'sticky' || (chromeSel && el.matches(chromeSel))) && r.width > 0) {
      for (const c of [el, ...Array.from(el.querySelectorAll('*'))]) {
        if (!(ownText(c) || c.matches('button, a, input, svg'))) continue;
        const cr = visibleRect(c);
        if (!cr || cr.width === 0 || cr.height === 0) continue;
        const i = o.insets;
        const under =
          i.top > 0 && cr.top < i.top && cr.bottom > 0
            ? 'top'
            : i.bottom > 0 && cr.bottom > vh - i.bottom && cr.top < vh
              ? 'bottom'
              : i.left > 0 && cr.left < i.left && cr.right > 0
                ? 'left'
                : i.right > 0 && cr.right > vw - i.right && cr.left < vw
                  ? 'right'
                  : null;
        if (under) {
          hit('safe-area', c, cr, `under the ${under} inset (${describe(el)})`);
          break;
        }
      }
    }

    // glass-on-content: glass belongs to chrome and overlays
    const glass = s.backdropFilter && s.backdropFilter !== 'none';
    if (glass && !closestDeep(el, OVERLAY) && !isChrome(el) && s.position !== 'fixed' && s.position !== 'sticky') {
      let pinned = false;
      for (let a = parentOf(el); a && !pinned; a = parentOf(a)) pinned = /^(fixed|sticky)$/.test(cs(a).position);
      if (!pinned) hit('glass-on-content', el, r, `backdrop-filter ${s.backdropFilter} on content`);
    }
  }

  // side insets hold for the page content too, not only the chrome
  if (o.insets.left > 0 || o.insets.right > 0) {
    for (const el of Array.from(document.querySelectorAll('main h1, main h2, main p, main button, main a, main td, main li'))) {
      if (el.closest('[aria-hidden=true], [inert]') || cs(el).visibility === 'hidden') continue;
      const r = visibleRect(el);
      if (!r || r.width === 0 || r.bottom < 0 || r.top > vh) continue;
      if (r.left < o.insets.left - 0.5 || r.right > vw - o.insets.right + 0.5) hit('safe-area', el, r, 'page content under a side inset');
    }
  }

  for (const [region, byText] of texts) {
    for (const [text, els] of byText) {
      if (els.length < 2 || !on('repeated-text')) continue;
      const b = bucket('repeated-text');
      b.count++;
      if (b.items.length < o.cap)
        b.items.push({
          path: describe(region),
          text: text.slice(0, 80),
          detail: `×${els.length} in one ${region.tagName.toLowerCase()}`,
          rect: rectOf(region.getBoundingClientRect()),
        });
    }
  }

  for (const el of surfaces) {
    for (let a = parentOf(el); a && a !== scope; a = parentOf(a)) {
      if (surfaceBoundary(a)) break;
      if (surfaces.has(a)) {
        hit('nested-surface', el, el.getBoundingClientRect(), `surface inside ${describe(a)}`);
        break;
      }
    }
  }

  if (docOverflowX > 0 && on('h-scroll') && !rules['h-scroll']) {
    const b = bucket('h-scroll');
    b.count++;
    b.items.push({ path: 'document', detail: `document scrolls ${docOverflowX}px sideways`, rect: [0, 0, vw, vh] });
    noteDistinct(b, 'document', 'document scrolls sideways');
  }

  return {
    viewport: { width: vw, height: vh },
    docOverflowX,
    insetsEmulated,
    scope: scopeName,
    rules: rules as LintResult['rules'],
    allowed: allowedRules as LintResult['allowed'],
  };
}
