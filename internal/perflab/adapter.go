package perflab

import (
	"context"
	"slices"
	"strings"
)

// AdapterField is one templated field as `adapter check` shows it: the
// template, the value with every project-static token filled (the rest
// stay {token} until a verb resolves them), and the tokens it may use.
type AdapterField struct {
	Field    string   `json:"field"`
	Template string   `json:"template"`
	Resolved string   `json:"resolved"`
	Allowed  []string `json:"allowed"`
}

// AdapterData is `adapter check`'s payload.
type AdapterData struct {
	ProjectDir string         `json:"projectDir"`
	ConfigPath string         `json:"configPath"`
	Branch     string         `json:"branch"`
	Fields     []AdapterField `json:"fields"`
	Scenarios  []ScenarioRow  `json:"scenarios"`
}

// AdapterCheck validates the perflab section (unknown keys and tokens are
// CONFIG_INVALID), shows every command for this worktree and decodes the
// scenario rows the scenarios command prints.
func (t *Tool) AdapterCheck(ctx context.Context) (Result, error) {
	c, err := t.Cfg()
	if err != nil {
		return Result{}, err
	}
	allowed := fieldTokens()
	static := NewVars(map[string]string{"date": t.Now().Format("2006-01-02"), "appRoot": c.App.Root})
	if t.Branch != "" {
		static.Set("branch", t.Branch)
		static.Set("branchSlug", slug(t.Branch))
	}
	if c.API != nil && c.API.WS != "" {
		if ws, err := static.Expand("api.ws", c.API.WS, false); err == nil {
			static.Set("ws", ws)
		}
	}
	for _, name := range Tokens {
		if _, ok, _ := static.Get(name); !ok {
			static.Set(name, "{"+name+"}")
		}
	}
	data := AdapterData{ProjectDir: t.ProjectDir, ConfigPath: t.ConfigPath, Branch: t.Branch, Fields: []AdapterField{}, Scenarios: []ScenarioRow{}}
	for _, f := range c.Fields() {
		key := f[0]
		switch {
		case strings.HasPrefix(key, "profiles."):
			key = "profiles.*.env"
		case strings.HasPrefix(key, "runner.env."):
			key = "runner.env.*"
		}
		resolved, _ := static.Expand(f[0], f[1], false)
		data.Fields = append(data.Fields, AdapterField{Field: f[0], Template: f[1], Resolved: resolved, Allowed: slices.Clone(allowed[key])})
	}
	res := Result{Data: &data, Lines: []string{"perflab section of " + t.ConfigPath + " is valid"}}
	rows, err := t.Scenarios(ctx, "")
	if err != nil {
		var d = errDiag(CodeOf(err), err.Error(), adapterFix)
		if d.Code == "" {
			d.Code = DiagAdapterCommandFailed
		}
		res.Diagnostics = append(res.Diagnostics, d)
		res.Next = []string{adapterFix}
		return res, nil
	}
	data.Scenarios = rows
	for _, r := range rows {
		res.Lines = append(res.Lines, "scenario "+r.Name)
	}
	res.Next = []string{"perflab device list --json", "perflab doctor --for all --json"}
	return res, nil
}
