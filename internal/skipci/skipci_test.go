package skipci

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const sample = `name: CI

on:
  pull_request:
  push:
    branches: [main]

jobs:
  # the plain job: no condition at all
  verify:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
  gated:
    if: github.event.action == 'opened' || github.event.action == 'reopened'
    runs-on: ubuntu-latest
  shelled:
    if: ${{ inputs.deploy == true }}
    runs-on: ubuntu-latest
  done:
    if: github.event_name != 'pull_request' || !contains(github.event.pull_request.labels.*.name, 'skip-ci')
    runs-on: ubuntu-latest
  never-on-pr:
    if: github.event_name != 'pull_request'
    runs-on: ubuntu-latest
  folded:
    if: >-
      github.ref == 'refs/heads/main' &&
      inputs.deploy == true
    runs-on: ubuntu-latest
`

func write(t *testing.T, dir, name, src string) string {
	t.Helper()
	wf := filepath.Join(dir, ".github", "workflows")
	if err := os.MkdirAll(wf, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(wf, name)
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func states(wf Workflow) map[string]State {
	m := map[string]State{}
	for _, j := range wf.Jobs {
		m[j.Name] = j.State
	}
	return m
}

func TestCheckClassifiesJobs(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "ci.yml", sample)
	write(t, dir, "release.yml", "on:\n  push:\n    tags: ['v*']\njobs:\n  ship:\n    runs-on: ubuntu-latest\n")
	write(t, dir, "seq.yml", "on: [push, pull_request_target]\njobs:\n  a:\n    runs-on: x\n")

	r, err := Check(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Workflows) != 3 || r.Workflows[0].Path != ".github/workflows/ci.yml" {
		t.Fatalf("workflows: %+v", r.Workflows)
	}
	got := states(r.Workflows[0])
	want := map[string]State{"verify": Missing, "gated": Wrap, "shelled": Wrap, "done": Guarded, "never-on-pr": Guarded, "folded": Manual}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: got %s, want %s", k, got[k], v)
		}
	}
	if r.Workflows[1].PullRequest || len(r.Workflows[1].Jobs) != 0 {
		t.Errorf("push-only workflow must carry no jobs: %+v", r.Workflows[1])
	}
	if !r.Workflows[2].PullRequest || states(r.Workflows[2])["a"] != Missing {
		t.Errorf("sequence trigger with pull_request_target: %+v", r.Workflows[2])
	}
	if r.Guarded != 2 || r.Missing != 2 || r.Wrap != 2 || r.Manual != 1 || r.Clean() {
		t.Errorf("totals: %+v", r)
	}
}

func TestApplyInsertsWrapsAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "ci.yml", sample)

	r, err := Apply(dir)
	if err != nil {
		t.Fatal(err)
	}
	if r.Applied != 3 || r.Missing != 0 || r.Wrap != 0 || r.Manual != 1 {
		t.Fatalf("apply: %+v", r)
	}
	out, _ := os.ReadFile(path)
	text := string(out)
	for _, want := range []string{
		"  verify:\n    if: " + Guard + "\n    runs-on: ubuntu-latest",
		"  gated:\n    if: (github.event.action == 'opened' || github.event.action == 'reopened') && (" + Guard + ")\n",
		"  shelled:\n    if: ${{ (inputs.deploy == true) && (" + Guard + ") }}\n",
		"  # the plain job: no condition at all\n",               // comments survive
		"    if: >-\n      github.ref == 'refs/heads/main' &&\n      inputs.deploy == true\n", // manual left alone
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	// The result is still valid YAML whose guarded jobs read the guard back.
	var doc map[string]any
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("rewritten file is not YAML: %v", err)
	}
	jobs := doc["jobs"].(map[string]any)
	if got := jobs["gated"].(map[string]any)["if"]; got != "(github.event.action == 'opened' || github.event.action == 'reopened') && ("+Guard+")" {
		t.Errorf("gated if round-trips wrong: %q", got)
	}

	again, err := Apply(dir)
	if err != nil {
		t.Fatal(err)
	}
	if again.Applied != 0 || again.Guarded != 5 {
		t.Errorf("second apply must be a no-op: %+v", again)
	}
	if after, _ := os.ReadFile(path); string(after) != text {
		t.Error("second apply changed the file")
	}
}

func TestApplyAppliedFlagsAndJobKeyWithComment(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "ci.yml", "on: pull_request\njobs:\n  a: # comment on the key\n\n    # a note\n    runs-on: x\n  b:\n    if: \"github.actor != 'dependabot[bot]'\"\n    runs-on: x\n")
	r, err := Apply(dir)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(filepath.Join(dir, ".github", "workflows", "ci.yml"))
	if !strings.Contains(string(out), "  a: # comment on the key\n    if: "+Guard+"\n\n    # a note\n") {
		t.Errorf("insert after a commented key:\n%s", out)
	}
	if !strings.Contains(string(out), "  b:\n    if: (github.actor != 'dependabot[bot]') && ("+Guard+")\n") {
		t.Errorf("double-quoted if re-emitted plain:\n%s", out)
	}
	for _, j := range r.Workflows[0].Jobs {
		if !j.Applied || j.State != Guarded {
			t.Errorf("%s: applied=%v state=%s", j.Name, j.Applied, j.State)
		}
	}
}

func TestNoWorkflowsAndBadYAML(t *testing.T) {
	if _, err := Check(t.TempDir()); err != ErrNoWorkflows {
		t.Errorf("want ErrNoWorkflows, got %v", err)
	}
	dir := t.TempDir()
	write(t, dir, "broken.yml", "on: [pull_request\njobs: {")
	r, err := Apply(dir)
	if err != nil {
		t.Fatal(err)
	}
	if r.Workflows[0].Error == "" || len(r.Workflows[0].Jobs) != 0 {
		t.Errorf("unparsable file must be reported, never edited: %+v", r.Workflows[0])
	}
}

func TestLabelArgs(t *testing.T) {
	got := strings.Join(LabelArgs(Labels()[1], "henderson-tech/vybava"), " ")
	want := "label create eve-ignore --color ededed --description skip eve's automatic PR review --force --repo henderson-tech/vybava"
	if got != want {
		t.Errorf("got %q", got)
	}
}

func TestIndirectGuardsThroughEventsAndNeeds(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "ci.yml", `on: [pull_request, push, schedule]
jobs:
  gate:
    if: github.event_name != 'pull_request' || !contains(github.event.pull_request.labels.*.name, 'skip-ci')
    runs-on: x
  unit:
    needs: gate
    if: ${{ needs.gate.outputs.unit == 'true' }}
    runs-on: x
  summary:
    needs: [gate, unit]
    if: ${{ always() }}
    runs-on: x
  image:
    if: github.event_name == 'push' && github.ref == 'refs/heads/main'
    runs-on: x
  reconcile:
    if: github.event_name == 'schedule' || github.event_name == 'workflow_dispatch'
    runs-on: x
  bench:
    if: (github.event_name != 'pull_request') && !inputs.e2e_only
    runs-on: x
  mixed:
    if: github.event_name == 'push' || github.event.action == 'opened'
    runs-on: x
  loose:
    needs: [gate, mixed]
    runs-on: x
`)
	r, err := Check(dir)
	if err != nil {
		t.Fatal(err)
	}
	via := map[string]string{}
	for _, j := range r.Workflows[0].Jobs {
		via[j.Name] = string(j.State) + "/" + j.Via
	}
	want := map[string]string{
		"gate": "guarded/", "unit": "guarded/needs", "summary": "guarded/needs",
		"image": "guarded/event", "reconcile": "guarded/event", "bench": "guarded/event",
		"mixed": "wrap/", "loose": "missing/",
	}
	for k, v := range want {
		if via[k] != v {
			t.Errorf("%s: got %s, want %s", k, via[k], v)
		}
	}
	if r.Clean() {
		t.Error("mixed and loose must still count as drift")
	}
}
