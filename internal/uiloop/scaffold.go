package uiloop

import (
	"bytes"
	"embed"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

//go:embed scaffold/*.tmpl
var scaffoldFS embed.FS

// InitData is what init reports.
type InitData struct {
	Created []string     `json:"created"`
	Kept    []string     `json:"kept"`
	Vendor  VendorReport `json:"vendor"`
}

// Init scaffolds <dir> (project.ts, screens/example.ts), the spec template,
// the gitignore line for <out>, and syncs the harness. It never overwrites a
// file that exists.
func (t *Tool) Init() (Result, error) {
	data := InitData{Created: []string{}, Kept: []string{}}
	c := t.Config
	// project.ts imports the example only when init creates it: a repo that
	// already has screens/*.ts gets a project.ts that imports nothing it lacks.
	screens, _ := filepath.Glob(t.abs(c.Dir + "/screens/*.ts"))
	vars := map[string]any{"Area": c.Areas[0], "Grid": c.Lint.Grid, "TouchTarget": c.Lint.TouchTarget, "Example": len(screens) == 0}
	write := func(rel, tmpl string) error {
		target := t.abs(rel)
		if _, err := os.Stat(target); err == nil {
			data.Kept = append(data.Kept, rel)
			return nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		src, err := scaffoldFS.ReadFile("scaffold/" + tmpl)
		if err != nil {
			return err
		}
		out, err := render(tmpl, string(src), vars)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, out, 0o644); err != nil {
			return err
		}
		data.Created = append(data.Created, rel)
		return nil
	}
	if err := write(c.Dir+"/project.ts", "project.ts.tmpl"); err != nil {
		return Result{}, err
	}
	if len(screens) == 0 {
		if err := write(c.Dir+"/screens/example.ts", "example.ts.tmpl"); err != nil {
			return Result{}, err
		}
	}
	if c.Spec != "" {
		if err := write(c.Spec, specTemplate); err != nil {
			return Result{}, err
		}
	}
	added, err := ensureIgnored(t.Root, c.Out)
	if err != nil {
		return Result{}, err
	}
	if added {
		data.Created = append(data.Created, ".gitignore: /"+strings.Trim(c.Out, "/")+"/")
	}
	synced, err := t.Sync(false)
	if err != nil {
		return Result{Data: data}, err
	}
	data.Vendor = synced.Data.(VendorReport)
	return Result{Data: data, Next: []string{"vybava ui-loop map", "vybava ui-loop check --json"}}, nil
}

// render executes one scaffold template; a key it names that vars lacks is an error.
func render(name, src string, vars map[string]any) ([]byte, error) {
	tpl, err := template.New(name).Option("missingkey=error").Parse(src)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := tpl.Execute(&out, vars); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// specTemplate is the spec init scaffolds.
const specTemplate = "ui-spec.md.tmpl"

// specRule is a line of the scaffolded spec that states a lint knob's value.
type specRule struct {
	Knob  string // lint.grid | lint.touchTarget
	Value int
	Line  string // as init writes it today, without the list marker
}

// specRules renders the scaffold spec's lines that state a lint value with
// the config's effective values: the sentence init would write today, so
// `check` can tell when the spec and the lint went apart.
func specRules(l Lint) ([]specRule, error) {
	src, err := scaffoldFS.ReadFile("scaffold/" + specTemplate)
	if err != nil {
		return nil, err
	}
	knobs := []struct {
		action, knob string
		value        int
	}{{"{{.Grid}}", "lint.grid", l.Grid}, {"{{.TouchTarget}}", "lint.touchTarget", l.TouchTarget}}
	vars := map[string]any{"Grid": l.Grid, "TouchTarget": l.TouchTarget}
	var rules []specRule
	for _, line := range strings.Split(string(src), "\n") {
		for _, k := range knobs {
			if !strings.Contains(line, k.action) {
				continue
			}
			out, err := render(specTemplate, line, vars)
			if err != nil {
				return nil, err
			}
			rules = append(rules, specRule{Knob: k.knob, Value: k.value, Line: strings.TrimPrefix(strings.TrimSpace(string(out)), "- ")})
			break
		}
	}
	return rules, nil
}

// ensureIgnored appends /<out>/ to the repo's .gitignore unless a line
// already names it.
func ensureIgnored(root, out string) (bool, error) {
	name := strings.Trim(out, "/")
	file := filepath.Join(root, ".gitignore")
	existing, err := os.ReadFile(file)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	for _, line := range strings.Split(string(existing), "\n") {
		if strings.Trim(strings.TrimSpace(line), "/") == name {
			return false, nil
		}
	}
	prefix := ""
	if len(existing) > 0 && !bytes.HasSuffix(existing, []byte("\n")) {
		prefix = "\n"
	}
	f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return false, err
	}
	if _, err := f.WriteString(prefix + "/" + name + "/\n"); err != nil {
		_ = f.Close()
		return false, err
	}
	return true, f.Close()
}
