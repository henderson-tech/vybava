package watch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/gitkit"
)

// Probe reads one kind of target. Probes shell out to the user's own CLIs
// (gh through gitkit, devbox, vitrinka, deployik) and never hold a token.
type Probe interface {
	Kind() string
	// Interval is the steady polling period; errors back off from it.
	Interval() time.Duration
	// Cost is the GitHub budget units one Observe spends (0 = free).
	Cost() int
	// Conditions are the kind's named conditions beside changed and
	// <field>=<value>.
	Conditions() map[string]Condition
	// Canonical turns a ref (and the subscriber's directory) into the
	// dedupe key's ref.
	Canonical(ctx context.Context, ref, dir string) (string, error)
	Observe(ctx context.Context, ref, dir string) (Observation, error)
}

// Runner runs a CLI in dir and returns its stdout; a failure carries the
// tail of its stderr.
type Runner func(ctx context.Context, dir, name string, args ...string) ([]byte, error)

// ExecRunner is the production Runner.
func ExecRunner(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, tail(stderr.String()))
	}
	return out, nil
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		s = "…" + s[len(s)-300:]
	}
	return s
}

// Verb is an in-process gitkit verb (gitkit.Verb's shape).
type Verb func(args []string, stdout, stderr io.Writer) int

// DefaultProbes are the production probes: gitkit's merge-precheck for PRs
// and the devbox, vitrinka and deployik CLIs.
func DefaultProbes(run Runner) []Probe {
	precheck, _ := gitkit.Native("merge-precheck")
	return []Probe{
		PRProbe{Run: run, Precheck: Verb(precheck)},
		DevboxProbe{Run: run},
		DevboxRunProbe{Run: run},
		VitrinkaProbe{Run: run},
		DeployikProbe{Run: run},
	}
}

func boolField(b bool) string { return strconv.FormatBool(b) }

// --- pr: gitkit merge-precheck ------------------------------------------

var (
	prBare  = regexp.MustCompile(`^#?([1-9]\d*)$`)
	prFull  = regexp.MustCompile(`^([^/\s#]+/[^/\s#]+)#([1-9]\d*)$`)
	prURL   = regexp.MustCompile(`^https://github\.com/([^/\s]+/[^/\s]+)/pull/([1-9]\d*)`)
	nwoLine = regexp.MustCompile(`^[^/\s]+/[^/\s]+$`)
)

// PRProbe reads a pull request through gitkit's merge-precheck — the same
// gates /prm merges on (CI rollup, review decision, required bot reviewers
// such as Eve, mergeability, draft) — so a watcher never disagrees with the
// merge loop. The subscriber's directory must be a checkout of the PR's repo.
type PRProbe struct {
	Run      Runner
	Precheck Verb
}

func (PRProbe) Kind() string            { return "pr" }
func (PRProbe) Interval() time.Duration { return 60 * time.Second }

// Cost: merge-precheck spends a repo view, a pr view and one GraphQL round
// (bot gate + merge rules) — budgeted as 5 to leave headroom.
func (PRProbe) Cost() int { return 5 }

func (PRProbe) Conditions() map[string]Condition {
	f := func(key, want string) func(Observation) bool {
		return func(o Observation) bool { return o.Fields[key] == want }
	}
	return map[string]Condition{
		"merged": {Doc: "the PR is merged", Holds: f("state", "MERGED")},
		"closed": {Doc: "the PR is merged or closed", Holds: func(o Observation) bool { return o.Fields["state"] != "OPEN" }},
		"checks-settled": {Doc: "CI is no longer pending (green, waived or red)", Holds: func(o Observation) bool {
			return o.Fields["ci"] == "green" || o.Fields["ci"] == "waived" || o.Fields["ci"] == "red"
		}},
		"checks-green": {Doc: "merge-precheck's ciOk (green, or waived by skip-ci)", Holds: func(o Observation) bool { return o.Fields["ci"] == "green" || o.Fields["ci"] == "waived" }},
		"checks-red":   {Doc: "a check failed", Holds: f("ci", "red")},
		"eve-approved": {Doc: "every required bot reviewer (Eve by default) approved", Holds: f("bots", "ok")},
		"ready":        {Doc: "every merge gate but the local worktree's cleanliness passes", Holds: f("failed", "")},
	}
}

func (p PRProbe) Canonical(ctx context.Context, ref, dir string) (string, error) {
	if m := prFull.FindStringSubmatch(ref); m != nil {
		return m[1] + "#" + m[2], nil
	}
	if m := prURL.FindStringSubmatch(ref); m != nil {
		return m[1] + "#" + m[2], nil
	}
	m := prBare.FindStringSubmatch(ref)
	if m == nil {
		return "", fmt.Errorf("pr ref %q is not <n>, owner/name#<n> or a PR URL", ref)
	}
	if dir == "" {
		return "", errors.New("pr:<n> needs --dir, a checkout of the PR's repository")
	}
	out, err := p.Run(ctx, dir, "gh", "repo", "view", "--json", "nameWithOwner")
	if err != nil {
		return "", err
	}
	var repo struct {
		NameWithOwner string `json:"nameWithOwner"`
	}
	if err := json.Unmarshal(out, &repo); err != nil || !nwoLine.MatchString(repo.NameWithOwner) {
		return "", fmt.Errorf("gh repo view in %s gave no nameWithOwner", dir)
	}
	return repo.NameWithOwner + "#" + m[1], nil
}

func (p PRProbe) Observe(ctx context.Context, ref, dir string) (Observation, error) {
	m := prFull.FindStringSubmatch(ref)
	if m == nil {
		return Observation{}, fmt.Errorf("pr ref %q is not canonical owner/name#<n>", ref)
	}
	if dir == "" {
		return Observation{}, fmt.Errorf("pr:%s has no checkout to anchor merge-precheck (subscribe with --dir)", ref)
	}
	if p.Precheck == nil {
		return Observation{}, errors.New("gitkit merge-precheck is not available in this build")
	}
	var stdout, stderr bytes.Buffer
	if code := p.Precheck([]string{m[2], "--repo=" + dir}, &stdout, &stderr); code != 0 {
		return Observation{}, fmt.Errorf("gitkit merge-precheck %s exited %d: %s", m[2], code, tail(stderr.String()))
	}
	var pc gitkit.Precheck
	if err := json.Unmarshal(stdout.Bytes(), &pc); err != nil {
		return Observation{}, fmt.Errorf("gitkit merge-precheck output: %w", err)
	}
	if got := pc.Owner + "/" + pc.Repo; got != m[1] {
		return Observation{}, fmt.Errorf("%s is a checkout of %s, not %s", dir, got, m[1])
	}
	return prObservation(pc), nil
}

// prObservation maps merge-precheck's gates onto stable fields; the
// decisions themselves (what green, ok or ready mean) are gitkit's.
func prObservation(pc gitkit.Precheck) Observation {
	var state string
	_ = json.Unmarshal(pc.Raw.State, &state)
	var review string
	_ = json.Unmarshal(pc.Raw.ReviewDecision, &review)
	var mergeable string
	_ = json.Unmarshal(pc.Raw.Mergeable, &mergeable)

	ci := "pending"
	switch {
	case pc.Gates.CIWaived:
		ci = "waived"
	case pc.Gates.CIOK:
		ci = "green"
	case pc.Checks == "FAILURE":
		ci = "red"
	case pc.Checks == "NONE":
		ci = "absent"
	}
	bots := "ok"
	if !pc.BotApproval.OK {
		bots = "pending: " + strings.Join(pc.BotApproval.Pending, ",")
	}
	// The local worktree's cleanliness is the watcher's checkout, not the PR.
	var failed []string
	for _, f := range pc.Gates.Failed {
		if f != "clean" {
			failed = append(failed, f)
		}
	}
	fields := map[string]string{
		"state":     state,
		"checks":    pc.Checks,
		"ci":        ci,
		"review":    review,
		"bots":      bots,
		"mergeable": mergeable,
		"draft":     boolField(!pc.Gates.DraftOK),
		"failed":    strings.Join(failed, ","),
	}
	summary := fmt.Sprintf("#%d %s · ci %s · bots %s", pc.PR, state, ci, bots)
	switch {
	case state != "OPEN":
		summary = fmt.Sprintf("#%d %s", pc.PR, state)
	case len(failed) == 0:
		summary += " · ready"
	default:
		summary += " · blocked: " + strings.Join(failed, ",")
	}
	return Observation{Fields: fields, Summary: summary}
}

// --- devbox: a box's state ------------------------------------------------

// devboxEnvelope is devbox's versioned --json envelope.
type devboxEnvelope struct {
	OK          bool            `json:"ok"`
	Data        json.RawMessage `json:"data"`
	Diagnostics []struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	} `json:"diagnostics"`
}

func decodeDevbox(out []byte, verb string) (json.RawMessage, error) {
	var env devboxEnvelope
	if err := json.Unmarshal(out, &env); err != nil {
		return nil, fmt.Errorf("devbox %s --json: %w", verb, err)
	}
	if !env.OK {
		var why []string
		for _, d := range env.Diagnostics {
			why = append(why, d.Code+": "+d.Detail)
		}
		return nil, fmt.Errorf("devbox %s: %s", verb, strings.Join(why, "; "))
	}
	return env.Data, nil
}

// DevboxProbe reads one box from `devbox boxes --json`.
type DevboxProbe struct{ Run Runner }

func (DevboxProbe) Kind() string            { return "devbox" }
func (DevboxProbe) Interval() time.Duration { return 60 * time.Second }
func (DevboxProbe) Cost() int               { return 0 }

func devboxUp(o Observation) bool {
	return o.Fields["state"] == "active" && o.Fields["reachable"] == "true" && o.Fields["health"] == "ok"
}

func (DevboxProbe) Conditions() map[string]Condition {
	return map[string]Condition{
		"up":   {Doc: "the box is active, reachable and healthy", Holds: devboxUp},
		"down": {Doc: "the box is not up", Holds: func(o Observation) bool { return !devboxUp(o) }},
	}
}

var boxName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

func (DevboxProbe) Canonical(_ context.Context, ref, _ string) (string, error) {
	if !boxName.MatchString(ref) {
		return "", fmt.Errorf("devbox ref %q is not a box name (devbox boxes)", ref)
	}
	return ref, nil
}

func (p DevboxProbe) Observe(ctx context.Context, ref, _ string) (Observation, error) {
	out, err := p.Run(ctx, "", "devbox", "boxes", "--json")
	if err != nil {
		return Observation{}, err
	}
	data, err := decodeDevbox(out, "boxes")
	if err != nil {
		return Observation{}, err
	}
	var boxes struct {
		Boxes []struct {
			Name      string `json:"name"`
			State     string `json:"state"`
			Reachable bool   `json:"reachable"`
			Health    any    `json:"health"`
		} `json:"boxes"`
	}
	if err := json.Unmarshal(data, &boxes); err != nil {
		return Observation{}, fmt.Errorf("devbox boxes --json: %w", err)
	}
	var names []string
	for _, b := range boxes.Boxes {
		names = append(names, b.Name)
		if b.Name != ref {
			continue
		}
		health, ok := b.Health.(string)
		if !ok {
			raw, _ := json.Marshal(b.Health)
			health = string(raw)
		}
		fields := map[string]string{"state": b.State, "reachable": boolField(b.Reachable), "health": health}
		obs := Observation{Fields: fields}
		word := "down"
		if devboxUp(obs) {
			word = "up"
		}
		obs.Summary = fmt.Sprintf("box %s %s (%s, health %s)", ref, word, b.State, health)
		return obs, nil
	}
	return Observation{}, fmt.Errorf("devbox has no box %q (boxes: %s)", ref, strings.Join(names, ", "))
}

// --- devbox-run: a workspace's runs ----------------------------------------

// DevboxRunProbe reads a workspace's queued and running runs from
// `devbox status <workspace> --json`; `idle` is "its runs finished".
type DevboxRunProbe struct{ Run Runner }

func (DevboxRunProbe) Kind() string            { return "devbox-run" }
func (DevboxRunProbe) Interval() time.Duration { return 30 * time.Second }
func (DevboxRunProbe) Cost() int               { return 0 }

func (DevboxRunProbe) Conditions() map[string]Condition {
	return map[string]Condition{
		"idle":    {Doc: "the workspace has no queued or running run", Holds: func(o Observation) bool { return o.Fields["runs"] == "0" }},
		"running": {Doc: "a run of the workspace is admitted and running", Holds: func(o Observation) bool { return o.Fields["running"] != "0" }},
	}
}

func (DevboxRunProbe) Canonical(_ context.Context, ref, _ string) (string, error) {
	if strings.ContainsAny(ref, " /\t") {
		return "", fmt.Errorf("devbox-run ref %q is not a workspace name (devbox ls)", ref)
	}
	return ref, nil
}

func (p DevboxRunProbe) Observe(ctx context.Context, ref, dir string) (Observation, error) {
	out, err := p.Run(ctx, dir, "devbox", "status", ref, "--json")
	if err != nil {
		return Observation{}, err
	}
	data, err := decodeDevbox(out, "status")
	if err != nil {
		return Observation{}, err
	}
	var status struct {
		Runs *[]struct {
			Run   string `json:"run"`
			State string `json:"state"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(data, &status); err != nil {
		return Observation{}, fmt.Errorf("devbox status --json: %w", err)
	}
	// An absent runs key is not "no runs": idle would fire on a shape change.
	if status.Runs == nil {
		return Observation{}, fmt.Errorf("devbox status %s --json carried no runs for the workspace", ref)
	}
	running, ids := 0, []string{}
	for _, r := range *status.Runs {
		if r.State == "running" {
			running++
		}
		ids = append(ids, r.Run)
	}
	fields := map[string]string{"runs": strconv.Itoa(len(ids)), "running": strconv.Itoa(running), "ids": strings.Join(ids, ",")}
	summary := fmt.Sprintf("workspace %s idle", ref)
	if len(ids) > 0 {
		summary = fmt.Sprintf("workspace %s: %d run(s), %d running", ref, len(ids), running)
	}
	return Observation{Fields: fields, Summary: summary}, nil
}

// --- vitrinka: a task's state ----------------------------------------------

// VitrinkaProbe reads a task through `vitrinka task get <ref> --json`.
type VitrinkaProbe struct{ Run Runner }

func (VitrinkaProbe) Kind() string            { return "vitrinka" }
func (VitrinkaProbe) Interval() time.Duration { return 60 * time.Second }
func (VitrinkaProbe) Cost() int               { return 0 }

func (VitrinkaProbe) Conditions() map[string]Condition {
	return map[string]Condition{
		"done": {Doc: "the task's status is done", Holds: func(o Observation) bool { return o.Fields["status"] == "done" }},
		"closed": {Doc: "the task is done or cancelled", Holds: func(o Observation) bool {
			s := o.Fields["status"]
			return s == "done" || s == "cancelled" || s == "canceled"
		}},
	}
}

var vitrinkaRef = regexp.MustCompile(`^([a-z0-9][a-z0-9-]*/)?[1-9]\d*$`)

func (VitrinkaProbe) Canonical(_ context.Context, ref, _ string) (string, error) {
	if !vitrinkaRef.MatchString(ref) {
		return "", fmt.Errorf("vitrinka ref %q is not <id> or <workspace>/<id>", ref)
	}
	return ref, nil
}

func (p VitrinkaProbe) Observe(ctx context.Context, ref, dir string) (Observation, error) {
	out, err := p.Run(ctx, dir, "vitrinka", "task", "get", ref, "--json")
	if err != nil {
		return Observation{}, err
	}
	var env struct {
		OK   bool `json:"ok"`
		Data struct {
			Task *struct {
				Title  string `json:"title"`
				State  string `json:"state"`
				Status string `json:"status"`
			} `json:"task"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &env); err != nil {
		return Observation{}, fmt.Errorf("vitrinka task get --json: %w", err)
	}
	if !env.OK || env.Data.Task == nil {
		return Observation{}, fmt.Errorf("vitrinka task get %s returned no task", ref)
	}
	t := env.Data.Task
	fields := map[string]string{"state": t.State, "status": t.Status}
	return Observation{Fields: fields, Summary: fmt.Sprintf("task %s %s (%s)", ref, t.Status, t.State)}, nil
}

// --- deployik: an environment's rollup ---------------------------------------

// DeployikProbe reads `deployik status <slug> --json` (run in the
// subscriber's directory, where .deployik.json names the server) and picks
// one environment's rollup_status (building|failed|live|none).
type DeployikProbe struct{ Run Runner }

func (DeployikProbe) Kind() string            { return "deployik" }
func (DeployikProbe) Interval() time.Duration { return 30 * time.Second }
func (DeployikProbe) Cost() int               { return 0 }

func (DeployikProbe) Conditions() map[string]Condition {
	f := func(want ...string) func(Observation) bool {
		return func(o Observation) bool {
			for _, w := range want {
				if o.Fields["rollup"] == w {
					return true
				}
			}
			return false
		}
	}
	return map[string]Condition{
		"live":     {Doc: "the environment's rollup is live", Holds: f("live")},
		"failed":   {Doc: "the environment's rollup is failed", Holds: f("failed")},
		"building": {Doc: "a deployment is building", Holds: f("building")},
		"settled":  {Doc: "live or failed — no deployment in flight", Holds: f("live", "failed")},
	}
}

var deployikRef = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*(/[A-Za-z0-9._-]+)?$`)

func (DeployikProbe) Canonical(_ context.Context, ref, _ string) (string, error) {
	if !deployikRef.MatchString(ref) {
		return "", fmt.Errorf("deployik ref %q is not <app-slug>[/<environment>]", ref)
	}
	if !strings.Contains(ref, "/") {
		ref += "/production"
	}
	return ref, nil
}

func (p DeployikProbe) Observe(ctx context.Context, ref, dir string) (Observation, error) {
	slug, env, _ := strings.Cut(ref, "/")
	out, err := p.Run(ctx, dir, "deployik", "status", slug, "--json")
	if err != nil {
		return Observation{}, err
	}
	var app struct {
		Slug         string `json:"slug"`
		Environments []struct {
			Name         string `json:"name"`
			Branch       string `json:"branch"`
			RollupStatus string `json:"rollup_status"`
		} `json:"environments"`
	}
	if err := json.Unmarshal(out, &app); err != nil {
		return Observation{}, fmt.Errorf("deployik status --json: %w", err)
	}
	var names []string
	for _, e := range app.Environments {
		names = append(names, e.Name)
		if e.Name == env {
			fields := map[string]string{"rollup": e.RollupStatus, "branch": e.Branch}
			return Observation{Fields: fields, Summary: fmt.Sprintf("%s/%s %s (%s)", slug, env, e.RollupStatus, e.Branch)}, nil
		}
	}
	return Observation{}, fmt.Errorf("deployik app %s has no environment %q (environments: %s)", slug, env, strings.Join(names, ", "))
}
