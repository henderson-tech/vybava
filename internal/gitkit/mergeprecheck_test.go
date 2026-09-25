package gitkit

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func checks(t *testing.T, rollup string) string {
	t.Helper()
	var nodes []checkNode
	if err := json.Unmarshal([]byte(rollup), &nodes); err != nil {
		t.Fatal(err)
	}
	return summarizeChecks(nodes)
}

func TestSummarizeChecks(t *testing.T) {
	for rollup, want := range map[string]string{
		`null`: "NONE", `[]`: "NONE",
		`[{"status":"COMPLETED","conclusion":"SUCCESS"},{"status":"COMPLETED","conclusion":"FAILURE"}]`: "FAILURE",
		`[{"status":"COMPLETED","conclusion":"SUCCESS"},{"status":"IN_PROGRESS","conclusion":""}]`:      "PENDING",
		`[{"status":"COMPLETED","conclusion":"SUCCESS"},{"status":"COMPLETED","conclusion":"SUCCESS"}]`: "SUCCESS",
		// A StatusContext carries its terminal result in state, with no
		// CheckRun status/conclusion fields.
		`[{"state":"SUCCESS"}]`: "SUCCESS",
		`[{"status":"COMPLETED","conclusion":"SUCCESS"},{"status":"COMPLETED","conclusion":"SKIPPED"},{"state":"SUCCESS"}]`: "SUCCESS",
		`[{"state":"PENDING"}]`: "PENDING", `[{"state":"EXPECTED"}]`: "PENDING",
		`[{"state":"FAILURE"}]`: "FAILURE", `[{"state":"ERROR"}]`: "FAILURE",
	} {
		if got := checks(t, rollup); got != want {
			t.Errorf("summarizeChecks(%s) = %s, want %s", rollup, got, want)
		}
	}
}

func TestSummarizeGates(t *testing.T) {
	clean := gateInput{state: "OPEN", mergeable: "MERGEABLE", mergeStateStatus: "CLEAN", reviewDecision: "APPROVED", checks: "SUCCESS", botApprovalOK: true}
	with := func(edit func(*gateInput)) GateSummary { g := clean; edit(&g); return summarizeGates(g) }
	if g := summarizeGates(clean); !g.AllPass || len(g.Failed) != 0 {
		t.Errorf("clean PR: %+v", g)
	}
	for name, tc := range map[string]struct {
		edit   func(*gateInput)
		failed []string
	}{
		"no review required": {func(g *gateInput) { g.reviewDecision = "" }, []string{}},
		"dirty worktree":     {func(g *gateInput) { g.worktreeDirty = true }, []string{"clean"}},
		"conflicting":        {func(g *gateInput) { g.mergeable = "CONFLICTING" }, []string{"conflict"}},
		"dirty merge state":  {func(g *gateInput) { g.mergeStateStatus = "DIRTY" }, []string{"conflict"}},
		"still computing":    {func(g *gateInput) { g.mergeable = "UNKNOWN" }, []string{"mergeable-unknown"}},
		"pending CI":         {func(g *gateInput) { g.checks = "PENDING" }, []string{"ci"}},
		"changes requested":  {func(g *gateInput) { g.reviewDecision = "CHANGES_REQUESTED" }, []string{"review"}},
		"review required":    {func(g *gateInput) { g.reviewDecision = "REVIEW_REQUIRED" }, []string{"review"}},
		"draft":              {func(g *gateInput) { g.isDraft = true }, []string{"draft"}},
		"merged":             {func(g *gateInput) { g.state = "MERGED" }, []string{"open"}},
		"bot approval":       {func(g *gateInput) { g.botApprovalOK = false }, []string{"botReview"}},
		// NONE passes only where no workflow exists, and absence is reported
		// apart from a red check.
		"no checks, workflows":    {func(g *gateInput) { g.checks, g.hasWorkflows = "NONE", true }, []string{"ci-absent"}},
		"no checks, no workflows": {func(g *gateInput) { g.checks = "NONE" }, []string{}},
		"red, workflows":          {func(g *gateInput) { g.checks, g.hasWorkflows = "FAILURE", true }, []string{"ci"}},
	} {
		if g := with(tc.edit); !slices.Equal(g.Failed, tc.failed) || g.AllPass != (len(tc.failed) == 0) {
			t.Errorf("%s: failed = %v, want %v", name, g.Failed, tc.failed)
		}
	}
}

func TestParseBotList(t *testing.T) {
	for raw, want := range map[string][]string{
		"": {"eve-bot-lovinka[bot]"}, "   ": {"eve-bot-lovinka[bot]"},
		"eve-bot-lovinka[bot], my-ci-machine-user": {"eve-bot-lovinka[bot]", "my-ci-machine-user"},
		"Eve-Bot-Lovinka[bot]":                     {"eve-bot-lovinka[bot]"},
	} {
		if got := parseBotList(raw); !slices.Equal(got, want) {
			t.Errorf("parseBotList(%q) = %v", raw, got)
		}
	}
}

func TestSummarizeBotApproval(t *testing.T) {
	def := []string{"eve-bot-lovinka[bot]"}
	eve := []string{"eve-bot-lovinka"}
	review := func(login, state string) []latestReview { return []latestReview{{login: login, state: state}} }
	for name, tc := range map[string]struct {
		config, requested []string
		reviews           []latestReview
		ok                bool
		required, pending []string
	}{
		"no bots on the PR":           {def, nil, nil, true, []string{}, []string{}},
		"configured but absent":       {eve, nil, nil, true, []string{}, []string{}},
		"requested, not approved":     {eve, []string{"eve-bot-lovinka"}, nil, false, []string{"eve-bot-lovinka"}, []string{"eve-bot-lovinka"}},
		"changes requested":           {eve, nil, review("eve-bot-lovinka", "CHANGES_REQUESTED"), false, []string{"eve-bot-lovinka"}, []string{"eve-bot-lovinka"}},
		"approved":                    {eve, nil, review("eve-bot-lovinka", "APPROVED"), true, []string{"eve-bot-lovinka"}, []string{}},
		"[bot] auto-required":         {def, []string{"some-app[bot]"}, nil, false, []string{"some-app[bot]"}, []string{"some-app[bot]"}},
		"machine user not required":   {def, nil, review("some-machine-user", "COMMENTED"), true, []string{}, []string{}},
		"canonicalized app, pending":  {def, []string{canonicalizeBotLogin("eve-bot-lovinka", "Bot")}, nil, false, []string{"eve-bot-lovinka[bot]"}, []string{"eve-bot-lovinka[bot]"}},
		"canonicalized app, approved": {def, nil, review(canonicalizeBotLogin("eve-bot-lovinka", "Bot"), "APPROVED"), true, []string{"eve-bot-lovinka[bot]"}, []string{}},
		"bare config matches [bot]":   {eve, []string{"eve-bot-lovinka[bot]"}, nil, false, []string{"eve-bot-lovinka[bot]"}, []string{"eve-bot-lovinka[bot]"}},
	} {
		s := summarizeBotApproval(tc.config, tc.requested, tc.reviews)
		if s.OK != tc.ok || !slices.Equal(s.Required, tc.required) || !slices.Equal(s.Pending, tc.pending) {
			t.Errorf("%s: %+v", name, s)
		}
	}
}

// eve's advisory posture posts every verdict as COMMENTED; only an approval
// header approves, and it never rescues CHANGES_REQUESTED.
func TestAdvisoryEveVerdicts(t *testing.T) {
	for _, tc := range []struct {
		body, state string
		want        bool
	}{
		{"## 🐉 eve review — ✅ Approved\n> `29b29e4` · 0 actionable findings", "COMMENTED", true},
		{"## 🐉 eve review — 🟡 Review comments\n> `abc1234` · 2 actionable findings", "COMMENTED", true},
		{"## 🐉 eve review — 🔴 Changes requested\n> 1 blocking finding", "COMMENTED", false},
		{"", "COMMENTED", false},
		{"🐉 **eve review in progress — run 1**", "COMMENTED", false},
		{"## 🐉 eve review — ✅ Approved", "CHANGES_REQUESTED", false},
	} {
		s := summarizeBotApproval([]string{"eve-bot-lovinka[bot]"}, nil, []latestReview{{login: "eve-bot-lovinka[bot]", state: tc.state, body: &tc.body}})
		if s.OK != tc.want {
			t.Errorf("%q/%s → ok %v", tc.body, tc.state, s.OK)
		}
	}
	if !isBotApprovalReview(latestReview{login: "x[bot]", state: "APPROVED"}) {
		t.Error("APPROVED must approve without a body")
	}
}

func TestCanonicalizeBotLogin(t *testing.T) {
	for _, tc := range [][3]string{
		{"eve-bot-lovinka", "Bot", "eve-bot-lovinka[bot]"}, {"SomeApp", "Bot", "someapp[bot]"},
		{"already[bot]", "Bot", "already[bot]"}, {"LEFTEQ", "User", "lefteq"}, {"LEFTEQ", "", "lefteq"},
	} {
		if got := canonicalizeBotLogin(tc[0], tc[1]); got != tc[2] {
			t.Errorf("canonicalizeBotLogin(%s, %s) = %s", tc[0], tc[1], got)
		}
	}
}

func TestSubstituteHookTokens(t *testing.T) {
	ctx := hookContext{slug: "pr-266", branch: "feat-x", worktree: "/r/.worktrees/pr-266", pr: 266}
	noPR := hookContext{slug: "wk-quiesce", branch: "work/wk-quiesce", worktree: "/r/.worktrees/wk-quiesce"}
	for _, tc := range []struct {
		cmd  string
		ctx  hookContext
		want string
	}{
		{"/wk:cleanup {slug} --remove --yes --delete-remote", ctx, "/wk:cleanup pr-266 --remove --yes --delete-remote"},
		{"{slug} {branch} {worktree} {pr}", ctx, "pr-266 feat-x /r/.worktrees/pr-266 266"},
		{"echo {pr}-{pr}", ctx, "echo 266-266"},
		{"docker compose down -v", ctx, "docker compose down -v"},
		// the quiesce hook resolves before a PR exists (pr = 0)
		{"/wk:pause {slug}", noPR, "/wk:pause wk-quiesce"},
		{"cmd --pr {pr}", noPR, "cmd --pr 0"},
	} {
		if got := substituteHookTokens(tc.cmd, tc.ctx); got != tc.want {
			t.Errorf("substituteHookTokens(%q) = %q", tc.cmd, got)
		}
	}
	// Both hooks coexist in one config; comments between them swallow nothing.
	cfg := parseConfig("# post-merge teardown\nAFTER_MERGE_CMD=/wk:cleanup {slug} --remove --yes --delete-remote\n\n# pre-review quiesce\nBEFORE_REVIEW_CMD=/wk:pause {slug}\n")
	if cfg["AFTER_MERGE_CMD"] != "/wk:cleanup {slug} --remove --yes --delete-remote" || cfg["BEFORE_REVIEW_CMD"] != "/wk:pause {slug}" {
		t.Errorf("both hooks: %v", cfg)
	}
}

func invalid(p policy) string {
	if p.invalid == nil {
		return "<nil>"
	}
	return *p.invalid
}

func TestEnumConfigKeys(t *testing.T) {
	for _, tc := range []struct {
		name       string
		got        policy
		value, bad string
	}{
		{"policy absent", parseMergePolicy(gitConfig{}), "review", "<nil>"},
		{"policy self", parseMergePolicy(gitConfig{"MERGE_POLICY": " Self "}), "self", "<nil>"},
		{"policy typo", parseMergePolicy(gitConfig{"MERGE_POLICY": "auto"}), "review", "auto"},
		// a typo must never widen a kill
		{"stop absent", parseStopServers(gitConfig{}), "worktree", "<nil>"},
		{"stop repo", parseStopServers(gitConfig{"AFTER_MERGE_STOP_SERVERS": " Repo "}), "repo", "<nil>"},
		{"stop none", parseStopServers(gitConfig{"AFTER_MERGE_STOP_SERVERS": "none"}), "none", "<nil>"},
		{"stop typo", parseStopServers(gitConfig{"AFTER_MERGE_STOP_SERVERS": "all"}), "worktree", "all"},
	} {
		if tc.got.value != tc.value || invalid(tc.got) != tc.bad {
			t.Errorf("%s: %s / %s", tc.name, tc.got.value, invalid(tc.got))
		}
	}
}

func TestResolveDefaultBranch(t *testing.T) {
	for _, tc := range []struct {
		cfg          gitConfig
		github, want string
	}{
		{gitConfig{}, "master", "master"}, {gitConfig{}, "", "main"},
		// the gitignored .local overlay wins over GitHub's default; blank is no override
		{gitConfig{"DEFAULT_BRANCH": "devlp"}, "main", "devlp"},
		{parseConfig("MERGE_METHOD=squash\nDEFAULT_BRANCH=devlp\n"), "master", "devlp"},
		{gitConfig{"DEFAULT_BRANCH": "  "}, "main", "main"},
	} {
		if got := resolveDefaultBranch(tc.cfg, tc.github); got != tc.want {
			t.Errorf("resolveDefaultBranch(%v, %q) = %q", tc.cfg, tc.github, got)
		}
	}
}

// The teardown hook is resolved only for the linked worktree that holds the
// PR's head branch — never for the main clone the caller's --repo names,
// which is what the cwd fallback lands on (FixIt PR #896).
func TestTeardownHookTargetsOnlyTheHeadWorktree(t *testing.T) {
	main := t.TempDir()
	run := func(dir string, args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run(main, "init", "-q", "-b", "main")
	run(main, "commit", "-q", "--allow-empty", "-m", "c1")
	wt := filepath.Join(main, ".worktrees", "pr-7")
	run(main, "worktree", "add", "-q", "-b", "feat/x", wt)
	git := func(a ...string) (string, error) { return execFile(execOpts{dir: main}, "git", a...) }
	cfg := gitConfig{"AFTER_MERGE_CMD": "/wk:cleanup {slug} --remove --yes"}

	paths, err := gatherPaths(git, "feat/x")
	if err != nil {
		t.Fatal(err)
	}
	top, _ := git("rev-parse", "--show-toplevel")
	top = strings.TrimSpace(top)
	if paths.mainClone != top || paths.worktree != filepath.Join(top, ".worktrees", "pr-7") || paths.branch != "feat/x" || !paths.isWorktree || paths.dirty {
		t.Fatalf("head worktree paths = %+v", paths)
	}
	hook := hookContext{slug: filepath.Base(paths.worktree), branch: paths.branch, worktree: paths.worktree, pr: 7}
	if _, resolved := afterMergeHook(cfg, paths, hook); resolved == nil || *resolved != "/wk:cleanup pr-7 --remove --yes" {
		t.Fatalf("resolved hook = %v", resolved)
	}

	// The cleanOk gate guards the directory about to be removed.
	os.WriteFile(filepath.Join(wt, "scratch.txt"), []byte("x"), 0o644)
	if paths, _ := gatherPaths(git, "feat/x"); !paths.dirty {
		t.Error("an untracked file in the head worktree must read dirty")
	}

	// No worktree holds the branch: the main clone comes back, and the hook
	// must resolve to nothing rather than tear it down.
	paths, err = gatherPaths(git, "feat/gone")
	if err != nil {
		t.Fatal(err)
	}
	cmd, resolved := afterMergeHook(cfg, paths, hookContext{slug: filepath.Base(paths.worktree), worktree: paths.worktree})
	if paths.isWorktree || paths.worktree != top || cmd == nil || resolved != nil {
		t.Fatalf("fallback: paths %+v, cmd %v, resolved %v", paths, cmd, resolved)
	}
}

// rulesOf is a base branch's merge rules as resolveMergeMethod sees them;
// every method is on unless edited.
func rulesOf(edit func(*mergeRules)) mergeRules {
	r := mergeRules{base: "main", buttons: map[string]bool{"merge": true, "squash": true, "rebase": true}}
	if edit != nil {
		edit(&r)
	}
	return r
}

func TestResolveMergeMethod(t *testing.T) {
	linear := rulesOf(func(r *mergeRules) { r.linearHistory = []string{`org ruleset "org main"`} })
	for _, tc := range []struct {
		name, raw     string
		rules         mergeRules
		method, bad   string
		source        string
		allowed       []string
		reasonPattern string
	}{
		{"explicit permitted wins", " Squash ", rulesOf(nil), "squash", "<nil>", "config", []string{"merge", "squash", "rebase"}, ""},
		{"unset keeps merge", "", rulesOf(nil), "merge", "<nil>", "repository", nil, "main permits every method → merge"},
		{"typo echoed", "fast-forward", rulesOf(nil), "merge", "fast-forward", "repository", nil, "MERGE_METHOD=fast-forward is not a merge method"},
		// semafor#3: linear history refused merge commits while the button was on
		{"linear history", "", linear, "squash", "<nil>", "repository", []string{"squash", "rebase"}, `org ruleset "org main" requires linear history on main → squash`},
		{"refused explicit is drift", "merge", linear, "squash", "merge", "repository", nil, "MERGE_METHOD=merge is refused"},
		{"squash-only buttons", "", rulesOf(func(r *mergeRules) { r.buttons = map[string]bool{"squash": true} }), "squash", "<nil>", "repository", nil, ""},
		{"ruleset narrows", "", rulesOf(func(r *mergeRules) { r.rulesetMethods = []rulesetMethods{{`repo ruleset "main"`, []string{"rebase"}}} }), "rebase", "<nil>", "repository", nil, ""},
	} {
		c, err := resolveMergeMethod(gitConfig{"MERGE_METHOD": tc.raw}, tc.rules, "feat/x")
		if tc.raw == "" {
			c, err = resolveMergeMethod(gitConfig{}, tc.rules, "feat/x")
		}
		if err != nil || c.method != tc.method || invalid(policy{invalid: c.invalid}) != tc.bad || c.source != tc.source ||
			(tc.allowed != nil && !slices.Equal(c.allowed, tc.allowed)) || !strings.Contains(c.reason, tc.reasonPattern) {
			t.Errorf("%s: %+v %v", tc.name, c, err)
		}
	}
	deadlock := rulesOf(func(r *mergeRules) {
		r.buttons = map[string]bool{"merge": true}
		r.linearHistory = []string{"branch protection"}
	})
	if _, err := resolveMergeMethod(gitConfig{"MERGE_METHOD": "merge"}, deadlock, "feat/x"); err == nil ||
		!strings.Contains(err.Error(), "no merge method is permitted into main (merge: branch protection requires linear history") {
		t.Errorf("deadlock: %v", err)
	}
}

// MERGE_METHOD_BY_HEAD: a promotion lands as a merge commit by default, every
// other head keeps MERGE_METHOD, and a base refusing the matched method is a
// STOP — never the squash fallback.
func TestHeadMergeMethod(t *testing.T) {
	squash := gitConfig{"MERGE_METHOD": "squash"}
	if c, err := resolveMergeMethod(squash, rulesOf(nil), "promote/canary-20260925"); err != nil || c.method != "merge" || c.source != "head" ||
		!strings.Contains(c.reason, "matches MERGE_METHOD_BY_HEAD promote/*:merge") {
		t.Errorf("promotion: %+v %v", c, err)
	}
	if c, err := resolveMergeMethod(squash, rulesOf(nil), "feat/x"); err != nil || c.method != "squash" || c.source != "config" {
		t.Errorf("feature: %+v %v", c, err)
	}
	linear := rulesOf(func(r *mergeRules) { r.linearHistory = []string{`repo ruleset "canary"`} })
	if c, err := resolveMergeMethod(squash, linear, "promote/canary-20260925"); err == nil ||
		!strings.Contains(err.Error(), `STOP — promote/canary-20260925 must land as merge`) || !strings.Contains(err.Error(), `requires linear history`) {
		t.Errorf("refused promotion fell back: %+v %v", c, err)
	}
	custom := gitConfig{"MERGE_METHOD": "squash", "MERGE_METHOD_BY_HEAD": "release/*:rebase, promote/*:merge"}
	if c, err := resolveMergeMethod(custom, rulesOf(nil), "release/2026-10"); err != nil || c.method != "rebase" {
		t.Errorf("first match: %+v %v", c, err)
	}
	off := gitConfig{"MERGE_METHOD": "squash", "MERGE_METHOD_BY_HEAD": ""}
	if c, err := resolveMergeMethod(off, rulesOf(nil), "promote/x"); err != nil || c.method != "squash" {
		t.Errorf("empty value turns it off: %+v %v", c, err)
	}
	for _, bad := range []string{"promote/*", "promote/*:ff", ":merge", "promote/[:merge"} {
		if _, err := resolveMergeMethod(gitConfig{"MERGE_METHOD_BY_HEAD": bad}, rulesOf(nil), "feat/x"); err == nil {
			t.Errorf("malformed %q accepted", bad)
		}
	}
}

// repoNode is the gate query's repository node, shaped as GitHub returns it.
func repoNode(t *testing.T, buttons, baseRef string) gateRepository {
	t.Helper()
	var r gateRepository
	if err := json.Unmarshal([]byte(`{`+buttons+`"pullRequest":{"baseRefName":"main","baseRef":`+baseRef+`}}`), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

const onOnOff = `"mergeCommitAllowed":true,"squashMergeAllowed":true,"rebaseMergeAllowed":false,`

func TestReadMergeRules(t *testing.T) {
	// org/repo rulesets and classic protection are read; evaluate-mode rules never refuse
	got, err := readMergeRules(repoNode(t, onOnOff, `{"branchProtectionRule":{"requiresLinearHistory":true},"rules":{"totalCount":4,"nodes":[
		{"type":"REQUIRED_LINEAR_HISTORY","parameters":{},"repositoryRuleset":{"name":"org main","enforcement":"ACTIVE","source":{"__typename":"Organization"}}},
		{"type":"PULL_REQUEST","parameters":{"allowedMergeMethods":["MERGE","SQUASH"]},"repositoryRuleset":{"name":"repo main","enforcement":"ACTIVE","source":{"__typename":"Repository"}}},
		{"type":"REQUIRED_LINEAR_HISTORY","parameters":{},"repositoryRuleset":{"name":"trial","enforcement":"EVALUATE","source":{"__typename":"Repository"}}},
		{"type":"DELETION","parameters":{},"repositoryRuleset":{"name":"org main","enforcement":"ACTIVE","source":{"__typename":"Organization"}}}]}}`), "acme/app")
	if err != nil || got.base != "main" || !got.buttons["merge"] || !got.buttons["squash"] || got.buttons["rebase"] ||
		!slices.Equal(got.linearHistory, []string{"branch protection", `org ruleset "org main"`}) ||
		len(got.rulesetMethods) != 1 || got.rulesetMethods[0].by != `repo ruleset "repo main"` || !slices.Equal(got.rulesetMethods[0].methods, []string{"merge", "squash"}) {
		t.Fatalf("readMergeRules = %+v, %v", got, err)
	}
	// unreadable settings or rules fail with the fix instead of guessing
	for _, tc := range []struct{ buttons, baseRef, want string }{
		{onOnOff, `null`, "cannot read the base branch's rules for acme/app (into main)"},
		{``, `{"rules":{"totalCount":0,"nodes":[]}}`, "cannot read the repository's merge settings"},
		{onOnOff, `{"rules":{"totalCount":101,"nodes":[]}}`, "all 101 base-branch rules"},
	} {
		if _, err := readMergeRules(repoNode(t, tc.buttons, tc.baseRef), "acme/app"); err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "gh auth status") {
			t.Errorf("%s: %v", tc.want, err)
		}
	}
}
