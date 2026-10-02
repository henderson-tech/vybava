# Device lab

`doctor` returns a `HUMAN_CHECK` row for each setting it cannot read; ask the human for those. Evidence cites FixIt, as in `rn-render-cost-rules.md`.

## Refresh classes

- ProMotion (120 Hz) exists only on Pro (iPhone 13 Pro and newer) and Air models; other iPhones are 60 Hz.
- Samsung offers 120 Hz only at FHD+ resolution, with Display > Motion smoothness > High.

## The perf profile (adapter `profiles.perf.env`)

- A PERF_MODE release build, profileable by shell and not debuggable.
- Sentry profiling and replay off, and no session recorder in the bundle.
  Evidence: dev-tier Sentry replay stalled touch-down 30-47 ms (PR #1746).
- Android bakes the API as `localhost:<devicePort>` and allows cleartext to it; perflab reverses and forwards the port, so one build serves any API port.
  Evidence: without cleartext every sign-in failed with "Network Error" (f530945f05).
- iOS has no adb reverse: the bundle bakes an origin the phone can reach (LAN or VPN).
- On a personal phone, measure the dev package (its own application id); the runner attaches to the variant perflab installed.
  Evidence: release-mode Appium with `noReset: false` clears app data (PR #1770).

## iOS

- `HUMAN_CHECK` settings: Settings > Developer > Enable UI Automation; Auto-Lock Never; Low Power Mode off (it caps ProMotion at 60 Hz); Accessibility > Motion > Limit Frame Rate off; Reduce Motion off.
- A packed variant runs with `EXUpdatesEnabled=NO`: fair for an A/B, not production-equivalent.
- A simulator never renders 120 Hz and refuses the Hitches instrument.
- Which API and account an installed build uses, under your lease: `perflab device pull <id> --lease <t> --domain-type appDataContainer --domain-id <bundleId> Documents/mmkv/<file> <local>`.
- Calibration (calendar view switches): Air 4.81 -> 0.32-1.27 ms/s; iPhone 11 2.77-3.72 ms/s, most hitches at the system menu.

## Android

- adb-injected input skips Samsung's kernel touch boost: adb numbers read pessimistic but stable (low RenderThread clocks). Injecting through `/dev/input` is SELinux-blocked without root (PR #1770). Confirm the worst screens with a real finger.
- `probe` gestures stay mid-content (no edge bounce, no pull-to-refresh) and scale from `wm size`; a screen that needs another path gets `--gesture-file`.
- A packed variant (bundle swapped into an APK) runs as built: on the S20 the swapped bundle loaded, logged and crashed nothing. `pack` also turns expo-updates off, or an over-the-air update could replace the swapped bundle under the APK-sha fence.
  Evidence: the build lane spike, pf1-0b95220e9a40cd6e79d2 + a marker bundle (Výbava docs/perflab.md "Android pack").

