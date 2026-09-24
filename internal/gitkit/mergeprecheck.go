package gitkit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// merge-precheck — read-only pre-merge gate gathering for /prm's terminus.

// hookContext fills the {slug}/{branch}/{worktree}/{pr} tokens of a
// configured AFTER_MERGE_CMD or BEFORE_REVIEW_CMD.
type hookContext struct {
	slug, branch, worktree string
	pr                     int
}

// substituteHookTokens replaces every occurrence of each token, in order; a
// command with no tokens is returned unchanged.
func substituteHookTokens(cmd string, ctx hookContext) string {
	cmd = strings.ReplaceAll(cmd, "{slug}", ctx.slug)
	cmd = strings.ReplaceAll(cmd, "{branch}", ctx.branch)
	cmd = strings.ReplaceAll(cmd, "{worktree}", ctx.worktree)
	return strings.ReplaceAll(cmd, "{pr}", strconv.Itoa(ctx.pr))
}

// resolveDefaultBranch: the branch a green PR lands on. GitHub's default is
// the team-wide answer; a machine adopting a new integration branch ahead of
// the team overrides it with DEFAULT_BRANCH in the gitignored
// .claude.git.config.local. Blank/absent → GitHub's, else main.
func resolveDefaultBranch(cfg gitConfig, githubDefault string) string {
	if override := strings.TrimSpace(cfg["DEFAULT_BRANCH"]); override != "" {
		return override
	}
	if d := strings.TrimSpace(githubDefault); d != "" {
		return d
	}
	return "main"
}

// checkNode is one statusCheckRollup entry: a CheckRun (status/conclusion)
// or a commit StatusContext (state only).
type checkNode struct {
	State      *string `json:"state"`
	Conclusion *string `json:"conclusion"`
	Status     *string `json:"status"`
}

func upper(s *string) string {
	if s == nil {
		return ""
	}
	return strings.ToUpper(*s)
}

// summarizeChecks folds the rollup into SUCCESS / PENDING / FAILURE / NONE.
func summarizeChecks(rollup []checkNode) string {
	if len(rollup) == 0 {
		return "NONE"
	}
	pending := false
	for _, n := range rollup {
		// A StatusContext (e.g. a review bot's "Review completed") has no
		// CheckRun lifecycle: its state IS the terminal result. Through the
		// CheckRun logic a green one (state=SUCCESS ≠ COMPLETED) reads PENDING.
		if n.Status == nil && n.Conclusion == nil {
			switch st := upper(n.State); {
			case st == "FAILURE" || st == "ERROR":
				return "FAILURE"
			case st != "" && st != "SUCCESS":
				pending = true // PENDING / EXPECTED / anything non-terminal
			}
			continue
		}
		concl := upper(n.Conclusion)
		status := upper(n.Status)
		if status == "" {
			status = upper(n.State)
		}
		if slices.Contains([]string{"FAILURE", "ERROR", "CANCELLED", "TIMED_OUT", "ACTION_REQUIRED", "STARTUP_FAILURE"}, concl) ||
			status == "FAILURE" || status == "ERROR" {
			return "FAILURE"
		}
		if slices.Contains([]string{"QUEUED", "IN_PROGRESS", "PENDING", "WAITING", "REQUESTED"}, status) || (status != "COMPLETED" && concl == "") {
			pending = true
		}
	}
	if pending {
		return "PENDING"
	}
	return "SUCCESS"
}

type gateInput struct {
	state, mergeable, mergeStateStatus, reviewDecision, checks string
	worktreeDirty, isDraft, botApprovalOK                      bool
	hasWorkflows                                               bool // does the repo define any GitHub Actions workflow?
}

// GateSummary is the gates object, keys in wire order.
type GateSummary struct {
	OpenOK        bool     `json:"openOk"`
	DraftOK       bool     `json:"draftOk"`
	CleanOK       bool     `json:"cleanOk"`
	MergeableOK   bool     `json:"mergeableOk"`
	CIOK          bool     `json:"ciOk"`
	ApprovedOK    bool     `json:"approvedOk"`
	BotApprovalOK bool     `json:"botApprovalOk"`
	AllPass       bool     `json:"allPass"`
	Failed        []string `json:"failed"`
}

// repoHasWorkflows: at least one .yml/.yaml under .github/workflows.
func repoHasWorkflows(mainClone string) (bool, error) {
	dir := filepath.Join(mainClone, ".github", "workflows")
	if !exists(dir) {
		return false, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(entries, func(e os.DirEntry) bool {
		return strings.HasSuffix(e.Name(), ".yml") || strings.HasSuffix(e.Name(), ".yaml")
	}), nil
}

func summarizeGates(g gateInput) GateSummary {
	s := GateSummary{
		OpenOK:  strings.ToUpper(g.state) == "OPEN",
		DraftOK: !g.isDraft,
		CleanOK: !g.worktreeDirty,
		// UNKNOWN is GitHub still computing (normal for seconds after every
		// push) — not a pass: an auto-merge must never race the conflict check.
		MergeableOK: strings.ToUpper(g.mergeable) == "MERGEABLE" && strings.ToUpper(g.mergeStateStatus) != "DIRTY",
		// NONE (no checks at all) passes only where no workflow exists. In a
		// repo WITH workflows it means path filters matched nothing, a run got
		// no runner, or nothing triggered yet — "no test ran" must never read
		// as "every test passed".
		CIOK:          g.checks == "SUCCESS" || (g.checks == "NONE" && !g.hasWorkflows),
		BotApprovalOK: g.botApprovalOK,
		Failed:        []string{},
	}
	rd := strings.ToUpper(g.reviewDecision)
	s.ApprovedOK = rd == "APPROVED" || rd == ""
	if !s.OpenOK {
		s.Failed = append(s.Failed, "open")
	}
	if !s.DraftOK {
		s.Failed = append(s.Failed, "draft")
	}
	if !s.CleanOK {
		s.Failed = append(s.Failed, "clean")
	}
	if !s.MergeableOK {
		if strings.ToUpper(g.mergeable) == "UNKNOWN" {
			s.Failed = append(s.Failed, "mergeable-unknown")
		} else {
			s.Failed = append(s.Failed, "conflict")
		}
	}
	// "ci" is a red check, "ci-absent" no check at all: the fix differs
	// (rerun vs. work out why nothing ran).
	if !s.CIOK {
		if g.checks == "NONE" {
			s.Failed = append(s.Failed, "ci-absent")
		} else {
			s.Failed = append(s.Failed, "ci")
		}
	}
	if !s.ApprovedOK {
		s.Failed = append(s.Failed, "review")
	}
	if !s.BotApprovalOK {
		s.Failed = append(s.Failed, "botReview")
	}
	s.AllPass = len(s.Failed) == 0
	return s
}

// --- Bot-approval gate -------------------------------------------------------
// A required bot must approve before a PR is mergeable: its LATEST review is
// APPROVED (or an advisory COMMENTED verdict carrying an approval marker).
// Required = REQUIRED_BOT_REVIEWERS (default eve-bot-lovinka[bot]) ∪ any
// `[bot]` reviewer — but a bot only GATES when it is on the PR (requested,
// or has reviewed). A bot absent from the PR never blocks.

type latestReview struct {
	login, state string
	body         *string
}

// eve's `verdictPosture: advisory` repos post EVERY verdict as a neutral
// COMMENTED review; only the header carries the outcome. These are the
// headers of the verdicts a gating repo posts as APPROVE. "🔴 Changes
// requested" is deliberately absent.
var advisoryApprovalMarkers = []string{"eve review — ✅ Approved", "eve review — 🟡 Review comments"}

func isBotApprovalReview(r latestReview) bool {
	if r.state == "APPROVED" {
		return true
	}
	if r.state != "COMMENTED" || r.body == nil || *r.body == "" {
		return false
	}
	return slices.ContainsFunc(advisoryApprovalMarkers, func(m string) bool { return strings.Contains(*r.body, m) })
}

// BotApproval is the botApproval object, keys in wire order.
type BotApproval struct {
	OK       bool     `json:"ok"`
	Required []string `json:"required"`
	Pending  []string `json:"pending"`
}

// policy is a parsed enum-valued config key: the value in force and the raw
// value when it was rejected (echoed so the caller flags the typo).
type policy struct {
	value   string
	invalid *string
}

// parseEnum reads an optional enum key: absent/blank → fallback, a known
// value (trimmed, lowercased) → itself, anything else → fallback + echo.
func parseEnum(cfg gitConfig, key string, known []string, fallback string) policy {
	raw := cfg[key]
	v := strings.ToLower(strings.TrimSpace(raw))
	if v == "" {
		return policy{value: fallback}
	}
	if slices.Contains(known, v) {
		return policy{value: v}
	}
	return policy{value: fallback, invalid: &raw}
}

// MERGE_POLICY: review (default) waits for the review loop; self is a
// solo-owner repo — ensure-pr labels the PR eve-ignore and merge.md
// admin-merges once clean + CI + no-conflict pass.
func parseMergePolicy(cfg gitConfig) policy {
	return parseEnum(cfg, "MERGE_POLICY", []string{"review", "self"}, "review")
}

// AFTER_MERGE_STOP_SERVERS: which dev servers teardown stops — worktree
// (default), repo (also the main clone's), none. A typo → worktree, the
// conservative reading, never a silently widened kill.
func parseStopServers(cfg gitConfig) policy {
	return parseEnum(cfg, "AFTER_MERGE_STOP_SERVERS", []string{"worktree", "repo", "none"}, "worktree")
}

// allowedMergeMethods is what the repository permits; nil = unknown.
type allowedMergeMethods struct{ merge, squash, rebase *bool }

func (a *allowedMergeMethods) get(method string) *bool {
	switch method {
	case "merge":
		return a.merge
	case "squash":
		return a.squash
	}
	return a.rebase
}

// MERGE_METHOD: which `gh pr merge` flag lands the PR. When the repository's
// allowed methods are known the default is derived from them (merge where
// offered, else squash, else rebase), so a squash-only repo needs no config.
// An explicit method the repository has disabled is drift: fall back, echo.
func parseMergeMethod(cfg gitConfig, allowed *allowedMergeMethods) policy {
	on := func(b *bool) bool { return b != nil && *b }
	fallback := "merge"
	if allowed != nil && !on(allowed.merge) {
		switch {
		case on(allowed.squash):
			fallback = "squash"
		case on(allowed.rebase):
			fallback = "rebase"
		}
	}
	p := parseEnum(cfg, "MERGE_METHOD", []string{"merge", "squash", "rebase"}, fallback)
	if p.invalid == nil && allowed != nil && strings.TrimSpace(cfg["MERGE_METHOD"]) != "" {
		if b := allowed.get(p.value); b != nil && !*b {
			raw := cfg["MERGE_METHOD"]
			return policy{value: fallback, invalid: &raw}
		}
	}
	return p
}

var botListSeparator = regexp.MustCompile(`[,\s]+`)

// parseBotList: REQUIRED_BOT_REVIEWERS, comma/space separated, lowercased.
// Absent/empty → eve-bot-lovinka[bot].
func parseBotList(raw string) []string {
	items := []string{}
	for _, s := range botListSeparator.Split(raw, -1) {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			items = append(items, s)
		}
	}
	if len(items) == 0 {
		return []string{"eve-bot-lovinka[bot]"}
	}
	return items
}

func stripBot(login string) string { return strings.TrimSuffix(login, "[bot]") }

// canonicalizeBotLogin: GitHub's GraphQL returns a Bot's login WITHOUT the
// `[bot]` suffix REST and the UI show; restore it from __typename so config
// and auto-detection key off one identity. Users are only lowercased.
func canonicalizeBotLogin(login, typename string) string {
	l := strings.ToLower(login)
	if typename == "Bot" && !strings.HasSuffix(l, "[bot]") {
		return l + "[bot]"
	}
	return l
}

// summarizeBotApproval: a login is a bot when it carries `[bot]` or config
// names it (machine-USER bots have no suffix); config matching ignores the
// suffix on both sides.
func summarizeBotApproval(configList, requested []string, reviews []latestReview) BotApproval {
	configSet := map[string]bool{}
	for _, s := range configList {
		configSet[stripBot(strings.ToLower(s))] = true
	}
	isBot := func(login string) bool { return strings.HasSuffix(login, "[bot]") || configSet[stripBot(login)] }
	present := []string{}
	add := func(login string) {
		if isBot(login) && !slices.Contains(present, login) {
			present = append(present, login)
		}
	}
	for _, r := range requested {
		add(r)
	}
	for _, r := range reviews {
		add(r.login)
	}
	pending := []string{}
	for _, bot := range present {
		i := slices.IndexFunc(reviews, func(r latestReview) bool { return r.login == bot })
		if i == -1 || !isBotApprovalReview(reviews[i]) {
			pending = append(pending, bot)
		}
	}
	required := slices.Clone(present)
	slices.Sort(required)
	slices.Sort(pending)
	return BotApproval{OK: len(pending) == 0, Required: required, Pending: pending}
}

// One GraphQL round for everything the bot-approval gate needs: requested
// reviewers and the latest review per author (first 50 each, no pagination).
const gateQuery = `
query($owner:String!, $repo:String!, $pr:Int!) {
  repository(owner:$owner, name:$repo) {
    pullRequest(number:$pr) {
      reviewRequests(first:50) {
        nodes { requestedReviewer { __typename ... on User { login } ... on Bot { login } ... on Mannequin { login } } }
      }
      latestReviews(first:50) { nodes { author { login __typename } state body } }
    }
  }
}`

type actor struct {
	Login    string `json:"login"`
	Typename string `json:"__typename"`
}

func gatherBotData(run func(args ...string) (string, error), owner, repo string, pr int) ([]string, []latestReview, error) {
	out, err := run("api", "graphql", "-f", "query="+gateQuery, "-f", "owner="+owner, "-f", "repo="+repo, "-F", "pr="+strconv.Itoa(pr))
	if err != nil {
		return nil, nil, err
	}
	var resp struct {
		Data *struct {
			Repository *struct {
				PullRequest *struct {
					ReviewRequests *struct {
						Nodes []struct {
							RequestedReviewer *actor `json:"requestedReviewer"`
						} `json:"nodes"`
					} `json:"reviewRequests"`
					LatestReviews *struct {
						Nodes []struct {
							Author *actor `json:"author"`
							State  any    `json:"state"`
							Body   any    `json:"body"`
						} `json:"nodes"`
					} `json:"latestReviews"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		return nil, nil, err
	}
	if resp.Data == nil || resp.Data.Repository == nil || resp.Data.Repository.PullRequest == nil {
		return nil, nil, errors.New("graphql response carries no pullRequest")
	}
	node := resp.Data.Repository.PullRequest
	requested := []string{}
	reviews := []latestReview{}
	if node.ReviewRequests != nil {
		for _, n := range node.ReviewRequests.Nodes {
			if rr := n.RequestedReviewer; rr != nil && rr.Login != "" {
				requested = append(requested, canonicalizeBotLogin(rr.Login, rr.Typename))
			}
		}
	}
	if node.LatestReviews != nil {
		for _, n := range node.LatestReviews.Nodes {
			if n.Author == nil || n.Author.Login == "" {
				continue
			}
			r := latestReview{login: canonicalizeBotLogin(n.Author.Login, n.Author.Typename)}
			if n.State != nil {
				r.state = strings.ToUpper(fmt.Sprint(n.State))
			}
			if body, ok := n.Body.(string); ok {
				r.body = &body
			}
			reviews = append(reviews, r)
		}
	}
	return requested, reviews, nil
}

type prPaths struct {
	worktree, branch, mainClone string
	dirty, isWorktree           bool
}

// gatherPaths resolves the worktree holding the PR's head branch. Resolving
// it from the ambient cwd — or from --repo, which /prm pins to the MAIN
// clone by design — reports the main checkout whenever the caller is not
// inside the worktree, and AFTER_MERGE_CMD then resolves to a teardown of
// the one directory it must never touch (FixIt PR #896, 2026-08-02). The
// cleanOk gate guards the directory about to be REMOVED.
func gatherPaths(git func(args ...string) (string, error), headRef string) (prPaths, error) {
	cwdTop, err := git("rev-parse", "--show-toplevel")
	if err != nil {
		return prPaths{}, err
	}
	listing, err := git("worktree", "list", "--porcelain")
	if err != nil {
		return prPaths{}, err
	}
	cwdTop, listing = strings.TrimSpace(cwdTop), strings.TrimSpace(listing)
	lines := strings.Split(listing, "\n")
	// The main clone is the FIRST entry (the primary worktree).
	mainClone := cwdTop
	for _, l := range lines {
		if path, ok := strings.CutPrefix(l, "worktree "); ok {
			mainClone = strings.TrimSpace(path)
			break
		}
	}
	worktree := cwdTop
	if headRef != "" {
		current := ""
		for _, l := range lines {
			if path, ok := strings.CutPrefix(l, "worktree "); ok {
				current = strings.TrimSpace(path)
			} else if current != "" && strings.TrimSpace(l) == "branch refs/heads/"+headRef {
				worktree = current
				break
			}
		}
	}
	branch, err := git("-C", worktree, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return prPaths{}, err
	}
	status, err := git("-C", worktree, "status", "--porcelain")
	if err != nil {
		return prPaths{}, err
	}
	return prPaths{
		worktree: worktree, branch: strings.TrimSpace(branch), mainClone: mainClone,
		dirty: strings.TrimSpace(status) != "", isWorktree: worktree != mainClone,
	}, nil
}

// Precheck is merge-precheck's output, keys in wire order.
type Precheck struct {
	Owner                   string          `json:"owner"`
	Repo                    string          `json:"repo"`
	PR                      int             `json:"pr"`
	URL                     string          `json:"url"`
	Title                   string          `json:"title"`
	HeadRef                 string          `json:"headRef"`
	BaseRef                 string          `json:"baseRef"`
	Branch                  string          `json:"branch"`
	DefaultBranch           string          `json:"defaultBranch"`
	OnDefaultBranch         bool            `json:"onDefaultBranch"`
	Worktree                string          `json:"worktree"`
	MainClone               string          `json:"mainClone"`
	IsWorktree              bool            `json:"isWorktree"`
	Slug                    string          `json:"slug"`
	Checks                  string          `json:"checks"`
	Gates                   GateSummary     `json:"gates"`
	BotApproval             BotApproval     `json:"botApproval"`
	RequiredBotReviewers    []string        `json:"requiredBotReviewers"`
	MergePolicy             string          `json:"mergePolicy"`
	MergePolicyInvalid      *string         `json:"mergePolicyInvalid"`
	MergeMethod             string          `json:"mergeMethod"`
	MergeMethodInvalid      *string         `json:"mergeMethodInvalid"`
	AfterMergeCmd           *string         `json:"afterMergeCmd"`
	ResolvedAfterMergeCmd   *string         `json:"resolvedAfterMergeCmd"`
	StopServers             string          `json:"stopServers"`
	StopServersInvalid      *string         `json:"stopServersInvalid"`
	BeforeReviewCmd         *string         `json:"beforeReviewCmd"`
	ResolvedBeforeReviewCmd *string         `json:"resolvedBeforeReviewCmd"`
	Raw                     precheckRawView `json:"raw"`
}

// precheckRawView echoes gh's values untouched (null stays null).
type precheckRawView struct {
	State            json.RawMessage `json:"state"`
	Mergeable        json.RawMessage `json:"mergeable"`
	MergeStateStatus json.RawMessage `json:"mergeStateStatus"`
	ReviewDecision   json.RawMessage `json:"reviewDecision"`
	IsDraft          json.RawMessage `json:"isDraft"`
}

func rawString(m json.RawMessage) string {
	var s string
	_ = json.Unmarshal(m, &s)
	return s
}

func runMergePrecheck(args []string, stdout, stderr io.Writer) int {
	prArg := ""
	for _, a := range args {
		if !strings.HasPrefix(a, "--") {
			prArg = a
			break
		}
	}
	root, err := repoRoot(args)
	if err != nil {
		return fail(stderr, err)
	}
	opts := execOpts{dir: root, echo: stderr, timeout: 60 * time.Second}
	gh := func(a ...string) (string, error) { return execFile(opts, "gh", a...) }
	git := func(a ...string) (string, error) { return execFile(opts, "git", a...) }

	repoOut, err := gh("repo", "view", "--json", "nameWithOwner,defaultBranchRef,mergeCommitAllowed,squashMergeAllowed,rebaseMergeAllowed")
	if err != nil {
		return fail(stderr, err)
	}
	var repo struct {
		NameWithOwner    string `json:"nameWithOwner"`
		DefaultBranchRef *struct {
			Name string `json:"name"`
		} `json:"defaultBranchRef"`
		MergeCommitAllowed *bool `json:"mergeCommitAllowed"`
		SquashMergeAllowed *bool `json:"squashMergeAllowed"`
		RebaseMergeAllowed *bool `json:"rebaseMergeAllowed"`
	}
	if err := json.Unmarshal([]byte(repoOut), &repo); err != nil {
		return fail(stderr, err)
	}
	owner, name, _ := strings.Cut(repo.NameWithOwner, "/")

	viewArgs := []string{"pr", "view"}
	if prArg != "" {
		viewArgs = append(viewArgs, prArg)
	}
	prOut, err := gh(append(viewArgs, "--json", "number,state,mergeable,mergeStateStatus,reviewDecision,statusCheckRollup,headRefName,baseRefName,url,title,isDraft")...)
	if err != nil {
		return fail(stderr, err)
	}
	var pr struct {
		precheckRawView
		Number            int         `json:"number"`
		StatusCheckRollup []checkNode `json:"statusCheckRollup"`
		HeadRefName       string      `json:"headRefName"`
		BaseRefName       string      `json:"baseRefName"`
		URL               string      `json:"url"`
		Title             string      `json:"title"`
	}
	if err := json.Unmarshal([]byte(prOut), &pr); err != nil {
		return fail(stderr, err)
	}
	paths, err := gatherPaths(git, pr.HeadRefName)
	if err != nil {
		return fail(stderr, err)
	}
	checks := summarizeChecks(pr.StatusCheckRollup)

	// The required-bot list comes from the SAME config that holds
	// AFTER_MERGE_CMD. Only OPEN PRs are worth a GraphQL round.
	cfg, _ := readGitConfig(paths.mainClone)
	githubDefault := ""
	if repo.DefaultBranchRef != nil {
		githubDefault = repo.DefaultBranchRef.Name
	}
	defaultBranch := resolveDefaultBranch(cfg, githubDefault)
	requiredBots := parseBotList(cfg["REQUIRED_BOT_REVIEWERS"])
	mergePolicy := parseMergePolicy(cfg)
	mergeMethod := parseMergeMethod(cfg, &allowedMergeMethods{merge: repo.MergeCommitAllowed, squash: repo.SquashMergeAllowed, rebase: repo.RebaseMergeAllowed})
	state := rawString(pr.State)
	requested, reviews := []string{}, []latestReview{}
	if strings.ToUpper(state) == "OPEN" {
		if requested, reviews, err = gatherBotData(gh, owner, name, pr.Number); err != nil {
			return fail(stderr, err)
		}
	}
	botApproval := summarizeBotApproval(requiredBots, requested, reviews)
	hasWorkflows, err := repoHasWorkflows(paths.mainClone)
	if err != nil {
		return fail(stderr, err)
	}
	var isDraft bool
	_ = json.Unmarshal(pr.IsDraft, &isDraft)
	gates := summarizeGates(gateInput{
		state: state, mergeable: rawString(pr.Mergeable), mergeStateStatus: rawString(pr.MergeStateStatus),
		reviewDecision: rawString(pr.ReviewDecision), checks: checks, worktreeDirty: paths.dirty, isDraft: isDraft,
		botApprovalOK: botApproval.OK, hasWorkflows: hasWorkflows,
	})

	// AFTER_MERGE_CMD runs INSTEAD of the generic teardown, so it bypasses
	// the isWorktree guard: only ever hand it a real worktree. When no
	// worktree holds the head branch, `worktree` falls back to the cwd
	// checkout — usually the MAIN clone — and a resolved `/wk:cleanup … --remove`
	// would delete the primary checkout. Nothing to tear down → null.
	slug := filepath.Base(paths.worktree)
	hook := hookContext{slug: slug, branch: paths.branch, worktree: paths.worktree, pr: pr.Number}
	out := Precheck{
		Owner: owner, Repo: name, PR: pr.Number, URL: pr.URL, Title: pr.Title,
		HeadRef: pr.HeadRefName, BaseRef: pr.BaseRefName,
		Branch: paths.branch, DefaultBranch: defaultBranch, OnDefaultBranch: paths.branch == defaultBranch,
		Worktree: paths.worktree, MainClone: paths.mainClone, IsWorktree: paths.isWorktree, Slug: slug,
		Checks: checks, Gates: gates, BotApproval: botApproval, RequiredBotReviewers: requiredBots,
		MergePolicy: mergePolicy.value, MergePolicyInvalid: mergePolicy.invalid,
		MergeMethod: mergeMethod.value, MergeMethodInvalid: mergeMethod.invalid,
		StopServers: "", Raw: pr.precheckRawView,
	}
	if cmd, ok := cfg.get("AFTER_MERGE_CMD"); ok {
		out.AfterMergeCmd = &cmd
		if cmd != "" && paths.isWorktree {
			resolved := substituteHookTokens(cmd, hook)
			out.ResolvedAfterMergeCmd = &resolved
		}
	}
	stop := parseStopServers(cfg)
	out.StopServers, out.StopServersInvalid = stop.value, stop.invalid
	// BEFORE_REVIEW_CMD: the pre-review quiesce hook, run on loop entry and
	// each round — idempotent and cheap by contract.
	if cmd, ok := cfg.get("BEFORE_REVIEW_CMD"); ok {
		out.BeforeReviewCmd = &cmd
		if cmd != "" {
			resolved := substituteHookTokens(cmd, hook)
			out.ResolvedBeforeReviewCmd = &resolved
		}
	}
	if err := writeJSON(stdout, out); err != nil {
		return fail(stderr, err)
	}
	return 0
}
