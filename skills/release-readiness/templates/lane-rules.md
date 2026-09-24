# Release lane rules — epic {{.Run.Epic.URL}}

Rendered by `readiness render` from vybava.config.ts (`readiness`) and {{.Dir}}/run.json. Never edit this file: change an input, re-render, and the orchestrator broadcasts the change. Every lane agent reads it in full before its first action; it binds every lane.

Authority (phase 0): merges via `{{.Run.Authority.Merge}}` · finish line: {{.Run.Authority.Finish}} · integration: {{.Integration}}.
{{- if .Run.Authority.Concurrency}} At most {{.Run.Authority.Concurrency}} lane agents run at once; the orchestrator starts lanes in waves and may park yours.{{end}}

## Dev environment
- Your checkouts are the commands in your brief: one per repo your lane touches{{if gt (len .C.Repos) 1}} ({{range $i, $r := .C.Repos}}{{if $i}}, {{end}}{{$r.ID}}{{end}}){{end}}, same slug. Never work in a main clone. One lane = its checkout(s) + one dev stack + one board + its PRs.
- `{ws}` below is your stack's workspace: {{if .C.Lane.Workspace}}your brief names it{{else}}read its name from the up/status output once and keep it{{end}}.
- Stack up: `{{.C.Lane.DevEnv.Up}}`. Hold it while active: `{{.C.Lane.DevEnv.Hold}}`. Park it the moment you wait on CI, an audit or a review: `{{.C.Lane.DevEnv.Park}}`; bring it back with the up verb when you need it.
{{- if .C.Lane.DevEnv.URL}}
- App addresses: `{{.C.Lane.DevEnv.URL}}`.
{{- end}}
{{- range .C.Lane.DevEnv.Checks}}
- After up, before any run: `{{.}}`
{{- end}}
- Never re-run setup/up/gen on a stack that is already up: it can restart other lanes' stacks. After a config change, recreate only your own service.
- EVERY heavy job (suite, build, browser batch) goes through `{{.C.Lane.Heavy}}`, one job per invocation. Keep jobs short; the other lanes queue behind you.
- Kill processes by recorded PID only, never `pkill`/`killall`/`pgrep` patterns. Put the lane slug in every wrapper script name.
- Never edit synced source while a run is in flight.

## Tests
{{- range .C.Tests}}
- {{.Name}}{{if .Framework}} ({{.Framework}}){{end}}: `{{.Cmd}}`
{{- end}}
- A fix needs a failing-first test in the repo's framework. Never weaken an assertion. Migrations are additive (expand-only).

## Devices
- In scope, every result from ONE commit: {{range $i, $d := .Matrix}}{{if $i}} · {{end}}{{$d.ID}} = {{$d.Name}} ({{$d.Framework}} on {{$d.Host}}){{end}}.
{{- if .C.Devices.OneRunPerLane}}
- Every run resets your stack's shared test data: never start a run on your stack while another is in flight, whoever started it (a device request included).
{{- end}}
{{- if .DeviceRunner}}
- Simulators and emulators belong to the device runner. Never boot, install on or drive one yourself. When a commit is ready for device evidence, send the orchestrator `DEVICE REQUEST <slug> <sha> worktree=<path> apiUrl=<device-reachable API url> qa=<qa id> specs=<paths> realtime=<paths|none>`. Then freeze that checkout (no edits, no runs on your stack) until `DEVICE RESULT` arrives; `BUILT` is progress, not a release.
- The runner builds {{.C.Devices.Build}} apps, runs {{range $i, $d := .MacDevices}}{{if $i}} + {{end}}{{$d.ID}}{{end}}{{if .C.Devices.OneRunPerLane}} one after another{{else}} at the same time{{end}}{{with .C.Devices.Realtime}}, runs realtime specs with the roles ({{join .Roles ", "}}) on different platforms ({{if eq .Directions "both"}}every role on every platform{{else}}one pairing{{end}}){{end}}, publishes to your QA task and reports pass/fail per device.
{{- range .Matrix}}{{if or (ne .Host "mac") (eq .Platform "web")}}
- {{.ID}} is yours to run: `{{.Run}}`{{end}}{{end}}
{{- else}}
- Every device runs a {{.C.Devices.Build}} build of the evidence commit.
{{- range .Matrix}}
- {{.ID}}: {{if .Build}}build `{{.Build}}`, then {{end}}run `{{.Run}}`
{{- end}}
{{- with .C.Devices.Realtime}}
- Realtime specs ({{join .Specs ", "}}) run with the roles ({{join .Roles ", "}}) on different platforms ({{if eq .Directions "both"}}every role on every platform{{else}}one pairing{{end}}): `{{.Run}}`. A pass that needed a reload or a pull-to-refresh is a fail.
{{- end}}
{{- end}}

## Valid evidence
- A run counts only if its results show the expected test count with real passes, and there was no exit 137, no container/app/DB restart during it, no browser or simulator launch error, and no skip on an unreachable-dependency guard. Anything else: re-run it.
- A retry-pass is not a clean pass. Triage every flake and record the verdict in the PR body.

## Publishing (a usertest board per lane)
- Publish to your lane's OWN QA task (`--task <qa id>`), never the story or the epic: a story without a QA child resolves to the epic's shared board (fixit/vitrinka#3261).
- Every run that feeds a board takes a shot per case (`VITRINKA_SHOTS=always`, or the project's equivalent in its run command). A pass without a shot is not board evidence.
- `vitrinka qa run -- <cmd>` executes `<cmd>` without a shell: wrap a command that sets variables or pipes as `-- sh -c '<cmd>'`.
{{- if .Playwright}}
- When Playwright results come as JUnit attachments (all named `test-finished-1.png`), board cards show the wrong shots, because vitrinka maps shots by basename (fixit/vitrinka#3122). Run `sh {{.Dir}}/uniq-shots.sh <checkout>/.vitrinka/runs/<runId> [<junit file name>]` before publishing. Then build the manifest with `vitrinka qa run --dry-run --results <junit> -- true`, keep the runId when republishing, and publish with `vitrinka qa run publish <manifest> --task <qa id>`.
{{- end}}
- Verify with scrape_board and get_card_image: every card shows its own case{{range .Matrix}}{{if .Width}}; {{.ID}} cards are {{.Width}} px wide{{end}}{{end}}.

## PRs
- `{{.Run.Authority.Merge}}`{{if .C.Merge.Order}}, merge order {{join .C.Merge.Order " → "}}. A later PR declares its dependency on the earlier one in its body{{end}}.
- The evidence-gated PR (the last one to merge) opens as a draft (`/prm --draft`, no `--auto`). Ready it (`gh pr ready`) only after the device evidence is published, then run `/prm <pr>` again with the merge authority's flags: prm stops at a draft and does not resume by itself.
- PR bodies link the lane task, the epic, the lane board and the evidence commit sha.
- Red CI unrelated to your diff: report it to the orchestrator and never bypass it. A queued run is not a hung run.
- On conflict, merge the integration branch in and keep both intents.
- A fix the orchestrator announces as SHARED lands once, as its own PR. Carry it as a cherry-pick only, never re-author it, and merge the integration branch once it lands so it drops out of your diff.
{{- if .C.Rules}}

## Project rules
{{- range .C.Rules}}
- {{.}}
{{- end}}
{{- end}}

## Finish
- Run `hand_back` on your lane task (status done only if every PR merged); the summary is one fact per line.
- Park your stack.
- End your final message with the lane JSON: `{"slug", "taskId", "branch", "prs": [{"repo", "number", "merged", "sha"}], "boardUrl", "qaTaskUrl", "stackUrl", "journeys": [{"name", {{range $i, $d := .Matrix}}{{if $i}}, {{end}}"{{$d.ID}}"{{end}}, "note"}], "bugsFixed", "bugsOpen", "testsAdded", "gapsClosed", "gapsSkipped", "releaseVerdict", "verdictReason"}`.
