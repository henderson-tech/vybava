// Package skipci is the org standard for opting a pull request out of the
// machinery that would otherwise gate it: two labels and one job guard.
//
//   - `skip-ci`    — every job of every pull_request-triggered workflow carries
//     Guard as its `if:`; a labelled PR's runs skip (GitHub counts a skipped
//     required check as passing) and stay skipped on later pushes.
//   - `eve-ignore` — Eve's own skip label (review-webhook.ts, fixed name,
//     case-sensitive); Eve is a webhook app, so no workflow ever checks it —
//     the label only has to EXIST in the repo.
//
// The guard is job-level on purpose: a workflow-level skip (paths, branches,
// `[skip ci]`) leaves required checks pending forever, and a `labeled` trigger
// would re-run CI on every unrelated label. The label must sit on the PR
// BEFORE the push it should cover; a run that already started is cancelled
// by the caller (gitkit's admin-labels), never by the workflow.
//
// Check reads a repo's workflows and reports each job as guarded, missing or
// manual; Apply inserts the guard where it is missing and AND-wraps a
// single-line `if:` that lacks it. Multi-line conditions are reported, never
// rewritten. Docs: docs/skip-ci.md.
package skipci

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Label names — the contract prm, Eve and every workflow share.
const (
	LabelSkipCI    = "skip-ci"
	LabelEveIgnore = "eve-ignore"
)

// Guard is the job-level `if:` expression, verbatim as FixIt shipped it.
const Guard = "github.event_name != 'pull_request' || !contains(github.event.pull_request.labels.*.name, 'skip-ci')"

// guardCore is the part of Guard that proves a condition honours the label:
// a job may embed it in a larger expression (FixIt's run-testing does).
const guardCore = "!contains(github.event.pull_request.labels.*.name, 'skip-ci')"

// Label is one repo label as `gh label create` needs it.
type Label struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

// Labels are the two labels every repo carries, in creation order.
func Labels() []Label {
	return []Label{
		{Name: LabelSkipCI, Color: "ededed", Description: "skip CI on this PR — every pull_request job guards on it; merge is --admin"},
		{Name: LabelEveIgnore, Color: "ededed", Description: "skip eve's automatic PR review"},
	}
}

// LabelArgs is the `gh label create` argv for one label. Never --force: a
// label a human recoloured or re-described is theirs — callers create only
// what a repo lacks (repolicy audits presence, admin-labels lists first).
func LabelArgs(l Label, repo string) []string {
	argv := []string{"label", "create", l.Name, "--color", l.Color, "--description", l.Description}
	if repo != "" {
		argv = append(argv, "--repo", repo)
	}
	return argv
}

// State of one job with respect to the guard.
type State string

const (
	// Guarded: the job's `if:` honours the label.
	Guarded State = "guarded"
	// Missing: no `if:` — Apply inserts the guard.
	Missing State = "missing"
	// Wrap: a single-line `if:` without the guard — Apply AND-wraps it.
	Wrap State = "wrap"
	// Manual: a multi-line or otherwise unrewritable `if:` — a human edits it.
	Manual State = "manual"
	// Aggregate: an always()/cancelled()/failure()/success() job whose needs
	// are all guarded — a gate that must run after skipped lanes (FixIt pins
	// its required gates to `always()` so cancellation can never skip them,
	// and they pass as a no-op under the label). Reported, never rewritten,
	// not drift: its steps, not its condition, must read the label.
	Aggregate State = "aggregate"
)

// Job is one job of a pull_request-triggered workflow.
type Job struct {
	Name  string `json:"name"`
	Line  int    `json:"line"`
	State State  `json:"state"`
	If    string `json:"if,omitempty"`
	// Via names an indirect guard: "needs" when every job this one needs is
	// guarded, so it is skipped with them (or runs as a no-op aggregate under
	// always()); "event" when its condition pins a non-pull_request event.
	Via string `json:"via,omitempty"`
	// Applied is set by Apply on the jobs it rewrote.
	Applied bool `json:"applied,omitempty"`
	needs   []string
}

// Workflow is one file under .github/workflows.
type Workflow struct {
	Path string `json:"path"`
	// PullRequest: the workflow runs on pull_request (pull_request_target is
	// outside the standard — see triggersPullRequest),
	// so its jobs need the guard. Others are listed with no jobs.
	PullRequest bool  `json:"pullRequest"`
	Jobs        []Job `json:"jobs"`
	// Error names a file that could not be parsed; its jobs are unknown.
	Error string `json:"error,omitempty"`
}

// Report is the per-repo result of Check or Apply.
type Report struct {
	Repo      string     `json:"repo"`
	Workflows []Workflow `json:"workflows"`
	Guarded   int        `json:"guarded"`
	Missing   int        `json:"missing"`
	Wrap      int        `json:"wrap"`
	Manual    int        `json:"manual"`
	Aggregate int        `json:"aggregate"`
	Applied   int        `json:"applied"`
}

// Clean reports whether every pull_request job is guarded.
func (r Report) Clean() bool { return r.Missing == 0 && r.Wrap == 0 && r.Manual == 0 }

// ErrNoWorkflows: the repo has no .github/workflows directory.
var ErrNoWorkflows = errors.New("no .github/workflows directory")

// Check reads every workflow and classifies its jobs without writing.
func Check(repo string) (Report, error) { return run(repo, false) }

// Apply rewrites Missing and Wrap jobs in place and returns the post-state.
func Apply(repo string) (Report, error) { return run(repo, true) }

func run(repo string, write bool) (Report, error) {
	report := Report{Repo: repo}
	dir := filepath.Join(repo, ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return report, ErrNoWorkflows
	}
	if err != nil {
		return report, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if ext := filepath.Ext(e.Name()); ext == ".yml" || ext == ".yaml" {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		path := filepath.Join(dir, name)
		src, err := os.ReadFile(path)
		if err != nil {
			return report, err
		}
		wf := inspect(src)
		wf.Path = filepath.ToSlash(filepath.Join(".github", "workflows", name))
		if write && wf.Error == "" {
			out, applied := rewrite(src, wf.Jobs)
			if len(applied) > 0 {
				if err := os.WriteFile(path, out, 0o644); err != nil {
					return report, err
				}
				wf = inspect(out)
				wf.Path = filepath.ToSlash(filepath.Join(".github", "workflows", name))
				for i := range wf.Jobs {
					wf.Jobs[i].Applied = applied[wf.Jobs[i].Name]
				}
				report.Applied += len(applied)
			}
		}
		for _, j := range wf.Jobs {
			switch j.State {
			case Guarded:
				report.Guarded++
			case Missing:
				report.Missing++
			case Wrap:
				report.Wrap++
			case Manual:
				report.Manual++
			case Aggregate:
				report.Aggregate++
			}
		}
		report.Workflows = append(report.Workflows, wf)
	}
	return report, nil
}

// inspect parses one workflow and classifies its jobs.
func inspect(src []byte) Workflow {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return Workflow{Error: err.Error(), Jobs: []Job{}}
	}
	wf := Workflow{Jobs: []Job{}}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return wf
	}
	root := doc.Content[0]
	on := mapValue(root, "on")
	if on == nil {
		on = mapValue(root, "true") // `on` read through the YAML 1.1 bool table
	}
	wf.PullRequest = triggersPullRequest(on)
	if !wf.PullRequest {
		return wf
	}
	jobs := mapValue(root, "jobs")
	if jobs == nil || jobs.Kind != yaml.MappingNode {
		return wf
	}
	lines := bytes.Split(src, []byte("\n"))
	for i := 0; i+1 < len(jobs.Content); i += 2 {
		key, val := jobs.Content[i], jobs.Content[i+1]
		job := Job{Name: key.Value, Line: key.Line}
		if val.Kind != yaml.MappingNode || val.Style&yaml.FlowStyle != 0 {
			job.State = Manual
			wf.Jobs = append(wf.Jobs, job)
			continue
		}
		cond := mapValue(val, "if")
		job.needs = needsOf(mapValue(val, "needs"))
		switch {
		case cond == nil:
			job.State = Missing
		case skipsLabelledPR(cond.Value):
			job.State, job.If = Guarded, cond.Value
			if !strings.Contains(cond.Value, guardCore) {
				job.Via = "event"
			}
		case singleLine(lines, cond):
			job.State, job.If = Wrap, cond.Value
		default:
			job.State, job.If = Manual, cond.Value
		}
		wf.Jobs = append(wf.Jobs, job)
	}
	guardThroughNeeds(wf.Jobs)
	return wf
}

// guardThroughNeeds marks a job guarded when every job it needs is: a
// skipped dependency skips its dependants — UNLESS the dependant's own
// condition uses a status function (always(), cancelled(), failure(),
// success()), which is exactly how a job opts back in; such a job behind
// guarded needs is an Aggregate. Runs to a fixpoint, so a chain resolves
// in any order.
func guardThroughNeeds(jobs []Job) {
	guarded := map[string]bool{}
	for _, j := range jobs {
		guarded[j.Name] = j.State == Guarded
	}
	for changed := true; changed; {
		changed = false
		for i := range jobs {
			j := &jobs[i]
			if j.State == Guarded || j.State == Aggregate || len(j.needs) == 0 {
				continue
			}
			all := true
			for _, n := range j.needs {
				if !guarded[n] {
					all = false
					break
				}
			}
			if all && runsAfterSkip(j.If) {
				j.State, j.Via, guarded[j.Name], changed = Aggregate, "needs", true, true
			} else if all {
				j.State, j.Via, guarded[j.Name], changed = Guarded, "needs", true, true
			}
		}
	}
}

func needsOf(n *yaml.Node) []string {
	if n == nil {
		return nil
	}
	switch n.Kind {
	case yaml.ScalarNode:
		return []string{n.Value}
	case yaml.SequenceNode:
		out := make([]string, 0, len(n.Content))
		for _, c := range n.Content {
			out = append(out, c.Value)
		}
		return out
	}
	return nil
}

// runsAfterSkip: a status function in the condition opts the job back in
// when its needs were skipped, so `needs` no longer implies skipped.
func runsAfterSkip(cond string) bool {
	// success() included: an explicit status function disables the implicit
	// success() gate, so `success() || x` can run when its needs were skipped.
	for _, fn := range []string{"always()", "cancelled()", "failure()", "success()"} {
		if strings.Contains(cond, fn) {
			return true
		}
	}
	return false
}

// skipsLabelledPR evaluates the condition for a pull_request run whose PR
// carries `skip-ci` and reports whether it is provably false. The evaluator
// is three-valued: `github.event_name` comparisons and the label test are
// known, everything else (an input, a needs output, always()) is unknown,
// and only a definite false counts — a substring is never proof.
func skipsLabelledPR(cond string) bool {
	c := strings.TrimSpace(cond)
	if inner, ok := strings.CutPrefix(c, "${{"); ok {
		c = strings.TrimSuffix(inner, "}}")
	}
	return evalOr(c) == tvFalse
}

type tv int

const (
	tvUnknown tv = iota
	tvTrue
	tvFalse
)

func evalOr(s string) tv {
	parts := splitTop(s, "||")
	if len(parts) == 1 {
		return evalAnd(s)
	}
	out := tvFalse
	for _, p := range parts {
		switch evalAnd(p) {
		case tvTrue:
			return tvTrue
		case tvUnknown:
			out = tvUnknown
		}
	}
	return out
}

func evalAnd(s string) tv {
	parts := splitTop(s, "&&")
	if len(parts) == 1 {
		return evalUnary(s)
	}
	out := tvTrue
	for _, p := range parts {
		switch evalUnary(p) {
		case tvFalse:
			return tvFalse
		case tvUnknown:
			out = tvUnknown
		}
	}
	return out
}

func evalUnary(s string) tv {
	s = strings.TrimSpace(s)
	if rest, ok := strings.CutPrefix(s, "!"); ok {
		switch evalUnary(rest) {
		case tvTrue:
			return tvFalse
		case tvFalse:
			return tvTrue
		}
		return tvUnknown
	}
	if strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") && balanced(s[1:len(s)-1]) {
		return evalOr(s[1 : len(s)-1])
	}
	return evalAtom(s)
}

// evalAtom knows the two facts about the run: the event is pull_request
// and the label is present. Whitespace inside the atom is normalized.
func evalAtom(s string) tv {
	a := strings.Join(strings.Fields(s), " ")
	switch {
	// Actions compares strings case-insensitively: 'Pull_Request' matches.
	case eventCmp.MatchString(a):
		m := eventCmp.FindStringSubmatch(a)
		equal := strings.EqualFold(m[2], "pull_request")
		if (m[1] == "==") == equal {
			return tvTrue
		}
		return tvFalse
	case a == strings.Join(strings.Fields(guardCore[1:]), " "):
		return tvTrue // contains(labels, 'skip-ci')
	}
	return tvUnknown
}

// eventCmp is `github.event_name == 'x'` / `!= 'x'`, the one fact the
// evaluator knows about the run.
var eventCmp = regexp.MustCompile(`^github\.event_name (==|!=) '([^']*)'$`)

// balanced: the parentheses of s close inside s (so `(a) || (b)` is not one
// parenthesised group).
func balanced(s string) bool {
	depth, quoted := 0, false
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '\'':
			quoted = !quoted
		case quoted:
		case s[i] == '(':
			depth++
		case s[i] == ')':
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return depth == 0
}

// splitTop splits on op outside parentheses and single quotes.
func splitTop(s, op string) []string {
	var parts []string
	depth, quoted, start := 0, false, 0
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '\'':
			quoted = !quoted
		case quoted:
		case s[i] == '(':
			depth++
		case s[i] == ')':
			depth--
		case depth == 0 && strings.HasPrefix(s[i:], op):
			parts = append(parts, s[start:i])
			i += len(op) - 1
			start = i + 1
		}
	}
	return append(parts, s[start:])
}

// singleLine: the `if:` value sits entirely on the key's line, so a line
// rewrite reproduces it. A block scalar, a multi-line plain scalar or a
// folded flow scalar fails the round-trip and stays Manual.
func singleLine(lines [][]byte, cond *yaml.Node) bool {
	if cond.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 || cond.Line < 1 || cond.Line > len(lines) {
		return false
	}
	_, rest, ok := bytes.Cut(lines[cond.Line-1], []byte("if:"))
	if !ok {
		return false
	}
	var v string
	if err := yaml.Unmarshal(rest, &v); err != nil {
		return false
	}
	return v == cond.Value
}

func triggersPullRequest(on *yaml.Node) bool {
	if on == nil {
		return false
	}
	// pull_request_target is deliberately NOT a pull_request workflow here:
	// its runs carry the base branch's permissions for automation (labelers,
	// assignment), the guard's `github.event_name != 'pull_request'` would
	// let them through, and skipping privileged automation is not what a
	// `skip-ci` label asks for.
	is := func(s string) bool { return s == "pull_request" }
	switch on.Kind {
	case yaml.ScalarNode:
		return is(on.Value)
	case yaml.SequenceNode:
		for _, n := range on.Content {
			if is(n.Value) {
				return true
			}
		}
	case yaml.MappingNode:
		for i := 0; i < len(on.Content); i += 2 {
			if is(on.Content[i].Value) {
				return true
			}
		}
	}
	return false
}

func mapValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// rewrite applies the guard to Missing and Wrap jobs by line edits, so the
// file's comments, ordering and quoting elsewhere survive untouched. Edits
// run bottom-up so earlier line numbers stay valid.
func rewrite(src []byte, jobs []Job) ([]byte, map[string]bool) {
	lines := strings.Split(string(src), "\n")
	edits := make([]Job, 0, len(jobs))
	for _, j := range jobs {
		if j.State == Missing || j.State == Wrap {
			edits = append(edits, j)
		}
	}
	sort.Slice(edits, func(a, b int) bool { return edits[a].Line > edits[b].Line })
	applied := map[string]bool{}
	for _, j := range edits {
		keyLine := j.Line - 1 // 0-based index of `  <job>:`
		if keyLine < 0 || keyLine >= len(lines) {
			continue
		}
		indent := bodyIndent(lines, keyLine)
		switch j.State {
		case Missing:
			line := indent + "if: " + Guard
			lines = append(lines[:keyLine+1], append([]string{line}, lines[keyLine+1:]...)...)
			applied[j.Name] = true
		case Wrap:
			idx := findIfLine(lines, keyLine, indent)
			if idx < 0 {
				continue
			}
			lines[idx] = indent + "if: " + renderIf(wrap(j.If)) + trailingComment(lines[idx])
			applied[j.Name] = true
		}
	}
	return []byte(strings.Join(lines, "\n")), applied
}

// bodyIndent is the indentation of the job's own keys: the first non-blank,
// non-comment line after the job key.
func bodyIndent(lines []string, keyLine int) string {
	for i := keyLine + 1; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		return lines[i][:len(lines[i])-len(strings.TrimLeft(lines[i], " "))]
	}
	return strings.Repeat(" ", jobKeyIndent(lines[keyLine])+2)
}

func jobKeyIndent(line string) int { return len(line) - len(strings.TrimLeft(line, " ")) }

// findIfLine locates the job's own `if:` line: at the body indent, before
// the next key at job-key indent or shallower.
func findIfLine(lines []string, keyLine int, indent string) int {
	jobIndent := jobKeyIndent(lines[keyLine])
	for i := keyLine + 1; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if jobKeyIndent(lines[i]) <= jobIndent {
			return -1
		}
		if strings.HasPrefix(lines[i], indent+"if:") && jobKeyIndent(lines[i]) == len(indent) {
			return i
		}
	}
	return -1
}

// trailingComment returns the ` # note` that ends an `if:` line, outside
// quotes, so a wrap keeps it; "" when there is none.
func trailingComment(line string) string {
	quote := byte(0)
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quote != 0:
			if c == '\\' && quote == '"' {
				i++ // an escaped char inside double quotes never closes them
			} else if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '#' && i > 0 && (line[i-1] == ' ' || line[i-1] == '\t'):
			return " " + line[i:]
		}
	}
	return ""
}

// wrap ANDs the guard onto an existing expression, keeping a `${{ }}` shell
// where one was used.
func wrap(existing string) string {
	e := strings.TrimSpace(existing)
	if inner, ok := strings.CutPrefix(e, "${{"); ok {
		if inner, ok = strings.CutSuffix(inner, "}}"); ok {
			return "${{ (" + strings.TrimSpace(inner) + ") && (" + Guard + ") }}"
		}
	}
	return "(" + e + ") && (" + Guard + ")"
}

// renderIf emits the expression as a YAML plain scalar when that is
// unambiguous, else double-quoted. Compositions begin with `(` or `$`, so
// the indicators that would need quoting (`!`, `*`, `&`…) never lead.
func renderIf(expr string) string {
	if strings.ContainsAny(expr, "\"\n") || strings.Contains(expr, ": ") || strings.Contains(expr, " #") {
		return fmt.Sprintf("%q", expr)
	}
	return expr
}
