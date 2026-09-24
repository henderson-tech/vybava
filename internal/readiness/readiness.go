// Package readiness is the deterministic layer of the release-readiness
// skill: it validates a project's `readiness` section of vybava.config.ts,
// freezes each repo's production..integration range, seeds a run directory
// with the skill's scripts and ledgers, and renders lane rules, lane bodies
// and agent briefs from the run's manifests. The templates are the skill's
// own files (skills/release-readiness/templates), read from the embedded
// payload, so the skill text and the renderer can never disagree.
package readiness

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/runx"
	"github.com/henderson-tech/vybava/internal/vconfig"
)

// Section is the vybava.config.ts key this applet owns.
const Section = "readiness"

// Tool is one repository's readiness configuration plus the skill payload.
type Tool struct {
	Root    string // directory holding vybava.config.ts
	Config  Config
	Payload fs.FS // skills/release-readiness
	// SimCap is the repo's claude-guards simCap (default 2).
	SimCap int
	Home   string
	Now    func() time.Time
}

// Result is what a verb hands the envelope.
type Result struct {
	Data        any
	Diagnostics []runx.Diagnostic
	Next        []string
}

// Open loads and validates the section for cwd.
func Open(cwd string, payload fs.FS) (*Tool, error) {
	cfg, err := vconfig.Load(cwd)
	if err != nil {
		if errors.Is(err, vconfig.ErrNotFound) {
			return nil, diag(DiagConfigMissing, err.Error(), "add a readiness section to vybava.config.ts (docs/readiness.md)")
		}
		return nil, diag(DiagConfigInvalid, err.Error(), "fix the config file so `vybava config show --json` evaluates it")
	}
	t := &Tool{Root: cfg.Root, Payload: payload, SimCap: DefaultConcurrent, Now: time.Now}
	if err := cfg.Section(Section, &t.Config); err != nil {
		if errors.Is(err, vconfig.ErrNoSection) {
			return nil, diag(DiagConfigMissing, cfg.Path+" has no readiness section", "add one (docs/readiness.md)")
		}
		return nil, diag(DiagConfigInvalid, err.Error(), "fix the readiness section in "+cfg.Path+" (unknown keys are rejected; docs/readiness.md has the shape)")
	}
	if problems := t.Config.Validate(); len(problems) > 0 {
		return nil, diag(DiagConfigInvalid, strings.Join(problems, "; "), "fix the readiness section in "+cfg.Path)
	}
	// claude-guards owns the guards section; only its simCap matters here.
	if raw, ok := cfg.Sections["guards"]; ok {
		var g struct {
			SimCap int `json:"simCap"`
		}
		if err := json.Unmarshal(raw, &g); err != nil {
			return nil, diag(DiagConfigInvalid, "guards: "+err.Error(), "fix the guards section in "+cfg.Path)
		}
		if g.SimCap > 0 {
			t.SimCap = g.SimCap
		}
	}
	if t.Home, err = os.UserHomeDir(); err != nil {
		return nil, err
	}
	return t, nil
}

// CheckData is what check reports.
type CheckData struct {
	Root     string      `json:"root"`
	Project  string      `json:"project"`
	Runner   string      `json:"runner"`
	Build    string      `json:"build"`
	Devices  []string    `json:"devices"`
	Budget   int         `json:"concurrentDevices"`
	SimCap   int         `json:"simCap"`
	Ranges   []RepoRange `json:"ranges"`
	Plumbing []string    `json:"plumbing,omitempty"`
}

// Check validates the section against the machine and the repos.
func (t *Tool) Check(fetch bool) (Result, error) {
	c := t.Config
	data := CheckData{
		Root: t.Root, Project: c.Vitrinka.Workspace + "/" + c.Vitrinka.Project,
		Runner: c.Devices.Runner, Build: c.Devices.Build,
		Budget: c.Devices.ConcurrentDevices(), SimCap: t.SimCap, Plumbing: c.Plumbing,
		Devices: []string{}, Ranges: []RepoRange{},
	}
	for _, d := range c.Devices.Matrix {
		data.Devices = append(data.Devices, d.ID)
	}
	res := Result{Data: &data}
	for _, d := range c.Devices.Matrix {
		if c.Devices.Build == "release" && d.Host == "mac" && d.Platform != "web" && d.Build == "" {
			res.Diagnostics = append(res.Diagnostics, warn(DiagDeviceBuildMissing,
				"device "+d.ID+" runs a release build but has no build command",
				"phase 3 plumbing: add the build, then set devices.matrix."+d.ID+".build"))
		}
	}
	if c.Devices.Runner == "device-runner" && data.Budget > t.SimCap {
		res.Diagnostics = append(res.Diagnostics, warn(DiagSimCapBelowDevices,
			fmt.Sprintf("devices.concurrent is %d but guards.simCap is %d — claude-guards will refuse the extra boots", data.Budget, t.SimCap),
			fmt.Sprintf("set guards.simCap: %d, or lower devices.concurrent", data.Budget)))
	}
	for _, p := range c.Plumbing {
		res.Diagnostics = append(res.Diagnostics, info(DiagPlumbingPending, p, "build it in phase 3, before lanes start"))
	}
	ranges, diags, err := t.ranges(fetch)
	res.Diagnostics = append(res.Diagnostics, diags...)
	data.Ranges = ranges
	if err != nil {
		return res, err
	}
	res.Next = []string{"readiness init --json"}
	return res, nil
}

// Range resolves production..integration for every repo, now.
func (t *Tool) Range(fetch bool) (Result, error) {
	ranges, diags, err := t.ranges(fetch)
	return Result{Data: ranges, Diagnostics: diags}, err
}

func (t *Tool) ranges(fetch bool) ([]RepoRange, []runx.Diagnostic, error) {
	var out []RepoRange
	var diags []runx.Diagnostic
	for _, r := range t.Config.Repos {
		rr, d, err := t.resolve(r, fetch)
		diags = append(diags, d...)
		if err != nil {
			return out, diags, err
		}
		out = append(out, rr)
	}
	return out, diags, nil
}

// repoBase is the directory repo paths are relative to: the config's
// directory, seen from the MAIN checkout of its repo, so a sibling path
// (../eve-ai-layer) resolves the same from any worktree. Outside git (or on
// a git too old for --path-format) it is the config's directory as found.
func (t *Tool) repoBase() string {
	common, err := git(t.Root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || filepath.Base(common) != ".git" {
		return t.Root
	}
	prefix, err := git(t.Root, "rev-parse", "--show-prefix")
	if err != nil {
		return t.Root
	}
	return filepath.Join(filepath.Dir(common), prefix)
}

func (t *Tool) resolve(r Repo, fetch bool) (RepoRange, []runx.Diagnostic, error) {
	path := r.Path
	if !filepath.IsAbs(path) {
		path = filepath.Join(t.repoBase(), path)
	}
	path = filepath.Clean(path)
	rr := RepoRange{Repo: r.ID, Path: path, GitHub: r.GitHub}
	var diags []runx.Diagnostic
	if _, err := git(path, "rev-parse", "--git-dir"); err != nil {
		return rr, diags, diag(DiagRepoMissing, "repos."+r.ID+": "+path+" is not a git checkout", "clone "+r.GitHub+" there, or fix repos."+r.ID+".path")
	}
	remote := r.RemoteName()
	if fetch {
		if _, err := git(path, "fetch", "--quiet", "--tags", remote); err != nil {
			diags = append(diags, warn(DiagFetchFailed, r.ID+": "+err.Error(), "the range is computed from the last fetched state"))
		}
	}
	rr.Integration = remote + "/" + r.Integration
	sha, err := git(path, "rev-parse", "--verify", "--quiet", rr.Integration+"^{commit}")
	if err != nil {
		return rr, diags, diag(DiagRefMissing, r.ID+": integration branch "+rr.Integration+" does not resolve", "git -C "+path+" fetch "+remote)
	}
	rr.IntSHA = sha
	if r.Production.Branch != "" {
		rr.Production = remote + "/" + r.Production.Branch
	} else {
		args := []string{"describe", "--tags", "--abbrev=0", "--match", r.Production.Tag}
		if r.Production.Exclude != "" {
			args = append(args, "--exclude", r.Production.Exclude)
		}
		tag, err := git(path, append(args, rr.Integration)...)
		if err != nil {
			return rr, diags, diag(DiagRefMissing, r.ID+": no tag matching "+r.Production.Tag+" is reachable from "+rr.Integration+" ("+err.Error()+")", "git -C "+path+" fetch --tags "+remote)
		}
		rr.Production = tag
	}
	if rr.ProdSHA, err = git(path, "rev-parse", "--verify", "--quiet", rr.Production+"^{commit}"); err != nil {
		return rr, diags, diag(DiagRefMissing, r.ID+": production "+rr.Production+" does not resolve ("+err.Error()+")", "git -C "+path+" fetch --tags "+remote)
	}
	rr.Range = rr.Production + ".." + rr.Integration
	count, err := git(path, "rev-list", "--count", "--no-merges", rr.Range)
	if err != nil {
		return rr, diags, err
	}
	if rr.Commits, err = strconv.Atoi(count); err != nil {
		return rr, diags, fmt.Errorf("%s: rev-list --count: %w", r.ID, err)
	}
	return rr, diags, nil
}

// InitData is what init reports.
type InitData struct {
	Dir     string   `json:"dir"`
	Created []string `json:"created"`
	Kept    []string `json:"kept"`
	Run     Run      `json:"run"`
}

// payloadCopies are the skill files a run directory carries, run-dir name →
// payload path. They are copied, never linked: a run adapts them in place.
var payloadCopies = [][2]string{
	{"slot", "scripts/slot"},
	{"uniq-shots.sh", "scripts/uniq-shots.sh"},
	{"inventory.workflow.js", "workflows/inventory.js"},
	{"final-phase.md", "references/final-phase.md"},
}

// ledgers are rendered once and then only ever appended to by the orchestrator.
var ledgers = []string{"results.md", "decisions.md", "rotation.md"}

// Init creates (or completes) a run directory. It never overwrites run.json,
// a ledger or a copied script.
func (t *Tool) Init(dir, date string, fetch bool) (Result, error) {
	if date == "" {
		date = t.Now().Format(time.DateOnly)
	}
	if _, err := time.Parse(time.DateOnly, date); err != nil {
		return Result{}, diag(DiagRunInvalid, "--date "+date+" is not YYYY-MM-DD", "readiness init --date "+t.Now().Format(time.DateOnly))
	}
	if dir == "" {
		if t.Config.Exports == "" {
			return Result{}, diag(DiagDirRequired, "no --dir and the readiness section names no exports folder", "pass --dir, or set readiness.exports")
		}
		dir = filepath.Join(t.Home, "Exports", t.Config.Exports, "release-readiness-"+date)
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Result{}, err
	}
	data := &InitData{Dir: dir, Created: []string{}, Kept: []string{}}
	res := Result{Data: data}

	// Read the existing args BEFORE writing anything: a malformed file must fail
	// init while run.json is still untouched.
	var oldArgs InventoryArgs
	hadArgs, err := readOptional(dir, ArgsFile, &oldArgs)
	if err != nil {
		return res, err
	}

	run, err := ReadRun(dir)
	var de runx.DiagError
	freshRun := false
	switch {
	case err == nil:
		data.Kept = append(data.Kept, RunFile)
	case errors.As(err, &de) && de.Diag.Code == DiagRunMissing:
		ranges, diags, err := t.ranges(fetch)
		res.Diagnostics = append(res.Diagnostics, diags...)
		if err != nil {
			return res, err
		}
		// Frozen means frozen: every later fetch moves origin/<integration>,
		// so the range the inventory reads is pinned to shas here.
		for i := range ranges {
			ranges[i].Range = ranges[i].ProdSHA + ".." + ranges[i].IntSHA
		}
		run = Run{V: 1, Date: date, Dir: dir, Ranges: ranges}
		if err := writeJSON(filepath.Join(dir, RunFile), run); err != nil {
			return res, err
		}
		data.Created = append(data.Created, RunFile)
		freshRun = true
	default:
		return res, err
	}
	data.Run = run

	// The args are derived from run.json except the clusters the orchestrator
	// chose: every init re-derives the rest, so they can never keep ranges that
	// run.json no longer has.
	args := t.inventoryArgs(run)
	args.Clusters = oldArgs.Clusters
	if args.Clusters == nil {
		args.Clusters = []ClusterSpec{}
	}
	if !hadArgs || !reflect.DeepEqual(args, oldArgs) {
		if err := writeJSON(filepath.Join(dir, ArgsFile), args); err != nil {
			return res, err
		}
	}
	if hadArgs {
		data.Kept = append(data.Kept, ArgsFile)
		if !freshRun && !reflect.DeepEqual(args.Repos, oldArgs.Repos) {
			res.Diagnostics = append(res.Diagnostics, info(DiagPayloadDiffers, ArgsFile+" repos differed from run.json's ranges and were refreshed", ""))
		}
	} else {
		data.Created = append(data.Created, ArgsFile)
	}

	for _, pc := range payloadCopies {
		want, err := fs.ReadFile(t.Payload, pc[1])
		if err != nil {
			return res, fmt.Errorf("skill payload %s: %w", pc[1], err)
		}
		dst := filepath.Join(dir, pc[0])
		have, err := os.ReadFile(dst)
		switch {
		case errors.Is(err, os.ErrNotExist):
			mode := os.FileMode(0o644)
			if strings.HasPrefix(pc[1], "scripts/") {
				mode = 0o755
			}
			if err := os.WriteFile(dst, want, mode); err != nil {
				return res, err
			}
			data.Created = append(data.Created, pc[0])
		case err != nil:
			return res, err
		default:
			data.Kept = append(data.Kept, pc[0])
			if !bytes.Equal(have, want) {
				res.Diagnostics = append(res.Diagnostics, info(DiagPayloadDiffers, pc[0]+" differs from the skill's "+pc[1],
					"keep it if the run adapted it; delete it and re-run init to take the shipped one"))
			}
		}
	}
	for _, name := range ledgers {
		dst := filepath.Join(dir, name)
		if _, err := os.Stat(dst); err == nil {
			data.Kept = append(data.Kept, name)
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return res, err
		}
		out, err := t.render(name, view{C: t.Config, Run: run, Dir: dir, SimCap: t.SimCap})
		if err != nil {
			return res, err
		}
		if err := os.WriteFile(dst, out, 0o644); err != nil {
			return res, err
		}
		data.Created = append(data.Created, name)
	}
	res.Next = []string{
		"record the phase-0 authority answers in " + filepath.Join(dir, RunFile) + " (authority.merge, devices, deviceWalk, concurrency, finish)",
		"fill clusters in " + filepath.Join(dir, ArgsFile) + ", then Workflow({scriptPath: \"" + filepath.Join(dir, "inventory.workflow.js") + "\", args: <that file's JSON>})",
		"jq '.result' <workflow output file> > " + filepath.Join(dir, InventoryFile) + "   # never read the result into context",
	}
	return res, nil
}

// InventoryArgs is inventory-args.json: the Workflow args of the inventory phase.
type InventoryArgs struct {
	Project   string        `json:"project"`
	Devices   []string      `json:"devices"`
	Repos     []RepoRange   `json:"repos"`
	Clusters  []ClusterSpec `json:"clusters"`
	OutputDir string        `json:"outputDir"`
}

// ClusterSpec is one journey cluster the orchestrator asks a reader to cover.
type ClusterSpec struct {
	Name  string `json:"name"`
	Focus string `json:"focus"`
}

func (t *Tool) inventoryArgs(run Run) InventoryArgs {
	a := InventoryArgs{Project: t.Config.Vitrinka.Project, Repos: run.Ranges, Clusters: []ClusterSpec{}, OutputDir: run.Dir}
	for _, d := range t.Config.Devices.Matrix {
		a.Devices = append(a.Devices, d.ID)
	}
	return a
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// git runs git in dir and returns trimmed stdout; a failure carries stderr.
func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}
