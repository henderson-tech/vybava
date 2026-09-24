package readiness

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/template"
)

// view is the data every template sees. Lane, Features and Journeys are set
// only for per-lane files.
type view struct {
	C        Config
	Run      Run
	Dir      string
	SimCap   int
	Lanes    []LaneSpec
	Lane     *LaneSpec
	Features []Feature
	Journeys []Journey
	// Folded are critic-unassigned commits the lane took over.
	Folded []Unassigned
}

// MacDevices are the in-scope simulators and emulators (the device runner's matrix).
func (v view) MacDevices() []Device {
	var out []Device
	for _, d := range v.Matrix() {
		if d.Host == "mac" && d.Platform != "web" {
			out = append(out, d)
		}
	}
	return out
}

// Matrix is the device matrix restricted to the authority's device list.
func (v view) Matrix() []Device {
	if len(v.Run.Authority.Devices) == 0 {
		return v.C.Devices.Matrix
	}
	var out []Device
	for _, d := range v.C.Devices.Matrix {
		for _, id := range v.Run.Authority.Devices {
			if d.ID == id {
				out = append(out, d)
			}
		}
	}
	return out
}

// Playwright reports whether any in-scope device publishes Playwright results.
func (v view) Playwright() bool {
	for _, d := range v.Matrix() {
		if d.Framework == "playwright" {
			return true
		}
	}
	return false
}

// DeviceRunner reports whether one agent owns every in-scope simulator and emulator.
func (v view) DeviceRunner() bool {
	return v.C.Devices.Runner == "device-runner" && len(v.MacDevices()) > 0
}

// Workspace is the lane stack's {ws}, when the adapter names it.
func (v view) Workspace() string {
	if v.Lane == nil {
		return ""
	}
	return strings.ReplaceAll(v.C.Lane.Workspace, "{slug}", v.Lane.Slug)
}

// Checkout is one command a lane runs to get a checkout of one repo.
type Checkout struct{ Repo, Cmd string }

// Checkouts are the lane's checkout commands: the first repo always, every
// other repo its features touch, with {slug} and {path} filled in.
func (v view) Checkouts() []Checkout {
	if v.Lane == nil {
		return nil
	}
	touched := map[string]bool{}
	for _, f := range v.Features {
		for _, r := range f.Repos {
			touched[r] = true
		}
	}
	paths := map[string]string{}
	for _, rr := range v.Run.Ranges {
		paths[rr.Repo] = rr.Path
	}
	var out []Checkout
	for i, r := range v.C.Repos {
		switch {
		case i == 0:
			out = append(out, Checkout{r.ID, strings.ReplaceAll(v.C.Lane.Worktree, "{slug}", v.Lane.Slug)})
		case touched[r.ID]:
			cmd := strings.ReplaceAll(r.Worktree, "{slug}", v.Lane.Slug)
			out = append(out, Checkout{r.ID, strings.ReplaceAll(cmd, "{path}", paths[r.ID])})
		}
	}
	return out
}

// Integration names every repo's integration branch, "app → main, eve → main".
func (v view) Integration() string {
	parts := make([]string, len(v.C.Repos))
	for i, r := range v.C.Repos {
		parts[i] = r.ID + " → " + r.Integration
	}
	return strings.Join(parts, ", ")
}

var funcs = template.FuncMap{
	"join": strings.Join,
	// fill substitutes {token} placeholders: fill .C.Lane.Worktree "slug" .Lane.Slug.
	"fill": func(s string, kv ...string) string {
		for i := 0; i+1 < len(kv); i += 2 {
			s = strings.ReplaceAll(s, "{"+kv[i]+"}", kv[i+1])
		}
		return s
	},
	"add": func(a, b int) int { return a + b },
	"div": func(a, b int) int { return a / b },
}

// render executes templates/<name> from the skill payload.
func (t *Tool) render(name string, v view) ([]byte, error) {
	src, err := fs.ReadFile(t.Payload, "templates/"+name)
	if err != nil {
		return nil, fmt.Errorf("skill payload templates/%s: %w", name, err)
	}
	tpl, err := template.New(name).Funcs(funcs).Option("missingkey=error").Parse(string(src))
	if err != nil {
		return nil, fmt.Errorf("templates/%s: %w", name, err)
	}
	var out bytes.Buffer
	if err := tpl.Execute(&out, v); err != nil {
		return nil, fmt.Errorf("templates/%s: %w", name, err)
	}
	return out.Bytes(), nil
}

// RenderData is what render reports, per file.
type RenderData struct {
	Dir       string   `json:"dir"`
	Written   []string `json:"written"`
	Unchanged []string `json:"unchanged"`
	Drifted   []string `json:"drifted,omitempty"`
	// Removed are rendered files no input produces any more (a lane dropped
	// from lanes.json, the device runner out of scope).
	Removed []string `json:"removed,omitempty"`
}

// Render writes every file the run's manifests determine; with check it
// only reports the ones that differ. Ledgers and copied scripts are never
// touched — init seeds them once.
func (t *Tool) Render(dir string, check bool) (Result, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return Result{}, err
	}
	run, err := ReadRun(dir)
	if err != nil {
		return Result{}, err
	}
	if run.Authority.Merge == "" || run.Authority.Finish == "" {
		return Result{}, diag(DiagAuthorityMissing, "run.json authority.merge and authority.finish are empty",
			"ask the phase-0 authority questions, record the answers in "+filepath.Join(dir, RunFile))
	}
	if run.Epic.URL == "" {
		return Result{}, diag(DiagEpicMissing, "every rendered file links the epic, and run.json has no epic.url",
			"create the epic task, record run.json epic {id, url}, re-run readiness render")
	}
	known := map[string]bool{}
	for _, d := range t.Config.Devices.Matrix {
		known[d.ID] = true
	}
	for _, id := range run.Authority.Devices {
		if !known[id] {
			return Result{}, diag(DiagRunInvalid, "run.json authority.devices names "+id+", which is not in devices.matrix",
				"use matrix ids only: readiness check --json | jq -r '.data.devices[]'")
		}
	}
	var lanes []LaneSpec
	hasLanes, err := readOptional(dir, LanesFile, &lanes)
	if err != nil {
		return Result{}, err
	}
	var inv Inventory
	hasInv, err := readOptional(dir, InventoryFile, &inv)
	if err != nil {
		return Result{}, err
	}
	seen := map[string]bool{}
	for _, l := range lanes {
		// A slug names files and fills shell commands: kebab-case, once.
		if !idPattern.MatchString(l.Slug) || seen[l.Slug] {
			return Result{}, diag(DiagRunInvalid, fmt.Sprintf("lanes.json: slug %q is not unique lowercase kebab-case", l.Slug),
				"repair "+filepath.Join(dir, LanesFile))
		}
		seen[l.Slug] = true
	}

	base := view{C: t.Config, Run: run, Dir: dir, SimCap: t.SimCap, Lanes: lanes}
	files := map[string][]byte{}
	var order []string
	add := func(name, tpl string, v view) error {
		out, err := t.render(tpl, v)
		if err != nil {
			return err
		}
		files[name] = out
		order = append(order, name)
		return nil
	}
	res := Result{}
	if err := add("lane-rules.md", "lane-rules.md", base); err != nil {
		return res, err
	}
	if base.DeviceRunner() {
		if err := add("device-runner.md", "device-runner.md", base); err != nil {
			return res, err
		}
	}
	if err := add("final-brief.md", "final-brief.md", base); err != nil {
		return res, err
	}
	if !hasLanes || !hasInv {
		res.Next = append(res.Next, "write "+InventoryFile+" (inventory workflow) and "+LanesFile+", then re-run readiness render")
	} else {
		for i := range lanes {
			l := &lanes[i]
			v := base
			v.Lane = l
			if v.Features, err = pick(inv, l.Features, func(c Cluster) []Feature { return c.Features }, "feature", l.Slug); err != nil {
				return res, err
			}
			if v.Journeys, err = pick(inv, l.Journeys, func(c Cluster) []Journey { return c.Journeys }, "journey", l.Slug); err != nil {
				return res, err
			}
			for _, f := range v.Features {
				for _, r := range f.Repos {
					if !slices.ContainsFunc(t.Config.Repos, func(c Repo) bool { return c.ID == r }) {
						return res, diag(DiagRunInvalid, fmt.Sprintf("lane %s: feature %q names repo %q, which is not in the readiness section", l.Slug, f.Name, r),
							"fix the feature's repos in inventory.json to the configured repo ids")
					}
				}
			}
			for _, u := range l.Unassigned {
				if u < 0 || u >= len(inv.Critic.Unassigned) {
					return res, diag(DiagRunInvalid, fmt.Sprintf("lane %s: unassigned %d is out of range (the critic lists %d)", l.Slug, u, len(inv.Critic.Unassigned)),
						"jq -r '.critic.unassigned|to_entries[]|\"\\(.key) \\(.value.ref) \\(.value.subject)\"' inventory.json")
				}
				v.Folded = append(v.Folded, inv.Critic.Unassigned[u])
			}
			if err := add("body-"+l.Slug+".md", "lane-body.md", v); err != nil {
				return res, err
			}
			if l.Story.URL == "" || l.QA.URL == "" {
				res.Diagnostics = append(res.Diagnostics, info(DiagLaneTasksPending, "lane "+l.Slug+" has no story or QA task yet; its brief waits",
					"create the story (body-"+l.Slug+".md) and its QA task, record both in lanes.json, re-run readiness render"))
				continue
			}
			if err := add("brief-"+l.Slug+".md", "lane-brief.md", v); err != nil {
				return res, err
			}
		}
	}

	data := &RenderData{Dir: dir, Written: []string{}, Unchanged: []string{}}
	res.Data = data
	stale, err := staleRendered(dir, files, hasLanes && hasInv)
	if err != nil {
		return res, err
	}
	for _, name := range stale {
		if check {
			data.Drifted = append(data.Drifted, name)
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return res, err
		}
		data.Removed = append(data.Removed, name)
	}
	for _, name := range order {
		path := filepath.Join(dir, name)
		have, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return res, err
		}
		if err == nil && bytes.Equal(have, files[name]) {
			data.Unchanged = append(data.Unchanged, name)
			continue
		}
		if check {
			data.Drifted = append(data.Drifted, name)
			continue
		}
		if err := os.WriteFile(path, files[name], 0o644); err != nil {
			return res, err
		}
		data.Written = append(data.Written, name)
	}
	if len(data.Drifted) > 0 {
		return res, diag(DiagRenderDrift, strings.Join(data.Drifted, ", ")+" differ from what the run's manifests render",
			"readiness render --dir "+dir)
	}
	if hasLanes && hasInv && len(res.Diagnostics) == 0 && res.Next == nil {
		if base.DeviceRunner() {
			res.Next = append(res.Next, `Agent({name: "device-runner", prompt: <`+filepath.Join(dir, "device-runner.md")+`>})`)
		}
		res.Next = append(res.Next,
			`Agent({name: "lane-<slug>", prompt: <`+filepath.Join(dir, "brief-<slug>.md")+`>}) per wave-A lane — waves in rotation.md`,
			"readiness render --dir "+dir+" --check   # before every later spawn or broadcast")
	}
	return res, nil
}

// staleRendered lists files in dir that render once produced and no longer
// does: body-/brief- files of lanes gone from lanes.json, and device-runner.md
// when no device runner is in scope. Lane files count only when the lane
// picture is complete: a missing inventory.json never deletes bodies.
func staleRendered(dir string, files map[string][]byte, lanesKnown bool) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		rendered := name == "device-runner.md" ||
			(lanesKnown && strings.HasSuffix(name, ".md") && (strings.HasPrefix(name, "body-") || strings.HasPrefix(name, "brief-")))
		if _, ok := files[name]; rendered && !ok && !e.IsDir() {
			out = append(out, name)
		}
	}
	return out, nil
}
