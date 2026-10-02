package watch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// cannedRunner answers each "<name> <args>" with a fixture.
func cannedRunner(t *testing.T, answers map[string]string) Runner {
	return func(_ context.Context, _ string, name string, args ...string) ([]byte, error) {
		key := name + " " + strings.Join(args, " ")
		out, ok := answers[key]
		if !ok {
			t.Fatalf("unexpected command %q", key)
		}
		return []byte(out), nil
	}
}

const precheckJSON = `{
  "owner": "henderson-tech", "repo": "vybava", "pr": 155, "url": "u", "title": "t",
  "checks": "%s",
  "gates": {"openOk": true, "draftOk": true, "cleanOk": false, "mergeableOk": true, "ciOk": %t,
            "approvedOk": true, "botApprovalOk": %t, "allPass": false, "failed": [%s], "ciWaived": false},
  "botApproval": {"ok": %t, "required": ["eve-bot-lovinka[bot]"], "pending": [%s]},
  "raw": {"state": "%s", "mergeable": "MERGEABLE", "mergeStateStatus": "CLEAN", "reviewDecision": "APPROVED", "isDraft": false}
}`

func precheck(checks string, ciOK, botOK bool, failed, pending, state string) Verb {
	return func(_ context.Context, args []string, stdout, _ io.Writer) int {
		if len(args) != 2 || args[0] != "155" || args[1] != "--repo=/w/vybava" {
			panic(fmt.Sprintf("merge-precheck argv %v", args))
		}
		fmt.Fprintf(stdout, precheckJSON, checks, ciOK, botOK, failed, botOK, pending, state)
		return 0
	}
}

func TestPRProbeReadsGitkitMergePrecheck(t *testing.T) {
	ctx := context.Background()
	p := PRProbe{Precheck: precheck("PENDING", false, false, `"clean","ci","botReview"`, `"eve-bot-lovinka[bot]"`, "OPEN")}
	obs, err := p.Observe(ctx, "henderson-tech/vybava#155", "/w/vybava")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"state": "OPEN", "checks": "PENDING", "ci": "pending", "review": "APPROVED",
		"bots": "pending: eve-bot-lovinka[bot]", "mergeable": "MERGEABLE", "draft": "false", "failed": "ci,botReview"}
	for k, v := range want {
		if obs.Fields[k] != v {
			t.Errorf("%s = %q, want %q", k, obs.Fields[k], v)
		}
	}
	conds := p.Conditions()
	if conds["checks-settled"].Holds(obs) || conds["eve-approved"].Holds(obs) || conds["ready"].Holds(obs) {
		t.Fatalf("pending PR reads settled/approved/ready: %+v", obs.Fields)
	}

	p.Precheck = precheck("SUCCESS", true, true, `"clean"`, ``, "OPEN")
	obs, _ = p.Observe(ctx, "henderson-tech/vybava#155", "/w/vybava")
	for _, c := range []string{"checks-settled", "checks-green", "eve-approved", "ready"} {
		if !conds[c].Holds(obs) {
			t.Errorf("green, approved PR does not hold %s: %+v", c, obs.Fields)
		}
	}
	if !strings.Contains(obs.Summary, "ready") {
		t.Errorf("summary %q", obs.Summary)
	}

	p.Precheck = precheck("FAILURE", false, true, `"ci"`, ``, "OPEN")
	obs, _ = p.Observe(ctx, "henderson-tech/vybava#155", "/w/vybava")
	if !conds["checks-red"].Holds(obs) || !conds["checks-settled"].Holds(obs) {
		t.Fatalf("red PR: %+v", obs.Fields)
	}

	p.Precheck = precheck("SUCCESS", true, true, ``, ``, "MERGED")
	obs, _ = p.Observe(ctx, "henderson-tech/vybava#155", "/w/vybava")
	if !conds["merged"].Holds(obs) || !conds["closed"].Holds(obs) {
		t.Fatalf("merged PR: %+v", obs.Fields)
	}

	if _, err := p.Observe(ctx, "other/repo#155", "/w/vybava"); err == nil || !strings.Contains(err.Error(), "not other/repo") {
		t.Fatalf("a checkout of another repo was accepted: %v", err)
	}
	if _, err := p.Observe(ctx, "henderson-tech/vybava#155", ""); err == nil {
		t.Fatal("a PR without an anchor checkout was probed")
	}
	p.Precheck = func(_ context.Context, _ []string, _, stderr io.Writer) int {
		fmt.Fprintln(stderr, "error: gh: Could not resolve to a PullRequest")
		return 1
	}
	if _, err := p.Observe(ctx, "henderson-tech/vybava#155", "/w/vybava"); err == nil || !strings.Contains(err.Error(), "Could not resolve") {
		t.Fatalf("a failing precheck answered %v", err)
	}
}

func TestPRProbeCanonicalizesEveryRefForm(t *testing.T) {
	p := PRProbe{Run: cannedRunner(t, map[string]string{"gh repo view --json nameWithOwner": `{"nameWithOwner":"henderson-tech/vybava"}`})}
	ctx := context.Background()
	for ref, want := range map[string]string{
		"155":                       "henderson-tech/vybava#155",
		"#155":                      "henderson-tech/vybava#155",
		"henderson-tech/vybava#155": "henderson-tech/vybava#155",
		"https://github.com/henderson-tech/vybava/pull/155": "henderson-tech/vybava#155",
	} {
		got, err := p.Canonical(ctx, ref, "/w/vybava")
		if err != nil || got != want {
			t.Errorf("%s → %q %v, want %q", ref, got, err, want)
		}
	}
	if _, err := p.Canonical(ctx, "155", ""); err == nil {
		t.Error("a bare number without a checkout was accepted")
	}
	if _, err := p.Canonical(ctx, "main", "/w"); err == nil {
		t.Error("a branch name was accepted as a PR")
	}
}

func TestDevboxProbes(t *testing.T) {
	ctx := context.Background()
	boxes := DevboxProbe{Run: cannedRunner(t, map[string]string{"devbox boxes --json": `{"v":3,"ok":true,"verb":"boxes","data":{"boxes":[
		{"name":"a","state":"active","reachable":true,"health":"ok"},
		{"name":"b","state":"active","reachable":false,"health":"ok"}]}}`})}
	obs, err := boxes.Observe(ctx, "a", "")
	if err != nil || !boxes.Conditions()["up"].Holds(obs) {
		t.Fatalf("box a: %+v %v", obs, err)
	}
	obs, _ = boxes.Observe(ctx, "b", "")
	if !boxes.Conditions()["down"].Holds(obs) {
		t.Fatalf("unreachable box b reads up: %+v", obs)
	}
	if _, err := boxes.Observe(ctx, "c", ""); err == nil || !strings.Contains(err.Error(), "boxes: a, b") {
		t.Fatalf("missing box: %v", err)
	}

	runs := DevboxRunProbe{Run: cannedRunner(t, map[string]string{
		"devbox status ws-busy --json": `{"v":3,"ok":true,"data":{"runs":[{"run":"ws-busy.41","state":"running"},{"run":"ws-busy.42","state":"waiting-admission"}]}}`,
		"devbox status ws-idle --json": `{"v":3,"ok":true,"data":{"runs":[]}}`,
		"devbox status ws-odd --json":  `{"v":3,"ok":true,"data":{"overview":{}}}`,
		"devbox status ws-bad --json":  `{"v":3,"ok":false,"diagnostics":[{"code":"WS_UNKNOWN","detail":"no workspace ws-bad"}]}`,
	})}
	obs, _ = runs.Observe(ctx, "ws-busy", "")
	if obs.Fields["runs"] != "2" || !runs.Conditions()["running"].Holds(obs) || runs.Conditions()["idle"].Holds(obs) {
		t.Fatalf("busy workspace: %+v", obs.Fields)
	}
	obs, _ = runs.Observe(ctx, "ws-idle", "")
	if !runs.Conditions()["idle"].Holds(obs) {
		t.Fatalf("idle workspace: %+v", obs.Fields)
	}
	if _, err := runs.Observe(ctx, "ws-odd", ""); err == nil {
		t.Fatal("an answer without runs read as idle")
	}
	if _, err := runs.Observe(ctx, "ws-bad", ""); err == nil || !strings.Contains(err.Error(), "WS_UNKNOWN") {
		t.Fatalf("a failed envelope answered %v", err)
	}
}

func TestVitrinkaProbe(t *testing.T) {
	p := VitrinkaProbe{Run: cannedRunner(t, map[string]string{
		"vitrinka task get fixit/4759 --json": `{"ok":true,"data":{"task":{"id":4759,"state":"in_review","status":"in_review","title":"x"}}}`,
		"vitrinka task get fixit/1 --json":    `{"ok":true,"data":{"task":{"state":"done","status":"done"}}}`,
	})}
	ctx := context.Background()
	obs, err := p.Observe(ctx, "fixit/4759", "")
	if err != nil || obs.Fields["status"] != "in_review" || p.Conditions()["done"].Holds(obs) {
		t.Fatalf("%+v %v", obs, err)
	}
	obs, _ = p.Observe(ctx, "fixit/1", "")
	if !p.Conditions()["done"].Holds(obs) || !p.Conditions()["closed"].Holds(obs) {
		t.Fatalf("done task: %+v", obs)
	}
	if _, err := p.Canonical(ctx, "fixit/abc", ""); err == nil {
		t.Fatal("a non-numeric task id was accepted")
	}
}

func TestDeployikProbe(t *testing.T) {
	p := DeployikProbe{Run: cannedRunner(t, map[string]string{"deployik status luko --json": `{"slug":"luko","environments":[
		{"name":"production","branch":"main","rollup_status":"building"},
		{"name":"preview","branch":"x","rollup_status":"live"}]}`})}
	ctx := context.Background()
	ref, err := p.Canonical(ctx, "luko", "")
	if err != nil || ref != "luko/production" {
		t.Fatalf("canonical %q %v", ref, err)
	}
	obs, err := p.Observe(ctx, ref, "")
	if err != nil || !p.Conditions()["building"].Holds(obs) || p.Conditions()["settled"].Holds(obs) {
		t.Fatalf("building env: %+v %v", obs, err)
	}
	if _, err := p.Observe(ctx, "luko/staging", ""); err == nil || !strings.Contains(err.Error(), "production, preview") {
		t.Fatalf("missing env answered %v", err)
	}
}

func TestExecRunnerCarriesStderrIntoTheError(t *testing.T) {
	_, err := ExecRunner(context.Background(), "", "/bin/sh", "-c", "echo boom >&2; exit 3")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("stderr lost: %v", err)
	}
	var exit interface{ ExitCode() int }
	if !errors.As(err, &exit) || exit.ExitCode() != 3 {
		t.Fatalf("exit status lost: %v", err)
	}
}

func TestDefaultProbesServeEveryKind(t *testing.T) {
	var kinds []string
	for _, p := range DefaultProbes(ExecRunner) {
		kinds = append(kinds, p.Kind())
		if pr, ok := p.(PRProbe); ok && pr.Precheck == nil {
			t.Fatal("the PR probe has no gitkit merge-precheck")
		}
	}
	if got := strings.Join(kinds, ","); got != "pr,devbox,devbox-run,vitrinka,deployik" {
		t.Fatalf("kinds %s", got)
	}
}
