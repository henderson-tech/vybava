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
	vars := map[string]any{"Area": c.Areas[0], "Grid": c.Lint.Grid, "TouchTarget": c.Lint.TouchTarget}
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
		tpl, err := template.New(tmpl).Option("missingkey=error").Parse(string(src))
		if err != nil {
			return err
		}
		var out bytes.Buffer
		if err := tpl.Execute(&out, vars); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, out.Bytes(), 0o644); err != nil {
			return err
		}
		data.Created = append(data.Created, rel)
		return nil
	}
	if err := write(c.Dir+"/project.ts", "project.ts.tmpl"); err != nil {
		return Result{}, err
	}
	screens, _ := filepath.Glob(t.abs(c.Dir + "/screens/*.ts"))
	if len(screens) == 0 {
		if err := write(c.Dir+"/screens/example.ts", "example.ts.tmpl"); err != nil {
			return Result{}, err
		}
	}
	if c.Spec != "" {
		if err := write(c.Spec, "ui-spec.md.tmpl"); err != nil {
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
