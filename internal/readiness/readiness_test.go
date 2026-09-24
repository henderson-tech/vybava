package readiness

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	assets "github.com/henderson-tech/vybava"
	"github.com/henderson-tech/vybava/internal/runx"
)

// adapter is a complete, valid section; tests mutate copies of it.
const adapter = `{
  "vitrinka": {"workspace": "acme", "project": "shop"},
  "repos": [{"id": "app", "path": ".", "github": "acme/shop", "production": {"tag": "v[0-9]*", "exclude": "*-*"}, "integration": "main"}],
  "lane": {
    "worktree": "bun run worktree:create rr-{slug}",
    "devEnv": {"up": "devbox up {ws}", "hold": "devbox hold {ws} --for 2h", "park": "devbox unhold {ws}; devbox park {ws}"},
    "heavy": "devbox run -- '{cmd}'"
  },
  "tests": [{"name": "api-unit", "framework": "jest", "cmd": "bun run api:test:unit -- {pattern}"}],
  "devices": {
    "runner": "device-runner", "build": "release", "concurrent": 4,
    "matrix": [
      {"id": "ios-phone", "platform": "ios", "framework": "appium", "host": "mac", "name": "iPhone 17 Pro", "width": 1206, "run": "bun run appium:ios:release -- --spec {specs}"},
      {"id": "android-phone", "platform": "android", "framework": "appium", "host": "mac", "name": "Pixel 7", "build": "bun run build:apk -- --api {apiUrl} --out {out}", "run": "bun run appium:android:release -- --spec {specs}"},
      {"id": "web-desktop", "platform": "web", "framework": "playwright", "host": "devbox", "name": "Desktop Chrome", "width": 1440, "run": "bash e2e.sh --grep {grep}"}
    ],
    "realtime": {"specs": ["appium/specs/multi/**"], "roles": ["customer", "worker"], "directions": "both", "run": "CUSTOMER={customer} WORKER={worker} bun run appium:multi -- --spec {spec}"}
  },
  "merge": {"command": "/prm --auto --audit", "order": ["app"]},
  "final": {"checks": ["bun run release:preflight"], "handoff": "/release:production"},
  "rules": ["Run api:generate before test lanes."],
  "plumbing": ["iOS simulator release build that bakes the lane API URL"]
}`

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// isolateGit keeps the developer's git config (signing, hooks) out of test repos.
func isolateGit(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
}

// fixture clones an upstream with tags v1.0.0 (production) and
// v1.1.0-rc.1 (a prerelease the exclude glob skips) plus two later commits,
// and writes the adapter as the clone's vybava.config.json.
func fixture(t *testing.T, section string) *Tool {
	t.Helper()
	isolateGit(t)
	base := t.TempDir()
	up := filepath.Join(base, "upstream")
	gitRun(t, base, "init", "-q", "-b", "main", up)
	for i, tag := range []string{"v1.0.0", "v1.1.0-rc.1", "", ""} {
		gitRun(t, up, "commit", "-q", "--allow-empty", "-m", "change "+string(rune('a'+i)))
		if tag != "" {
			gitRun(t, up, "tag", tag)
		}
	}
	root := filepath.Join(base, "app")
	gitRun(t, base, "clone", "-q", up, root)
	doc := `{"guards": {"simCap": 4}, "readiness": ` + section + `}`
	if err := os.WriteFile(filepath.Join(root, "vybava.config.json"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	payload, err := fs.Sub(assets.FS, "skills/release-readiness")
	if err != nil {
		t.Fatal(err)
	}
	tool, err := Open(root, payload)
	if err != nil {
		t.Fatal(err)
	}
	tool.Home = base
	tool.Now = func() time.Time { return time.Date(2026, 9, 24, 21, 0, 0, 0, time.UTC) }
	return tool
}

func code(err error) string {
	var de runx.DiagError
	if errors.As(err, &de) {
		return de.Diag.Code
	}
	return ""
}

func TestValidateReportsEveryProblem(t *testing.T) {
	var c Config
	if err := json.Unmarshal([]byte(adapter), &c); err != nil {
		t.Fatal(err)
	}
	if problems := c.Validate(); len(problems) != 0 {
		t.Fatalf("valid adapter rejected: %v", problems)
	}
	c.Lane.Heavy = "HOME=${HOME} devbox run -- '{cmd}'" // a shell expansion is not a token
	c.Devices.Realtime.Run += " CUSTOMER_UDID={customerUdid} PORT={port}"
	if problems := c.Validate(); len(problems) != 0 {
		t.Fatalf("shell expansion or role tokens rejected: %v", problems)
	}
	c.Lane.Worktree = "create {branch}"
	c.Merge.Order = []string{"api"}
	c.Devices.Realtime.Run = "run {spec} as {admin}"
	c.Devices.Realtime.Roles = []string{"customer", "spec"}
	c.Devices.Concurrent = 1
	c.Repos[0].Production.Branch = "release"
	c.Repos = append(c.Repos, Repo{ID: "eve", Path: "../eve", GitHub: "acme/eve", Production: Production{Tag: "v*"}, Integration: "main"})
	got := strings.Join(c.Validate(), "\n")
	for _, want := range []string{
		"lane.worktree: unknown token {branch}",
		`merge.order[0]: "api" is not a repo id`,
		"devices.realtime.run: unknown token {admin}",
		`devices.realtime.roles[1]: "spec" collides with the built-in {spec} token`,
		"devices.concurrent (1) is below one full set of mac devices (2)",
		"repos[0].production: set tag or branch, not both",
		"repos[1].worktree is required",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	c.Devices.Runner = "swarm"
	if got := strings.Join(c.Validate(), "\n"); !strings.Contains(got, `devices.runner: "swarm" is not one of lane | device-runner`) {
		t.Errorf("unknown runner accepted:\n%s", got)
	}
}

func TestOpenRejectsUnknownKeys(t *testing.T) {
	isolateGit(t)
	root := t.TempDir()
	gitRun(t, root, "init", "-q")
	doc := `{"readiness": ` + adapter[:len(adapter)-1] + `, "exprots": "Shop"}}`
	if err := os.WriteFile(filepath.Join(root, "vybava.config.json"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root, assets.FS); code(err) != DiagConfigInvalid || !strings.Contains(err.Error(), "exprots") {
		t.Fatalf("typo'd key accepted: %v", err)
	}
}

func TestCheckResolvesTheProductionTagAndFlagsPlumbing(t *testing.T) {
	tool := fixture(t, adapter)
	res, err := tool.Check(false)
	if err != nil {
		t.Fatal(err)
	}
	data := res.Data.(*CheckData)
	rr := data.Ranges[0]
	if rr.Production != "v1.0.0" || rr.Integration != "origin/main" || rr.Commits != 3 || rr.Range != "v1.0.0..origin/main" {
		t.Fatalf("range %+v", rr)
	}
	if data.SimCap != 4 || data.Budget != 4 {
		t.Fatalf("simCap %d budget %d", data.SimCap, data.Budget)
	}
	codes := map[string]int{}
	for _, d := range res.Diagnostics {
		codes[d.Code]++
	}
	// ios-phone has no build command; android-phone does; web is not the runner's.
	if codes[DiagDeviceBuildMissing] != 1 || codes[DiagPlumbingPending] != 1 || codes[DiagSimCapBelowDevices] != 0 {
		t.Fatalf("diagnostics %v", res.Diagnostics)
	}
}

func TestRepoPathsResolveFromTheMainCheckout(t *testing.T) {
	tool := fixture(t, adapter)
	main := tool.Root
	wt := filepath.Join(main, ".worktrees", "lane")
	gitRun(t, main, "worktree", "add", "-q", "--detach", wt)
	// A sibling path is meant relative to the main checkout, never to whichever
	// worktree the orchestrator stands in.
	section := strings.Replace(adapter, `"integration": "main"}]`,
		`"integration": "main"}, {"id": "sib", "path": "../app", "github": "acme/sib", "production": {"branch": "main"}, "integration": "main"}]`, 1)
	if err := os.WriteFile(filepath.Join(wt, "vybava.config.json"), []byte(`{"readiness": `+section+`}`), 0o644); err != nil {
		t.Fatal(err)
	}
	fromWT, err := Open(wt, tool.Payload)
	if err != nil {
		t.Fatal(err)
	}
	res, err := fromWT.Range(false)
	if err != nil {
		t.Fatal(err)
	}
	ranges := res.Data.([]RepoRange)
	// git reports real paths; macOS temp dirs sit behind a /var symlink.
	if main, err = filepath.EvalSymlinks(main); err != nil {
		t.Fatal(err)
	}
	if len(ranges) != 2 || ranges[0].Path != main || ranges[1].Path != main || ranges[1].Commits != 0 {
		t.Fatalf("ranges from a worktree: %+v", ranges)
	}
}

func TestInitSeedsTheRunDirectoryOnce(t *testing.T) {
	tool := fixture(t, strings.Replace(adapter, `"vitrinka"`, `"exports": "Shop", "vitrinka"`, 1))
	res, err := tool.Init("", "", false)
	if err != nil {
		t.Fatal(err)
	}
	dir := res.Data.(*InitData).Dir
	if want := filepath.Join(tool.Home, "Exports", "Shop", "release-readiness-2026-09-24"); dir != want {
		t.Fatalf("dir %s, want %s", dir, want)
	}
	for _, name := range []string{RunFile, ArgsFile, "slot", "uniq-shots.sh", "inventory.workflow.js", "results.md", "decisions.md", "rotation.md"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s not seeded: %v", name, err)
		}
	}
	run, err := ReadRun(dir)
	if err != nil || run.Ranges[0].Production != "v1.0.0" {
		t.Fatalf("run.json %+v %v", run, err)
	}
	// Frozen by sha: a later fetch that moves origin/main cannot widen it.
	if rr := run.Ranges[0]; rr.Range != rr.ProdSHA+".."+rr.IntSHA || len(rr.IntSHA) != 40 {
		t.Fatalf("range not pinned to shas: %+v", rr)
	}

	if err := os.WriteFile(filepath.Join(dir, "slot"), []byte("adapted\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "results.md"), []byte("- lane result\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = tool.Init(dir, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Data.(*InitData).Created; len(got) != 0 {
		t.Fatalf("second init created %v", got)
	}
	if len(res.Diagnostics) != 1 || res.Diagnostics[0].Code != DiagPayloadDiffers {
		t.Fatalf("diagnostics %v", res.Diagnostics)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "results.md")); string(b) != "- lane result\n" {
		t.Fatalf("ledger overwritten: %q", b)
	}
}

func TestRenderWritesLaneFilesAndDetectsDrift(t *testing.T) {
	tool := fixture(t, adapter)
	dir := t.TempDir()
	if _, err := tool.Init(dir, "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Render(dir, false); code(err) != DiagAuthorityMissing {
		t.Fatalf("render without authority: %v", err)
	}
	run, _ := ReadRun(dir)
	run.Authority = Authority{Merge: "/prm --auto --audit", Finish: "main ready + readiness board", Concurrency: 12}
	run.Epic = TaskRef{ID: "100", URL: "https://example.test/t/100"}
	inv := Inventory{Inventories: []Cluster{{Lane: "Checkout", Features: []Feature{{
		Name: "Card refunds", Summary: "Customers choose voucher or card.", Refs: []string{"app#12"},
		Devices: []string{"ios-phone"}, Gaps: []string{"No e2e for a customer card refund."}, Risk: "high",
	}}}}}
	lanes := []LaneSpec{
		{Slug: "refunds", Title: "Refunds", Features: []string{"0.0"}, Story: TaskRef{ID: "101", URL: "https://example.test/t/101"}, QA: TaskRef{ID: "102", URL: "https://example.test/t/102"}},
		{Slug: "pending", Title: "Pending", Features: []string{"0.0"}},
	}
	for name, v := range map[string]any{RunFile: run, InventoryFile: inv, LanesFile: lanes} {
		if err := writeJSON(filepath.Join(dir, name), v); err != nil {
			t.Fatal(err)
		}
	}
	res, err := tool.Render(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	written := strings.Join(res.Data.(*RenderData).Written, " ")
	if written != "lane-rules.md device-runner.md final-brief.md body-refunds.md brief-refunds.md body-pending.md" {
		t.Fatalf("written %q", written)
	}
	if len(res.Diagnostics) != 1 || res.Diagnostics[0].Code != DiagLaneTasksPending {
		t.Fatalf("diagnostics %v", res.Diagnostics)
	}
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	for name, wants := range map[string][]string{
		"body-refunds.md":  {"## Card refunds  _(risk: high; devices: ios-phone)_", "  - [ ] No e2e for a customer card refund."},
		"brief-refunds.md": {"`bun run worktree:create rr-refunds`", "https://example.test/t/102"},
		"lane-rules.md":    {"devbox run -- '{cmd}'", "- Run api:generate before test lanes.", "sh " + dir + "/uniq-shots.sh", `"ios-phone", "android-phone", "web-desktop"`},
		"device-runner.md": {"At most 4 devices at once", "2 full set(s)", "build: NONE YET", "every role runs on every platform"},
		"final-brief.md":   {"`bun run release:preflight`", "`/release:production`"},
	} {
		got := read(name)
		for _, want := range wants {
			if !strings.Contains(got, want) {
				t.Errorf("%s lacks %q:\n%s", name, want, got)
			}
		}
	}

	if err := os.WriteFile(filepath.Join(dir, "lane-rules.md"), []byte("hand-edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if res, err := tool.Render(dir, true); code(err) != DiagRenderDrift || strings.Join(res.Data.(*RenderData).Drifted, " ") != "lane-rules.md" {
		t.Fatalf("drift not reported: %v", err)
	}
	if read("lane-rules.md") != "hand-edited\n" {
		t.Fatal("--check wrote a file")
	}
}

func TestRenderFollowsTheRunsScopeAndLanes(t *testing.T) {
	tool := fixture(t, adapter)
	dir := t.TempDir()
	if _, err := tool.Init(dir, "", false); err != nil {
		t.Fatal(err)
	}
	// Ids exactly as vitrinka answers them: numbers.
	runJSON := `{"v": 1, "date": "2026-09-24", "dir": "` + dir + `", "epic": {"id": 100, "url": "https://example.test/t/100"},
	  "authority": {"merge": "human", "devices": ["ios-phone", "android-phone"], "concurrency": 2, "finish": "main ready"}}`
	inv := `{"inventories": [{"lane": "Checkout", "features": [{"name": "Refunds", "summary": "s", "refs": ["app#1"], "gaps": ["g"], "risk": "low", "repos": ["app"]}]}],
	  "critic": {"unassigned": [{"ref": "app@abc123456", "repo": "app", "subject": "fix: stray refund rounding", "lane": "Checkout"}]}}`
	lanes := `[{"slug": "refunds", "title": "Refunds", "features": ["0.0"], "unassigned": [0], "story": {"id": 101, "url": "https://example.test/t/101"}, "qa": {"id": 102, "url": "https://example.test/t/102"}}]`
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(RunFile, runJSON)
	write(InventoryFile, inv)
	write(LanesFile, lanes)
	write("body-gone.md", "a lane dropped from lanes.json\n")
	res, err := tool.Render(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if removed := res.Data.(*RenderData).Removed; len(removed) != 1 || removed[0] != "body-gone.md" {
		t.Fatalf("stale body not removed: %v", removed)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "body-refunds.md"))
	brief, _ := os.ReadFile(filepath.Join(dir, "brief-refunds.md"))
	rules, _ := os.ReadFile(filepath.Join(dir, "lane-rules.md"))
	for _, c := range []struct{ file, got, want string }{
		{"body", string(body), "- [ ] app@abc123456: fix: stray refund rounding"},
		{"brief", string(brief), "QA task #102"},
		{"brief", string(brief), "`--task 102`"},
		{"lane-rules", string(rules), "ios-phone = iPhone 17 Pro"},
	} {
		if !strings.Contains(c.got, c.want) {
			t.Errorf("%s lacks %q:\n%s", c.file, c.want, c.got)
		}
	}
	// web-desktop is out of the phase-0 scope: nobody is told to run it.
	if strings.Contains(string(rules), "web-desktop") {
		t.Errorf("lane-rules still names the out-of-scope web-desktop:\n%s", rules)
	}

	write(RunFile, strings.Replace(runJSON, `"android-phone"`, `"pixel-9"`, 1))
	if _, err := tool.Render(dir, true); code(err) != DiagRunInvalid {
		t.Fatalf("unknown authority device accepted: %v", err)
	}
	write(RunFile, runJSON)
	write(LanesFile, strings.Replace(lanes, `"refunds"`, `"../escape"`, 1))
	if _, err := tool.Render(dir, true); code(err) != DiagRunInvalid {
		t.Fatalf("path-escaping slug accepted: %v", err)
	}
}
