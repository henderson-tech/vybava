// Package repolicy converges GitHub repository settings to a declared policy.
//
// GitHub has no organization-level default for repository settings — only the
// default branch NAME is inherited, so a knob like "automatically delete head
// branches" is set per repository and every new repository starts off-policy.
// This package reads the owner's repositories through `gh`, reports the ones
// that drift from the declared policy, and converges them.
package repolicy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/henderson-tech/vybava/internal/skipci"
	"gopkg.in/yaml.v3"
)

// Runner executes a process; tests substitute a scripted fake.
type Runner interface {
	// Run executes name with args in dir and returns trimmed stdout.
	Run(dir, name string, args ...string) (string, error)
}

// knob is one settable repository boolean: the policy key an operator writes,
// the `gh repo list --json` field that reads it, and the REST field that
// writes it. The three names differ per knob — GraphQL and REST disagree.
type knob struct {
	Key  string
	List string
	REST string
}

// Vocabulary is the complete set of settings a policy may declare, in report
// order. Extending it is one line plus its doc row — but only with a knob
// `gh repo list --json` actually serves: auto-merge, for one, is writable over
// REST yet absent from the list fields, and a sweep that needs one extra API
// call per repository is not a sweep.
var Vocabulary = []knob{
	{"deleteBranchOnMerge", "deleteBranchOnMerge", "delete_branch_on_merge"},
	{"allowMergeCommit", "mergeCommitAllowed", "allow_merge_commit"},
	{"allowSquashMerge", "squashMergeAllowed", "allow_squash_merge"},
	{"allowRebaseMerge", "rebaseMergeAllowed", "allow_rebase_merge"},
}

// Policy is the declared desired state. Only settings it names are compared or
// written — anything a policy does not mention is left exactly as it is.
type Policy struct {
	Owners   []string        `yaml:"owners" json:"owners,omitempty"`
	Exclude  []string        `yaml:"exclude" json:"exclude,omitempty"`
	Settings map[string]bool `yaml:"settings" json:"settings"`
	// Labels every repository must carry. Presence is the policy — an
	// existing label keeps whatever colour and text a human gave it; only a
	// missing one is created, with the declared colour and description.
	Labels []Label `yaml:"labels" json:"labels,omitempty"`
}

// Label is one repository label the policy requires.
type Label struct {
	Name        string `yaml:"name" json:"name"`
	Color       string `yaml:"color" json:"color,omitempty"`
	Description string `yaml:"description" json:"description,omitempty"`
}

// DefaultPolicy is what ships when no policy file is given: merged branches
// disappear, the two skip labels exist (docs/skip-ci.md), everything else
// stays the owner's business.
func DefaultPolicy() Policy {
	var labels []Label
	for _, l := range skipci.Labels() {
		labels = append(labels, Label{Name: l.Name, Color: l.Color, Description: l.Description})
	}
	return Policy{Settings: map[string]bool{"deleteBranchOnMerge": true}, Labels: labels}
}

// LoadPolicy reads a YAML policy file and validates its vocabulary.
//
// Decoding is strict: a mistyped top-level key (`excludes:` for `exclude:`)
// would otherwise load cleanly, leave the field empty, and let apply write to
// the very repositories the operator wrote the file to protect.
func LoadPolicy(path string) (Policy, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Policy{}, fmt.Errorf("read policy: %w", err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var p Policy
	if err := decoder.Decode(&p); err != nil && !errors.Is(err, io.EOF) {
		return Policy{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return p, p.Validate()
}

// Validate refuses a policy that declares nothing or names a setting outside
// the vocabulary — a typo must never pass as "no drift".
func (p Policy) Validate() error {
	if len(p.Settings) == 0 && len(p.Labels) == 0 {
		return fmt.Errorf("policy declares no settings and no labels — settings are one of: %s", strings.Join(keys(), ", "))
	}
	for _, l := range p.Labels {
		if strings.TrimSpace(l.Name) == "" {
			return fmt.Errorf("labels: every entry needs a name")
		}
	}
	for key := range p.Settings {
		if find(key) == nil {
			return fmt.Errorf("unknown setting %q — one of: %s", key, strings.Join(keys(), ", "))
		}
	}
	for _, repo := range p.Exclude {
		if strings.Count(repo, "/") != 1 {
			return fmt.Errorf("exclude %q must be owner/repo", repo)
		}
	}
	return nil
}

// declared returns the policy's knobs in vocabulary order.
func (p Policy) declared() []knob {
	var out []knob
	for _, k := range Vocabulary {
		if _, ok := p.Settings[k.Key]; ok {
			out = append(out, k)
		}
	}
	return out
}

// Options tune the sweep itself, never the desired state.
type Options struct {
	Limit           int
	IncludeArchived bool
}

// Drift is one repository setting that does not match the policy.
type Drift struct {
	Repo    string `json:"repo"`
	Setting string `json:"setting"`
	Want    bool   `json:"want"`
	Got     bool   `json:"got"`
}

// Change is the outcome of converging one repository.
type Change struct {
	Repo     string   `json:"repo"`
	Settings []string `json:"settings"`
	OK       bool     `json:"ok"`
	Error    string   `json:"error,omitempty"`
}

// Report is the stable envelope both verbs emit.
type Report struct {
	Owners   []string        `json:"owners"`
	Settings map[string]bool `json:"settings"`
	Labels   []Label         `json:"labels,omitempty"`
	Checked  int             `json:"checked"`
	Skipped  []string        `json:"skipped,omitempty"`
	Drift    []Drift         `json:"drift"`
	Applied  []Change        `json:"applied,omitempty"`
	Warnings []string        `json:"warnings,omitempty"`
}

// Failures lists the repositories Apply could not converge.
func (r Report) Failures() []Change {
	var out []Change
	for _, c := range r.Applied {
		if !c.OK {
			out = append(out, c)
		}
	}
	return out
}

// Audit reads every owner's repositories and reports the drift. It never
// writes.
func Audit(r Runner, p Policy, owners []string, opts Options) (Report, error) {
	if err := p.Validate(); err != nil {
		return Report{}, err
	}
	if len(owners) == 0 {
		owners = p.Owners
	}
	if len(owners) == 0 {
		return Report{}, fmt.Errorf("no owners — pass them as arguments or declare owners: in the policy")
	}
	if opts.Limit <= 0 {
		opts.Limit = 500
	}
	excluded := index(p.Exclude)
	report := Report{Owners: owners, Settings: p.Settings, Labels: p.Labels}

	for _, owner := range owners {
		repos, err := list(r, owner, p, opts)
		if err != nil {
			return Report{}, err
		}
		if len(repos) == opts.Limit {
			report.Warnings = append(report.Warnings,
				fmt.Sprintf("%s returned exactly --limit %d repositories — raise it, the tail was not read", owner, opts.Limit))
		}
		for _, repo := range repos {
			name, _ := repo["nameWithOwner"].(string)
			if name == "" {
				continue
			}
			if excluded[name] {
				report.Skipped = append(report.Skipped, name)
				continue
			}
			report.Checked++
			for _, k := range p.declared() {
				want := p.Settings[k.Key]
				got, ok := repo[k.List].(bool)
				if !ok {
					report.Warnings = append(report.Warnings,
						fmt.Sprintf("%s: gh did not report %s — setting skipped", name, k.List))
					continue
				}
				if got != want {
					report.Drift = append(report.Drift, Drift{Repo: name, Setting: k.Key, Want: want, Got: got})
				}
			}
			if len(p.Labels) > 0 {
				have, ok := labelNames(repo["labels"])
				if !ok {
					report.Warnings = append(report.Warnings, fmt.Sprintf("%s: gh did not report labels — labels skipped", name))
				}
				for _, l := range p.Labels {
					if ok && !have[l.Name] {
						report.Drift = append(report.Drift, Drift{Repo: name, Setting: labelKey(l.Name), Want: true, Got: false})
					}
				}
			}
		}
	}
	sort.SliceStable(report.Drift, func(i, j int) bool { return report.Drift[i].Repo < report.Drift[j].Repo })
	return report, nil
}

// Apply audits, then converges each drifting repository with ONE PATCH
// carrying all of its off-policy settings, plus one `gh label create` per
// missing label (the REST label endpoint takes one at a time). A repository
// the token cannot administer is reported, never fatal — the rest of the
// sweep still lands.
func Apply(r Runner, p Policy, owners []string, opts Options) (Report, error) {
	report, err := Audit(r, p, owners, opts)
	if err != nil {
		return report, err
	}
	for _, repo := range order(report.Drift) {
		args := []string{"api", "-X", "PATCH", "repos/" + repo, "--silent"}
		var names, labels []string
		for _, d := range report.Drift {
			if d.Repo != repo {
				continue
			}
			names = append(names, d.Setting)
			if l, ok := strings.CutPrefix(d.Setting, labelPrefix); ok {
				labels = append(labels, l)
				continue
			}
			args = append(args, "-F", find(d.Setting).REST+"="+strconv.FormatBool(d.Want))
		}
		change := Change{Repo: repo, Settings: names, OK: true}
		failed := func(err error) { change.OK, change.Error = false, err.Error() }
		if len(names) > len(labels) {
			if _, err := r.Run("", "gh", args...); err != nil {
				failed(err)
			}
		}
		for _, name := range labels {
			l := p.label(name)
			if _, err := r.Run("", "gh", skipci.LabelArgs(skipci.Label{Name: l.Name, Color: l.Color, Description: l.Description}, repo)...); err != nil && change.OK {
				failed(err)
			}
		}
		report.Applied = append(report.Applied, change)
	}
	return report, nil
}

// labelPrefix keys a label row in Drift.Setting and Change.Settings, so the
// one report shape carries both kinds of drift.
const labelPrefix = "label:"

func labelKey(name string) string { return labelPrefix + name }

// labelNames reads the label names `gh repo list --json labels` served.
func labelNames(v any) (map[string]bool, bool) {
	rows, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := map[string]bool{}
	for _, row := range rows {
		if m, ok := row.(map[string]any); ok {
			if name, _ := m["name"].(string); name != "" {
				out[name] = true
			}
		}
	}
	return out, true
}

func (p Policy) label(name string) Label {
	for _, l := range p.Labels {
		if l.Name == name {
			return l
		}
	}
	return Label{Name: name}
}

// list asks gh for exactly the fields the policy declares — never the whole
// repository object, and never a field an older gh would reject.
func list(r Runner, owner string, p Policy, opts Options) ([]map[string]any, error) {
	fields := []string{"nameWithOwner"}
	for _, k := range p.declared() {
		fields = append(fields, k.List)
	}
	if len(p.Labels) > 0 {
		fields = append(fields, "labels")
	}
	args := []string{"repo", "list", owner, "--limit", strconv.Itoa(opts.Limit), "--json", strings.Join(fields, ",")}
	if !opts.IncludeArchived {
		args = append(args, "--no-archived")
	}
	out, err := r.Run("", "gh", args...)
	if err != nil {
		return nil, fmt.Errorf("gh repo list %s: %w", owner, err)
	}
	var repos []map[string]any
	if err := json.Unmarshal([]byte(out), &repos); err != nil {
		return nil, fmt.Errorf("gh repo list %s: unreadable output: %w", owner, err)
	}
	return repos, nil
}

// order returns the drifting repositories once each, in report order.
func order(drift []Drift) []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range drift {
		if !seen[d.Repo] {
			seen[d.Repo], out = true, append(out, d.Repo)
		}
	}
	return out
}

func index(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, v := range values {
		out[v] = true
	}
	return out
}

func find(key string) *knob {
	for i, k := range Vocabulary {
		if k.Key == key {
			return &Vocabulary[i]
		}
	}
	return nil
}

func keys() []string {
	out := make([]string, 0, len(Vocabulary))
	for _, k := range Vocabulary {
		out = append(out, k.Key)
	}
	return out
}
