package readiness

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Config is the `readiness` section of vybava.config.ts: what a
// release-readiness run needs to know about one project. Everything specific
// to one run — the epic, the authority answers, the lanes — lives in the run
// directory (run.json, lanes.json), never here. Command strings carry
// {token} placeholders; Validate rejects tokens a field does not define.
type Config struct {
	Vitrinka Vitrinka `json:"vitrinka"`
	// Exports names the ~/Exports/<Exports> folder a run directory defaults
	// to (release-readiness-<date> inside it).
	Exports string `json:"exports,omitempty"`
	Repos   []Repo `json:"repos"`
	Lane    Lane   `json:"lane"`
	// Tests are the suites lanes extend and run through Lane.Heavy.
	Tests   []Suite `json:"tests,omitempty"`
	Devices Devices `json:"devices"`
	Merge   Merge   `json:"merge"`
	Final   Final   `json:"final,omitempty"`
	// Rules are project traps rendered verbatim into lane-rules.md.
	Rules []string `json:"rules,omitempty"`
	// Plumbing lists shared prerequisites known to be missing; phase 3
	// builds them as small PRs before any lane needs them.
	Plumbing []string `json:"plumbing,omitempty"`
}

// Vitrinka is the task engine and board target of a run.
type Vitrinka struct {
	Workspace string `json:"workspace"`
	Project   string `json:"project"`
}

// Repo is one repository that ships in the release.
type Repo struct {
	// ID prefixes refs in inventories and lane bodies: app#123, app@abc1234.
	ID string `json:"id"`
	// Path is relative to the directory holding vybava.config.ts.
	Path   string `json:"path"`
	GitHub string `json:"github"`
	// Remote defaults to origin.
	Remote      string     `json:"remote,omitempty"`
	Production  Production `json:"production"`
	Integration string     `json:"integration"`
	// Worktree creates a lane checkout of this repo when a lane touches it; {slug}
	// {path} (the repo's resolved main clone). Required for every repo but the
	// first, whose checkout is Lane.Worktree.
	Worktree string `json:"worktree,omitempty"`
}

// Production names what production runs: the newest tag matching Tag that
// is reachable from the integration branch (minus Exclude), or a branch.
type Production struct {
	Tag     string `json:"tag,omitempty"`
	Exclude string `json:"exclude,omitempty"`
	Branch  string `json:"branch,omitempty"`
}

// Lane is how one lane gets its checkout and dev stack.
type Lane struct {
	// Worktree creates the lane checkout; {slug}.
	Worktree string `json:"worktree"`
	// Workspace names the lane stack's {ws}; {slug}. Briefs render it filled in.
	Workspace string `json:"workspace,omitempty"`
	DevEnv    DevEnv `json:"devEnv"`
	// Heavy wraps every heavy job (suite, build, browser batch); {cmd} {ws}.
	Heavy string `json:"heavy"`
}

// DevEnv holds the lane stack verbs; {ws} is the lane's workspace.
type DevEnv struct {
	Up   string `json:"up"`
	Hold string `json:"hold"`
	Park string `json:"park"`
	// URL prints an app's address on the stack; {ws} {app}.
	URL string `json:"url,omitempty"`
	// Checks verify the stack topology once after Up, before any run.
	Checks []string `json:"checks,omitempty"`
}

// Suite is one test suite lanes extend; {pattern} narrows a run.
type Suite struct {
	Name      string `json:"name"`
	Framework string `json:"framework,omitempty"`
	Cmd       string `json:"cmd"`
}

// Devices is the release device matrix and who drives it.
type Devices struct {
	// Runner is "lane" (each lane runs its devices) or "device-runner" (one
	// orchestrator-owned agent owns every simulator and emulator).
	Runner string `json:"runner"`
	// Build is "release" or "dev-client" — what the device runs.
	Build string `json:"build"`
	// Concurrent is the most devices driven at once (default 2, the
	// claude-guards simCap default).
	Concurrent int `json:"concurrent,omitempty"`
	// OneRunPerLane means every run resets the lane's shared test data: one lane
	// never has two device runs (or a device run beside a web run) at once, and
	// concurrency comes from serving several lanes.
	OneRunPerLane bool     `json:"oneRunPerLane,omitempty"`
	Matrix        []Device `json:"matrix"`
	// Realtime runs multi-actor specs with every role on a different platform.
	Realtime *Realtime `json:"realtime,omitempty"`
}

// Device is one entry of the matrix.
type Device struct {
	ID        string `json:"id"`
	Platform  string `json:"platform"`
	Framework string `json:"framework"`
	Host      string `json:"host"`
	Name      string `json:"name"`
	// Width is the screenshot width a board card must show for this device.
	Width int `json:"width,omitempty"`
	// Build produces the app a release run installs; {commit} {apiUrl} {out} {worktree}.
	Build string `json:"build,omitempty"`
	// Run executes specs; {specs} {apiUrl} {app} {runDir} {udid} {worktree} {grep}, and
	// {port} {driverPort}: the Appium server and driver ports the runner assigns so
	// concurrent runs never share one.
	Run string `json:"run"`
}

// Realtime describes the multi-actor specs and how roles pair across platforms.
type Realtime struct {
	Specs []string `json:"specs"`
	Roles []string `json:"roles"`
	// Directions is "both" (every role on every platform once) or "one".
	Directions string `json:"directions"`
	// Run executes one spec; {spec} {apiUrl} {runDir} {worktree}, {iosApp}
	// {androidApp} {port}, and per role {<role>} (its platform) and {<role>Udid}.
	Run string `json:"run"`
}

// Merge is how lane PRs land.
type Merge struct {
	Command string `json:"command"`
	// Order lists repo IDs, first to merge first.
	Order []string `json:"order,omitempty"`
}

// Final is the closing phase's project hooks.
type Final struct {
	// Checks are read-only release gates run on the merged integration branch.
	Checks []string `json:"checks,omitempty"`
	// Handoff is the command that ships the release; the run never runs it.
	Handoff string `json:"handoff,omitempty"`
}

// Enumerations the section accepts.
var (
	runnerKinds = []string{"lane", "device-runner"}
	buildKinds  = []string{"release", "dev-client"}
	platforms   = []string{"ios", "android", "web"}
	frameworks  = []string{"appium", "playwright"}
	hosts       = []string{"mac", "devbox", "ci"}
	directions  = []string{"both", "one"}
	idPattern   = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	tokenRe     = regexp.MustCompile(`\{([A-Za-z]+)\}`)
)

// DefaultConcurrent mirrors claude-guards' default simCap.
const DefaultConcurrent = 2

// RemoteName returns the repo's remote, defaulting to origin.
func (r Repo) RemoteName() string {
	if r.Remote == "" {
		return "origin"
	}
	return r.Remote
}

// ConcurrentDevices returns the configured device budget or its default.
func (d Devices) ConcurrentDevices() int {
	if d.Concurrent > 0 {
		return d.Concurrent
	}
	return DefaultConcurrent
}

// Validate reports every problem at once, so one edit fixes the section.
func (c Config) Validate() []string {
	var errs []string
	add := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }
	tokens := func(field, value string, allowed ...string) {
		for _, name := range Tokens(value) {
			if !slices.Contains(allowed, name) {
				add("%s: unknown token {%s} (allowed: %s)", field, name, braced(allowed))
			}
		}
	}
	required := func(field, value string) {
		if strings.TrimSpace(value) == "" {
			add("%s is required", field)
		}
	}
	oneOf := func(field, value string, allowed []string) {
		if !slices.Contains(allowed, value) {
			add("%s: %q is not one of %s", field, value, strings.Join(allowed, " | "))
		}
	}

	required("vitrinka.workspace", c.Vitrinka.Workspace)
	required("vitrinka.project", c.Vitrinka.Project)

	if len(c.Repos) == 0 {
		add("repos: at least one repo is required")
	}
	repoIDs := map[string]bool{}
	for i, r := range c.Repos {
		f := fmt.Sprintf("repos[%d]", i)
		if !idPattern.MatchString(r.ID) {
			add("%s.id: %q must be lowercase kebab-case", f, r.ID)
		}
		if repoIDs[r.ID] {
			add("%s.id: %q is duplicated", f, r.ID)
		}
		repoIDs[r.ID] = true
		required(f+".path", r.Path)
		required(f+".github", r.GitHub)
		required(f+".integration", r.Integration)
		tokens(f+".worktree", r.Worktree, "slug", "path")
		if i > 0 && strings.TrimSpace(r.Worktree) == "" {
			add("%s.worktree is required: a lane touching %s needs its own checkout command", f, r.ID)
		}
		switch {
		case r.Production.Tag != "" && r.Production.Branch != "":
			add("%s.production: set tag or branch, not both", f)
		case r.Production.Tag == "" && r.Production.Branch == "":
			add("%s.production: set tag (a glob) or branch", f)
		case r.Production.Exclude != "" && r.Production.Tag == "":
			add("%s.production.exclude only applies to a tag", f)
		}
	}

	required("lane.worktree", c.Lane.Worktree)
	tokens("lane.worktree", c.Lane.Worktree, "slug")
	tokens("lane.workspace", c.Lane.Workspace, "slug")
	required("lane.heavy", c.Lane.Heavy)
	tokens("lane.heavy", c.Lane.Heavy, "cmd", "ws")
	if !strings.Contains(c.Lane.Heavy, "{cmd}") {
		add("lane.heavy must contain {cmd}")
	}
	for name, v := range map[string]string{"up": c.Lane.DevEnv.Up, "hold": c.Lane.DevEnv.Hold, "park": c.Lane.DevEnv.Park} {
		required("lane.devEnv."+name, v)
		tokens("lane.devEnv."+name, v, "ws", "slug")
	}
	tokens("lane.devEnv.url", c.Lane.DevEnv.URL, "ws", "app")
	for i, check := range c.Lane.DevEnv.Checks {
		tokens(fmt.Sprintf("lane.devEnv.checks[%d]", i), check, "ws", "slug")
	}

	for i, s := range c.Tests {
		f := fmt.Sprintf("tests[%d]", i)
		required(f+".name", s.Name)
		required(f+".cmd", s.Cmd)
		tokens(f+".cmd", s.Cmd, "pattern")
	}

	d := c.Devices
	oneOf("devices.runner", d.Runner, runnerKinds)
	oneOf("devices.build", d.Build, buildKinds)
	if d.Concurrent < 0 {
		add("devices.concurrent must be positive")
	}
	if len(d.Matrix) == 0 {
		add("devices.matrix: at least one device is required")
	}
	deviceIDs := map[string]bool{}
	macDevices := 0
	for i, dev := range d.Matrix {
		f := fmt.Sprintf("devices.matrix[%d]", i)
		if !idPattern.MatchString(dev.ID) {
			add("%s.id: %q must be lowercase kebab-case", f, dev.ID)
		}
		if deviceIDs[dev.ID] {
			add("%s.id: %q is duplicated", f, dev.ID)
		}
		deviceIDs[dev.ID] = true
		oneOf(f+".platform", dev.Platform, platforms)
		oneOf(f+".framework", dev.Framework, frameworks)
		oneOf(f+".host", dev.Host, hosts)
		required(f+".name", dev.Name)
		required(f+".run", dev.Run)
		tokens(f+".build", dev.Build, "commit", "apiUrl", "out", "worktree")
		tokens(f+".run", dev.Run, "specs", "apiUrl", "app", "runDir", "udid", "worktree", "grep", "port", "driverPort")
		if dev.Width < 0 {
			add("%s.width must be positive", f)
		}
		if dev.Host == "mac" && dev.Platform != "web" {
			macDevices++
		}
	}
	if d.Runner == "device-runner" && macDevices == 0 {
		add("devices.runner is device-runner but no matrix device is a simulator/emulator on the mac")
	}
	if d.Runner == "device-runner" && macDevices > 0 && d.ConcurrentDevices() < macDevices {
		add("devices.concurrent (%d) is below one full set of mac devices (%d)", d.ConcurrentDevices(), macDevices)
	}
	if rt := d.Realtime; rt != nil {
		if len(rt.Specs) == 0 {
			add("devices.realtime.specs: at least one spec glob is required")
		}
		if len(rt.Roles) < 2 {
			add("devices.realtime.roles: a multi-actor spec needs at least two roles")
		}
		rtTokens := []string{"spec", "apiUrl", "runDir", "worktree", "iosApp", "androidApp", "port"}
		roleTokens := slices.Clone(rtTokens)
		for i, role := range rt.Roles {
			if !idPattern.MatchString(role) || strings.Contains(role, "-") {
				add("devices.realtime.roles[%d]: %q must be a lowercase word (it doubles as the {%s} token)", i, role, role)
			}
			if slices.Contains(rtTokens, role) {
				add("devices.realtime.roles[%d]: %q collides with the built-in {%s} token", i, role, role)
			}
			roleTokens = append(roleTokens, role, role+"Udid")
		}
		oneOf("devices.realtime.directions", rt.Directions, directions)
		required("devices.realtime.run", rt.Run)
		tokens("devices.realtime.run", rt.Run, roleTokens...)
	}

	required("merge.command", c.Merge.Command)
	seen := map[string]bool{}
	for i, id := range c.Merge.Order {
		if !repoIDs[id] {
			add("merge.order[%d]: %q is not a repo id", i, id)
		}
		if seen[id] {
			add("merge.order[%d]: %q is listed twice", i, id)
		}
		seen[id] = true
	}
	slices.Sort(errs)
	return errs
}

func braced(tokens []string) string {
	out := make([]string, len(tokens))
	for i, t := range tokens {
		out[i] = "{" + t + "}"
	}
	return strings.Join(out, " ")
}

// Tokens lists the {token} placeholders of a command string. A shell
// parameter expansion (${VAR}) is not a token.
func Tokens(value string) []string {
	var out []string
	for _, m := range tokenRe.FindAllStringSubmatchIndex(value, -1) {
		if m[0] > 0 && value[m[0]-1] == '$' {
			continue
		}
		out = append(out, value[m[2]:m[3]])
	}
	return out
}
