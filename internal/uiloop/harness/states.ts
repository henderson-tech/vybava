// State recipes: how a `kind: 'state'` screen reaches empty / error / loading
// without touching seed data. Each helper is a Recipe for a screen's `before`
// (installed on the fresh page, before navigation). Generalized from voke's
// responsive manifest.
//
//   before: emptyList(/\/api\/tasks\?/)                 // the list answers an empty page
//   ...listStates(tasksScreen, /\/api\/tasks\?/)         // its empty · error · loading children

import type { Page, Route } from '@playwright/test';

import type { Recipe, Screen, ScreenState } from './manifest';

export type Json = null | boolean | number | string | Json[] | { [key: string]: Json };

/** A request matcher: a Playwright glob, a RegExp, or a predicate over the URL. */
export type UrlMatch = string | RegExp | ((url: URL) => boolean);

/**
 * Every list emptied and every count zeroed, the server's shape kept — so the
 * page's own parsing still accepts it. `nextCursor`-like keys become null,
 * `has*` booleans false.
 */
export function emptied(value: Json): Json {
  if (Array.isArray(value)) return [];
  if (typeof value === 'number') return 0;
  if (value === null || typeof value !== 'object') return value;
  return Object.fromEntries(
    Object.entries(value).map(([key, v]) => [
      key,
      /cursor$/i.test(key) ? null : typeof v === 'boolean' && /^has[A-Z]/.test(key) ? false : emptied(v),
    ]),
  );
}

/** Routes the matching GETs to `handle`; every other method falls through. */
export function onGet(url: UrlMatch, handle: (route: Route, page: Page) => Promise<void>): Recipe {
  return async (page) => {
    await page.route(url, (route) =>
      route.request().method() === 'GET' ? handle(route, page) : route.fallback(),
    );
  };
}

/** Rewrites the real GET's JSON (a passthrough when the server itself failed). */
export function patchJson(url: UrlMatch, patch: (body: Json) => Json): Recipe {
  return onGet(url, async (route) => {
    const response = await route.fetch();
    if (!response.ok()) {
      await route.fulfill({ response });
      return;
    }
    await route.fulfill({ response, json: patch((await response.json()) as Json) });
  });
}

/** empty: the list answers an empty page of its own shape. */
export function emptyList(url: UrlMatch): Recipe {
  return patchJson(url, emptied);
}

/** error: the list answers `status` with `body` (default: a 500 with a generic error body). */
export function failingList(url: UrlMatch, status = 500, body: Json = { message: 'Internal Server Error' }): Recipe {
  return onGet(url, (route) => route.fulfill({ status, json: body }));
}

/** loading: the list request is held until the page closes, so the shot catches the skeleton. */
export function heldList(url: UrlMatch): Recipe {
  return onGet(
    url,
    () =>
      new Promise<void>(() => {
        // Never settles: the page closes with the request still pending.
      }),
  );
}

/**
 * Writes to `url` (PUT/PATCH/POST) are echoed back instead of stored: a
 * preference the recipe toggles must not bleed into the next shot when many
 * workers share one account — and the seed stays as it was.
 */
export function echoWrites(url: UrlMatch, methods: readonly string[] = ['PUT', 'PATCH']): Recipe {
  return async (page) => {
    await page.route(url, (route) => {
      const request = route.request();
      if (!methods.includes(request.method())) return route.fallback();
      let body: Json = null;
      try {
        body = request.postDataJSON() as Json;
      } catch (error) {
        if (!(error instanceof SyntaxError)) throw error;
      }
      return route.fulfill({ json: body });
    });
  };
}

/** Runs recipes in order — a state mock plus the screen's own routing. */
export function all(...recipes: readonly (Recipe | undefined)[]): Recipe {
  return async (page, ctx) => {
    for (const recipe of recipes) if (recipe) await recipe(page, ctx);
  };
}

const LIST_STATE_MOCKS = {
  empty: emptyList,
  error: (url: UrlMatch) => failingList(url),
  loading: heldList,
} as const satisfies Partial<Record<ScreenState, (url: UrlMatch) => Recipe>>;

export type ListState = keyof typeof LIST_STATE_MOCKS;

/**
 * The empty · error · loading children of one list screen, reached with the
 * mocks above: ids `<base>-<state>`, parent = the base screen, no full shot
 * (a held request never ends, an empty list has nothing below the fold).
 */
export function listStates(
  base: Screen,
  url: UrlMatch,
  states: readonly ListState[] = ['empty', 'error', 'loading'],
): Screen[] {
  return states.map((state) => {
    const child: Screen = {
      ...base,
      id: `${base.id}-${state}`,
      kind: 'state',
      state,
      title: `${base.title} · ${state}`,
      parentId: base.id,
      full: false,
      before: all(base.before, LIST_STATE_MOCKS[state](url)),
    };
    // A state keeps the base's drive into its overlay (`open`) but never its
    // `ready`: the content it waits for is exactly what the mock withholds.
    delete child.ready;
    delete child.knownIssues;
    delete child.variantOf;
    delete child.destructive;
    return child;
  });
}
