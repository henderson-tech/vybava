package polishkit

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// PlanOptions are the plan verb's flags.
type PlanOptions struct {
	Base      string
	Targets   []string
	Intensity string
	// Findings is passed through: a vitrinka board slug, a PR URL or a file
	// the skill reads; the applet only records it.
	Findings string
}

// PlanTarget is one inferred target with the files that put it there.
type PlanTarget struct {
	ID    Target   `json:"id"`
	Files []string `json:"files"`
	// Reason: "flag" (--target), "cwd" (invoked inside its glob root), "diff".
	Reason string `json:"reason"`
}

// PlanData is what plan reports and what run init snapshots.
type PlanData struct {
	Targets        []PlanTarget `json:"targets"`
	Intensity      string       `json:"intensity"`
	Base           string       `json:"base"`
	Lanes          []string     `json:"lanes"`
	ScreensTouched []string     `json:"screensTouched"`
	Findings       string       `json:"findings,omitempty"`
	// Changed is every changed path (the inference input, kept for the ledger).
	Changed []string `json:"changed"`
}

// TargetIDs lists the plan's targets in order.
func (p PlanData) TargetIDs() []Target {
	ids := make([]Target, len(p.Targets))
	for i, t := range p.Targets {
		ids[i] = t.ID
	}
	return ids
}

// ParseTargets validates a comma list of targets.
func ParseTargets(list []string) ([]Target, error) {
	var out []Target
	for _, raw := range list {
		t := Target(strings.TrimSpace(raw))
		if t == "" {
			continue
		}
		if !slices.Contains(Targets, t) {
			return nil, diag(DiagUnknownTarget, fmt.Sprintf("target %q is not app, ui or api", t), "polish-kit plan --target app --json")
		}
		if !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return out, nil
}

// ParseIntensity validates the intensity axis (default when empty).
func ParseIntensity(s string) (string, error) {
	if s == "" {
		return "default", nil
	}
	if !slices.Contains(Intensities, s) {
		return "", diag(DiagUsage, fmt.Sprintf("intensity %q is not quick, default or full", s), "polish-kit plan --intensity default --json")
	}
	return s, nil
}

// SplitList splits a comma list, trimming and dropping empties.
func SplitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ChangedFiles lists the paths that differ between the base and HEAD, and
// the base actually used: the configured one when it resolves, else the
// remote's default branch, else main.
func (t *Tool) ChangedFiles(ctx context.Context, base string) ([]string, string, error) {
	if base == "" {
		base = t.Config.Base
	}
	resolved, err := t.resolveBase(ctx, base)
	if err != nil {
		return nil, base, err
	}
	out, err := t.run(ctx, 30*time.Second, "git", "diff", "--name-only", resolved+"...HEAD")
	if err != nil {
		return nil, resolved, err
	}
	if out.Code != 0 {
		return nil, resolved, fmt.Errorf("git diff --name-only %s...HEAD: %s", resolved, stderrTail(out))
	}
	var files []string
	for _, line := range strings.Split(out.Stdout, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			files = append(files, line)
		}
	}
	return files, resolved, nil
}

func (t *Tool) resolveBase(ctx context.Context, base string) (string, error) {
	ok, err := t.refExists(ctx, base)
	if err != nil || ok {
		return base, err
	}
	// origin/HEAD names the remote's default branch when the clone recorded it.
	out, err := t.run(ctx, 10*time.Second, "git", "symbolic-ref", "--short", "refs/remotes/origin/HEAD")
	if err == nil && out.Code == 0 {
		if head := strings.TrimSpace(out.Stdout); head != "" {
			if ok, err := t.refExists(ctx, head); err == nil && ok {
				return head, nil
			}
		}
	}
	for _, candidate := range []string{"main", "master"} {
		if ok, err := t.refExists(ctx, candidate); err == nil && ok {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("base %q does not resolve and the repo has no origin/HEAD, main or master", base)
}

func (t *Tool) refExists(ctx context.Context, ref string) (bool, error) {
	out, err := t.run(ctx, 10*time.Second, "git", "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		return false, err
	}
	return out.Code == 0, nil
}

// Plan infers targets from the diff, the cwd and --target.
func (t *Tool) Plan(ctx context.Context, opts PlanOptions) (Result, error) {
	explicit, err := ParseTargets(opts.Targets)
	if err != nil {
		return Result{}, err
	}
	intensity, err := ParseIntensity(opts.Intensity)
	if err != nil {
		return Result{}, err
	}
	changed, base, err := t.ChangedFiles(ctx, opts.Base)
	if err != nil {
		return Result{}, err
	}
	data := t.Infer(changed, explicit)
	data.Base = base
	data.Intensity = intensity
	data.Findings = opts.Findings
	if len(data.Targets) == 0 {
		return Result{Data: data}, diag(DiagNoChanges, fmt.Sprintf("no changed file under a target glob between %s and HEAD, and the cwd is outside every target", base), "polish-kit plan --target app --json")
	}
	res := Result{Data: data, Lines: planLines(data)}
	targets := make([]string, len(data.Targets))
	for i, pt := range data.Targets {
		targets[i] = string(pt.ID)
	}
	res.Next = []string{
		"polish-kit lanes --target " + strings.Join(targets, ",") + " --json",
		fmt.Sprintf("polish-kit run init --pass %d --target %s --intensity %s --json", t.nextPass(), strings.Join(targets, ","), intensity),
	}
	return res, nil
}

// Infer is the pure inference: explicit targets first, then the cwd's, then
// the diff's by weight (changed files per target, ties in fixed order).
func (t *Tool) Infer(changed []string, explicit []Target) PlanData {
	byTarget := map[Target][]string{}
	for _, f := range changed {
		if target, ok := t.Config.TargetOf(f); ok {
			byTarget[target] = append(byTarget[target], f)
		}
	}
	var ordered []PlanTarget
	seen := map[Target]bool{}
	push := func(id Target, reason string) {
		if seen[id] {
			return
		}
		seen[id] = true
		files := byTarget[id]
		if files == nil {
			files = []string{}
		}
		ordered = append(ordered, PlanTarget{ID: id, Files: files, Reason: reason})
	}
	for _, id := range explicit {
		push(id, "flag")
	}
	if rel, err := filepath.Rel(t.Root, t.Cwd); err == nil {
		if target, ok := t.Config.TargetOfDir(filepath.ToSlash(rel)); ok {
			push(target, "cwd")
		}
	}
	weighted := make([]Target, 0, len(byTarget))
	for target := range byTarget {
		weighted = append(weighted, target)
	}
	sort.SliceStable(weighted, func(i, j int) bool {
		wi, wj := len(byTarget[weighted[i]]), len(byTarget[weighted[j]])
		if wi != wj {
			return wi > wj
		}
		return slices.Index(Targets, weighted[i]) < slices.Index(Targets, weighted[j])
	})
	for _, target := range weighted {
		push(target, "diff")
	}
	data := PlanData{Targets: ordered, Changed: changed}
	if data.Changed == nil {
		data.Changed = []string{}
	}
	if len(ordered) == 0 {
		data.Lanes, data.ScreensTouched = []string{}, []string{}
		return data
	}
	data.Lanes = t.Config.LaneIDs(data.TargetIDs())
	if data.Lanes == nil {
		data.Lanes = []string{}
	}
	data.ScreensTouched = t.screensTouched(data.TargetIDs(), changed)
	return data
}

// screensTouched: per target, the screens whose area or url's last path
// segment names a component of a changed path; when none derives, every
// screen of the target.
func (t *Tool) screensTouched(targets []Target, changed []string) []string {
	components := map[string]bool{}
	for _, f := range changed {
		for _, c := range strings.Split(f, "/") {
			c = strings.ToLower(c)
			components[c] = true
			if ext := path.Ext(c); ext != "" {
				components[strings.TrimSuffix(c, ext)] = true
			}
		}
	}
	out := []string{}
	for _, target := range targets {
		var all, touched []string
		for _, s := range t.Config.Screens {
			if s.Target != target {
				continue
			}
			all = append(all, s.ID)
			if s.Area != "" && components[strings.ToLower(s.Area)] {
				touched = append(touched, s.ID)
				continue
			}
			if seg := lastSegment(s.URL); seg != "" && components[seg] {
				touched = append(touched, s.ID)
			}
		}
		if len(touched) == 0 {
			touched = all
		}
		out = append(out, touched...)
	}
	return out
}

// lastSegment is the last path segment of a URL or path, lower-cased.
func lastSegment(u string) string {
	s := u
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	s = strings.Trim(s, "/")
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	return strings.ToLower(s)
}

func planLines(d PlanData) []string {
	lines := []string{fmt.Sprintf("intensity %s  base %s", d.Intensity, d.Base)}
	rows := [][]string{{"TARGET", "FILES", "REASON"}}
	for _, pt := range d.Targets {
		rows = append(rows, []string{string(pt.ID), itoa(len(pt.Files)), pt.Reason})
	}
	lines = append(lines, table(rows)...)
	lines = append(lines, "lanes: "+strings.Join(d.Lanes, ", "), "screens: "+strings.Join(d.ScreensTouched, ", "))
	return lines
}
