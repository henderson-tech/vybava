# menubar-doctor — macOS menu-bar items that run but never appear

You build a menu-bar app, launch it, and nothing shows up. The process is
running. Its `NSStatusItem` reports `isVisible == true`. Its button window
exists — parked at the right edge of a screen, under the clock, or mid-air on a
display that draws no menu bar. Every other app's icons are fine. Reinstalling,
restarting Control Center, changing displays, even logging out change nothing.

The cause is not your app.

Since macOS 26, Control Center hosts every third-party menu-bar item and files
each one under the **responsible process** of whatever launched the app — not
under the app itself. An app started from a terminal (`open -a`, a build
script, `xcodebuild`, an agent shell) is therefore filed under the *terminal's*
bundle id. If that terminal's "Allow in the Menu Bar" switch in System Settings
→ Menu Bar is off — and for a terminal it usually is — every item filed under
it is silently never hosted.

The attribution is persisted, which is what makes the symptom so disorienting:

```
~/Library/Group Containers/group.com.apple.controlcenter/
    Library/Preferences/group.com.apple.controlcenter.plist
```

Its `trackedApplications` key holds a *nested* binary plist. Because it encodes
a Swift dictionary with a non-string key, it is a flat array of alternating
key/value entries; each value carries `isAllowed` (the switch in the settings
pane), `location` (the owning process) and `menuItemLocations` (every item
filed under that owner). An item whose location differs from its owner's is
*foreign*; a foreign item under a disallowed owner is invisible.

```sh
menubar-doctor                                # what is filed under whom, exit 1 if any item is invisible
menubar-doctor fix                            # unfile the invisible ones, restart Control Center
menubar-doctor fix --all                      # also unfile items that are visible under a foreign owner
menubar-doctor launch /Applications/Foo.app   # start an app so its item is attributed to itself
menubar-doctor --json                         # the same scan, machine-readable
```

## Scan

Bare, the doctor names every foreign attribution and exits 1 when any of them
is invisible, so it drops straight into a script or a CI check:

```
com.example.bar        INVISIBLE — filed under the switched-off  dev.warp.Warp-Stable
com.example.snap       INVISIBLE — filed under the switched-off  dev.warp.Warp-Stable
com.example.adopted    visible, but filed under                  com.example.host
```

The second class is worth knowing about: the item shows today, but as its
owner's item — the moment that owner is switched off, it vanishes too.

## Fix

`fix` backs the registry up to `~/Backups/menubar-doctor/`, strips the mappings
that hide items under a switched-off owner, writes it back through `defaults
import` (a direct file write would be discarded by the running preferences
daemon's cached copy) and restarts `cfprefsd` and Control Center.

Only the invisible items are repaired by default: a foreign item under an
*allowed* owner is on screen right now, and stripping it would make it
disappear until its app is relaunched. `--all` does that too.

Keys the doctor does not model are preserved verbatim, so an OS update that
adds fields to the registry cannot be flattened by a repair.

## Launch — the preventative half

`fix` restores correct attribution, but the app still has to be relaunched
outside the shell's process tree, or Control Center files it under the terminal
again on the spot:

```sh
menubar-doctor launch /Applications/SwitcherooBar.app
```

That runs the binary under `launchd` (`launchctl submit`), so the responsible
process is launchd and the item is attributed to the app. Finder and Spotlight
do the same thing. `open -a` from a terminal is exactly what creates the trap,
so any script, build or agent that starts a menu-bar app should use `launch`.

Forget the job afterwards with `launchctl remove vybava.menubar.<binary>`.

## Notes

- macOS only. Off macOS every verb refuses with an explanation rather than
  pretending the registry exists.
- Nothing here needs `sudo`, and nothing touches an app bundle or its code
  signature — only the per-user Control Center preference.
- If an app is still invisible after a repair *and* a detached relaunch, check
  the app's own row in System Settings → Menu Bar: the per-app switch is
  authoritative and no API can read or set it.
