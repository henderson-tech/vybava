package gitkit

import (
	"encoding/json"
	"slices"
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
	yes, no := true, false
	squashOnly := &allowedMergeMethods{merge: &no, squash: &yes, rebase: &no}
	all := &allowedMergeMethods{merge: &yes, squash: &yes, rebase: &yes}
	for _, tc := range []struct {
		name       string
		got        policy
		value, bad string
	}{
		{"method absent", parseMergeMethod(gitConfig{}, nil), "merge", "<nil>"},
		{"method squash", parseMergeMethod(gitConfig{"MERGE_METHOD": " Squash "}, nil), "squash", "<nil>"},
		{"method rebase", parseMergeMethod(gitConfig{"MERGE_METHOD": "rebase"}, nil), "rebase", "<nil>"},
		{"method typo", parseMergeMethod(gitConfig{"MERGE_METHOD": "fast-forward"}, nil), "merge", "fast-forward"},
		// a squash-only repo needs no config; a disabled explicit method is drift
		{"squash-only default", parseMergeMethod(gitConfig{}, squashOnly), "squash", "<nil>"},
		{"squash-only refuses merge", parseMergeMethod(gitConfig{"MERGE_METHOD": "merge"}, squashOnly), "squash", "merge"},
		{"all allowed default", parseMergeMethod(gitConfig{}, all), "merge", "<nil>"},
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
