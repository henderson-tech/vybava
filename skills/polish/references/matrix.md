# Adverse-condition matrix

Rows × the feature's flows = cells; "Correct" is the pass criterion. Commands assume `blip up api` (HTTP leg) and `blip up db` (TCP leg to Postgres) from SKILL.md step 4.

## 1. Client network (blip on the API leg)

| Condition | Provoke | Correct |
|---|---|---|
| Offline at the moment of action | `blip api set drop`, then tap | inline error with retry, input preserved, no endless spinner |
| Blip mid-request | `blip api set drop --after 1 --for 3s`, submit; or `blip api cut` while submitting | retry produces one server effect, UI resolves |
| Slow network | `blip api set delay 3s --jitter 2s` | loading state, submit disabled or debounced, no double submit |
| Hung request | `blip api set timeout` | client timeout under 30 s, recoverable |
| Intermittent 5xx | `blip api set error 503 --rate 0.5` | error surfaced, retry works, no crash |
| Session expiry | `blip api set error 401 --match '/api/*'` | refresh or re-login, back to the same screen with state |
| Flapping | `blip api set flap 5s/10s` during the flow | eventual consistency, no zombie state |
| Slow body | `blip api set slow 20kbps` on lists and images | progressive rendering, cancelled on navigate away |
| Garbage response | `blip api set error 200 --body 'not json' --match <endpoint>` | parse error handled, no white screen |

## 2. App lifecycle — iOS simulator / Android emulator

| Condition | Provoke (iOS · Android) | Correct |
|---|---|---|
| Background during a request | `xcrun simctl launch booted com.apple.mobilesafari` then relaunch the app · `adb shell input keyevent KEYCODE_HOME` then `adb shell am start -n <pkg>/<activity>` | request completes or retries; screen state intact |
| Kill and relaunch mid-flow | `xcrun simctl terminate booted <bundle>` + `xcrun simctl launch booted <bundle>` · `adb shell am force-stop <pkg>` + start | draft or queue survives, no half-written record |
| Cold deep link into the flow | `xcrun simctl openurl booted '<scheme>://…'` · `adb shell am start -a android.intent.action.VIEW -d '<url>'` | right screen, auth handled |
| Push while the flow is open | `xcrun simctl push booted <bundle> payload.json` · iOS only | no navigation hijack mid-input |
| Permission revoked | `xcrun simctl privacy booted revoke <photos|camera|location> <bundle>` · `adb shell pm revoke <pkg> <permission>` | explains and offers settings, no crash |
| Memory warning | Simulator menu Debug → Simulate Memory Warning · `adb shell am send-trim-memory <pkg> RUNNING_CRITICAL` | no lost input |
| Rotation | Simulator ⌘← / ⌘→ · `adb shell settings put system accelerometer_rotation 0 && adb shell settings put system user_rotation 1` | layout and state hold |
| Accessibility sizes / dark | `xcrun simctl ui booted content_size extra-extra-extra-large`, `xcrun simctl ui booted appearance dark` · `adb shell settings put system font_scale 1.3` | no clipped CTA, readable |
| Doze / back stack (Android) | `adb shell dumpsys battery unplug && adb shell dumpsys deviceidle force-idle` (reset: `adb shell dumpsys battery reset`); `adb shell input keyevent KEYCODE_BACK` at each step | back never loses a submitted step or double-submits |

## 3. Web — browser

With a configurable API base prefer blip, so cells 1.x run unchanged. Browser-only cells:

| Condition | Provoke | Correct |
|---|---|---|
| Offline toggle | Playwright `context.setOffline(true)`; chrome-devtools `emulate` offline | as 1.1 |
| Route abort / throttle | `page.route('**/api/**', r => r.abort())`; `emulate` Slow 3G | as 1.2–1.4 |
| Reload mid-mutation | `page.reload()` right after submit | one effect, state reconciled on load |
| Back/forward after submit | browser back, then forward | no resubmission prompt loop, no duplicate |
| Two tabs, same record | edit in tab B, save in tab A | conflict surfaced or last-write documented |
| Tab suspended | hide the tab 60 s (`document.visibilityState`), return | sockets/polls resume, data fresh |
| Cookie/session expiry | delete the session cookie, act | as 1.6 |

## 4. Input and UI pitfalls — any tier

Double-tap submit · navigate away during load · empty list, one item, 500 items · very long strings, emoji, RTL · zero, negative, decimal, `cs-CZ` comma decimals (see money-locale) · paste into every field · keyboard covering the CTA · safe areas · stale data after another actor changes the record (change on B, act on A).

## 5. Backend and API — Go, Nest.js

| Condition | Provoke | Correct |
|---|---|---|
| Restart mid-request | `docker compose restart api` (or kill the process) with a request in flight | client retries, no partial write |
| Process paused | `docker compose pause api` 10 s then `unpause`, or `kill -STOP <pid>` / `-CONT` | health flips, client times out and recovers |
| Duplicate delivery | replay the same POST/webhook twice with identical body and idempotency key | exactly one effect |
| Concurrent writers | two writes to the same record in parallel (`xargs -P2 curl …`) | no lost update; conflict surfaced |
| Bad input at the edge | oversized body, unknown fields, wrong types, missing auth | 4xx with a body, never a 500 |
| Downstream dead | `blip up ext --listen :<port> --to tcp://<dep>` in front of Redis or an external API, `blip ext set drop` | degraded path, backoff, log names the cause |
| Downstream slow | `blip db set delay 2s` | per-request timeout; the pool does not starve the whole server |
| Job/cron overlap | trigger the job twice concurrently | lock or idempotent |

## 6. Database and infra

| Condition | Provoke | Correct |
|---|---|---|
| Connection loss | `blip db cut`; or `docker compose restart postgres` — restart only, never recreate | reconnect without a server restart; in-flight transaction rolled back |
| Pool exhaustion | `blip db set delay 5s` + a burst of requests | queued or timed out with an error body, recovers on heal |
| Read-only / failed statement | `blip db set drop --after 3` inside a multi-statement flow | transaction atomic, nothing half-committed |
| Migration on live data | back up first (`db:backup`), run the migration against the dev DB with the old code still serving | expand/contract holds |

## 7. Permissions and data access — any API

blip replays the flow's recorded requests under another identity; the other rows are hand-made requests against the running server. A candidate or an unexpected 2xx is a finding.

| Condition | Provoke | Correct |
|---|---|---|
| Unauthenticated | `blip api authz --as none` | 401 on every non-public route |
| Other tenant / other user | `blip api authz --as env:OTHER_TOKEN` (second test user, other tenant) | 403 or 404 on every record-bound route, no candidate |
| Lower role | `blip api authz --as env:VIEWER_TOKEN --mutations` on a throwaway tenant | writes denied |
| Expired / tampered token | replay one request with an expired token; with one altered character | 401, no stack trace in the body |
| Object reference swap | take a flow's record id, replace it with another tenant's id in path and body | 404/403; list endpoints return only own rows |
| Privileged field in the body | add `role`, `tenantId`, `ownerId`, `price`, `isAdmin` to a create/update body | ignored or 400, never persisted |
| Filter, sort, include params | `?filter=` / `?sort=` / `?include=` on foreign columns and relations | 400 or scoped, never a cross-tenant row |
| Bulk, export, search routes | call them as the other tenant | scoped or denied |
| Direct DB scope | `SELECT` the flow's tables as the app role | every row carries its owning tenant/user column and the app filters on it, or RLS is on |
| File and signed-URL access | open another tenant's attachment URL; an expired signed URL | denied |
