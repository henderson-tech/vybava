# Final phase — epic {{.Run.Epic.URL}}

You run the final phase of this release-readiness run in a FRESH context; the orchestrator has handed off to you. Your inputs are all in {{.Dir}}:
- run.json and lanes.json;
- results.md (one line per lane outcome);
- decisions.md (the human's gate list);
- lane-rules.md, which still binds you;
- final-phase.md, the order and the readiness board layout.

0. **Gate.** Every lane in lanes.json has a line in results.md, and the orchestrator has stopped every lane agent{{if .DeviceRunner}} and the device runner{{end}}. If not, stop and say which lane is missing: the final phase never overlaps live lanes.{{if .DeviceRunner}} The simulators and emulators are yours now: device-runner.md's rules bind you (at most {{.C.Devices.ConcurrentDevices}} devices, UDID pins, distinct ports, PID-only kills).{{end}}
1. **Merged-branch roll-up.** Bring up ONE stack on the merged integration heads ({{.Integration}}) and record their commit shas. Run every lane's device journeys across the whole matrix ({{range $i, $d := .Matrix}}{{if $i}}, {{end}}{{$d.ID}}{{end}}) at that one commit, and publish to the roll-up QA task{{if .Run.Rollup.URL}} #{{.Run.Rollup.ID}} ({{.Run.Rollup.URL}}){{else}} (run.json has none yet: create it under the epic, record run.json rollup, then publish){{end}}. Only this run catches cross-lane conflicts in shared test data: accounts, seeded tenants, suites that delete what another suite needs.
2. **Targeted re-runs.** Re-run every failure on its own, to separate interference from regressions. A failure that reproduces alone is a regression: fix it (lane rules apply) or record it in decisions.md.
{{- if .Run.Authority.DeviceWalk}}
3. **Real device walk.** Walk the top journeys the way a person would, not by re-running specs, on {{range $i, $d := .MacDevices}}{{if $i}} and {{end}}{{$d.Name}}{{else}}the release devices{{end}}. Capture the screens a user sees for the readiness board.
{{- end}}
{{- if .C.Final.Checks}}
4. **Release gates**, read-only, on the merged heads: {{range $i, $c := .C.Final.Checks}}{{if $i}}, {{end}}`{{$c}}`{{end}}. A failing gate is fixed (lane rules apply) or recorded in decisions.md with its evidence.
{{- end}}
5. **Readiness board** (layout: {{.Dir}}/final-phase.md):
   - a verdict card: integration code GO/NO-GO and production deploy GO/NO-GO;
   - chips: roll-up counts, commits, stack, device walks;
   - the roll-up's go/no-go per lane × device;
   - the lanes with their boards, PRs and evidence;
{{- if .Run.Authority.DeviceWalk}}
   - the device walk;
{{- end}}
   - production blockers and human decisions, from decisions.md.
6. **Close out.**
   - Every lane task is done, or explicitly open with its reason.
   - `hand_back` on the epic, naming the roll-up stack URL.
   - Lane stacks parked. The roll-up stack stays up and hand-testable: its URL goes on the board and in the hand_back.
   - Devices you booted shut down by UDID.
   - The run directory committed to ~/Exports.
{{- if .C.Final.Handoff}}
- The release itself (`{{.C.Final.Handoff}}`) is the human's call. Never run it; name it in the epic's hand_back.
{{- end}}
