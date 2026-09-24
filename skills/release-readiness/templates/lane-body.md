Lane of epic {{.Run.Epic.URL}}: release readiness of {{.Integration}}.

**Scope:** every feature below, tested live on the lane stack across the release device matrix ({{range $i, $d := .Matrix}}{{if $i}}, {{end}}{{$d.Name}}{{end}}), bugs fixed in this lane, tests in the repo frameworks, a usertest board via `vitrinka qa run`, and PRs via `{{.Run.Authority.Merge}}`.
{{range .Features}}
## {{.Name}}  _(risk: {{.Risk}}; devices: {{if .Devices}}{{join .Devices ", "}}{{else}}all{{end}})_
{{.Summary}}

- **Refs:** {{join .Refs ", "}}
{{- if .Surfaces}}
- **Surfaces:** {{join .Surfaces "; "}}
{{- end}}
- **Existing tests:** {{if .ExistingTests}}{{join .ExistingTests ", "}}{{else}}none found{{end}}
- **Gaps to close:**
{{- range .Gaps}}
  - [ ] {{.}}
{{- end}}
{{end}}
{{- if .Folded}}
## Commits no cluster claimed
The inventory critic found these functional commits outside every cluster; this lane owns them. Check what each ships, and cover it live and with a test.
{{range .Folded}}
- [ ] {{.Ref}}: {{.Subject}}
{{- end}}
{{end}}
{{- if .Journeys}}
## Journeys to walk
{{range .Journeys}}
### {{.Name}}  _(roles: {{join .Roles ", "}}; devices: {{join .Devices ", "}})_
{{.Steps}}

- **Data needs:** {{.DataNeeds}}
{{end}}
{{- end}}
