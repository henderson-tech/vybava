# Targets - lanes, tiers and deep passes per target

`polish-kit plan` picks the targets; this file says what each one runs. The
adverse-condition rows are in `matrix.md`, numbered by tier.

## Inference

In order; a later step widens, never narrows:

1. **The branch diff.** `git diff --name-only <base>...HEAD` mapped through
   the repo's `polish.targets` globs; a diff spanning two targets runs both,
   heaviest first.
2. **The findings' origin.** A board of device shots is `app`, a browser
   board is `ui`, review comments on controllers or migrations are `api`.
3. **The cwd.** Inside one target's root it goes first even on a mixed branch.
4. **The repo.** A repo declaring one target needs no inference.

State the verdict in one line before binding. An explicit word from the
user overrides it.

## `app` - native apps

| | |
|---|---|
| Lanes | one per OS tier the repo ships to: the native-glass tier (newest iOS simulator or the paired phone), the fallback tier (the oldest supported iOS runtime, simulator or phone), Android (a USB device or an AVD) in gesture AND 3-button navigation |
| Default matrix | tiers 1 (client network, blip on the API leg), 2 (app lifecycle), 4 (input and UI pitfalls) |
| `full` adds | `app-chrome.md`: every touched screen × lane × theme × the default locale over `polish-kit shoot`, with `polish-kit sheet` for the side-by-sides and edge crops; recordings for §5 |
| Evidence | one board per pass; each shot names the device and OS version |

Booting a lane is the repo's job (its sim or device command); `polish-kit
lanes` says which lane is missing and prints the create command.

## `ui` - web

| | |
|---|---|
| Lanes | the Onyx browser at the desktop viewport (1440×810) and one phone viewport, light and dark |
| Default matrix | tiers 1 (blip on the API leg, when the API base is configurable), 3 (browser rows), 4 |
| `full` adds | the `ui-loop` pass over the touched screens: `ui-loop run --only <ids> --viewports … --themes light,dark`, its lint (grid, touch targets, type ramp), `ui-loop publish`; findings are worked like any other |
| Evidence | the ui-loop board plus the matrix report |

`ui-loop` needs the repo's `uiLoop` config and manifest; without them `full`
degrades to a hand capture of the touched screens per viewport and theme,
and the hand-off names the missing manifest as the first follow-up.

## `api` - server

| | |
|---|---|
| Lanes | the branch's Devbox workspace API (never the Mac); blip on the DB leg |
| Default matrix | tiers 5 (backend), 6 (database and infra), 7 (permissions and data access with the blip authz replay) |
| `full` adds | `api-deep.md`: concurrency and duplicate delivery on every mutation, the journey suite extended for each fixed cell, a log read after every fault |
| Evidence | the matrix report with the request ids of each cell, the authz report |

## `all`

The three in sequence, `app` first when present, sharing one PR and one
evidence board per target.

## The `polish` config block

Declared in the repo's `vybava.config.ts` (typed by `PolishConfig`). FixIt,
as the worked example:

```ts
polish: {
  targets: {
    app: ['apps/client/**'],
    ui: ['apps/web/**', 'apps/admin-web/**'],
    api: ['apps/api/**', 'packages/**'],
  },
  base: 'origin/main',
  out: '.polish',
  lanes: [
    { id: 'ios26', target: 'app', kind: 'ios-sim', runtime: '26', deviceType: 'iPhone 17 Pro', locale: 'cs' },
    { id: 'ios18', target: 'app', kind: 'ios-sim', runtime: '18', deviceType: 'iPhone SE (3rd generation)', locale: 'cs' },
    { id: 'iphone', target: 'app', kind: 'ios-device', device: 'Lukáš - iPhone', locale: 'cs' },
    { id: 'android', target: 'app', kind: 'android-device', nav: ['gesture', '3button'], locale: 'cs' },
    { id: 'web', target: 'ui', kind: 'browser', url: 'http://10.8.0.10:3111' },
    { id: 'api', target: 'api', kind: 'server', url: 'http://10.8.0.10:3000/health' },
  ],
  screens: [
    { id: 'home', title: 'Domů', target: 'app', url: 'fixit://e2e/navigate?to=home', area: 'home' },
  ],
},
```

`screens` are optional: without them `polish-kit shoot` needs `--screens`
with deep links on the command line, and the plan lists no touched screens.
