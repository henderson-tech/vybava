import { test } from "node:test";
import assert from "node:assert/strict";
import { summarizeChecks, summarizeGates, substituteHookTokens, summarizeBotApproval, isBotApprovalReview, parseBotList, canonicalizeBotLogin, parseMergePolicy, parseMergeMethod, parseStopServers, resolveDefaultBranch } from "../bin/merge-precheck.ts";
import { parseConfig } from "../bin/sync-context.ts";

test("no checks configured → NONE", () => {
  assert.equal(summarizeChecks(null), "NONE");
  assert.equal(summarizeChecks([]), "NONE");
});

test("any failing check → FAILURE", () => {
  assert.equal(summarizeChecks([{ status: "COMPLETED", conclusion: "SUCCESS" }, { status: "COMPLETED", conclusion: "FAILURE" }]), "FAILURE");
});

test("in-progress check → PENDING", () => {
  assert.equal(summarizeChecks([{ status: "COMPLETED", conclusion: "SUCCESS" }, { status: "IN_PROGRESS", conclusion: "" }]), "PENDING");
});

test("all complete & successful → SUCCESS", () => {
  assert.equal(summarizeChecks([{ status: "COMPLETED", conclusion: "SUCCESS" }, { status: "COMPLETED", conclusion: "SUCCESS" }]), "SUCCESS");
});

// Commit StatusContext nodes (legacy commit statuses, e.g. a review bot's "Review completed")
// carry their terminal result in `state`, with NO CheckRun `status`/`conclusion` fields.
test("StatusContext SUCCESS (no status/conclusion fields) → SUCCESS", () => {
  assert.equal(summarizeChecks([{ state: "SUCCESS" }]), "SUCCESS");
});

test("mixed CheckRun + StatusContext, all green → SUCCESS", () => {
  assert.equal(
    summarizeChecks([
      { status: "COMPLETED", conclusion: "SUCCESS" }, // license-check
      { status: "COMPLETED", conclusion: "SKIPPED" }, // skipped E2E-comment job
      { state: "SUCCESS" }, // review-bot commit status
    ]),
    "SUCCESS",
  );
});

test("StatusContext PENDING/EXPECTED → PENDING", () => {
  assert.equal(summarizeChecks([{ state: "PENDING" }]), "PENDING");
  assert.equal(summarizeChecks([{ state: "EXPECTED" }]), "PENDING");
});

test("StatusContext FAILURE/ERROR → FAILURE", () => {
  assert.equal(summarizeChecks([{ state: "FAILURE" }]), "FAILURE");
  assert.equal(summarizeChecks([{ state: "ERROR" }]), "FAILURE");
});

const clean = {
  state: "OPEN", mergeable: "MERGEABLE", mergeStateStatus: "CLEAN",
  reviewDecision: "APPROVED", checks: "SUCCESS" as const, worktreeDirty: false, isDraft: false,
  botApprovalOk: true,
};

test("all gates pass on a clean approved green PR", () => {
  const g = summarizeGates(clean);
  assert.equal(g.allPass, true);
  assert.deepEqual(g.failed, []);
});

test("no review required (null decision) still passes the approval gate", () => {
  const g = summarizeGates({ ...clean, reviewDecision: "" });
  assert.equal(g.approvedOk, true);
  assert.equal(g.allPass, true);
});

test("dirty worktree fails the clean gate", () => {
  const g = summarizeGates({ ...clean, worktreeDirty: true });
  assert.equal(g.cleanOk, false);
  assert.ok(g.failed.includes("clean"));
  assert.equal(g.allPass, false);
});

test("conflicts fail the mergeable gate (CONFLICTING or DIRTY status)", () => {
  assert.equal(summarizeGates({ ...clean, mergeable: "CONFLICTING" }).mergeableOk, false);
  assert.equal(summarizeGates({ ...clean, mergeStateStatus: "DIRTY" }).mergeableOk, false);
});

test("a still-computing UNKNOWN mergeability fails as mergeable-unknown, never passes", () => {
  const g = summarizeGates({ ...clean, mergeable: "UNKNOWN" });
  assert.equal(g.mergeableOk, false);
  assert.deepEqual(g.failed, ["mergeable-unknown"]);
});

test("pending CI fails the ci gate", () => {
  assert.ok(summarizeGates({ ...clean, checks: "PENDING" }).failed.includes("ci"));
});

test("changes requested / review required fail the approval gate", () => {
  assert.equal(summarizeGates({ ...clean, reviewDecision: "CHANGES_REQUESTED" }).approvedOk, false);
  assert.equal(summarizeGates({ ...clean, reviewDecision: "REVIEW_REQUIRED" }).approvedOk, false);
});

test("draft and non-open PRs are flagged", () => {
  assert.ok(summarizeGates({ ...clean, isDraft: true }).failed.includes("draft"));
  assert.ok(summarizeGates({ ...clean, state: "MERGED" }).failed.includes("open"));
});

test("pending bot approval fails the botReview gate", () => {
  const g = summarizeGates({ ...clean, botApprovalOk: false });
  assert.equal(g.botApprovalOk, false);
  assert.ok(g.failed.includes("botReview"));
  assert.equal(g.allPass, false);
});

// --- parseBotList ---
test("parseBotList defaults to eve-bot-lovinka[bot] when unset/empty", () => {
  assert.deepEqual(parseBotList(undefined), ["eve-bot-lovinka[bot]"]);
  assert.deepEqual(parseBotList("   "), ["eve-bot-lovinka[bot]"]);
});

test("parseBotList splits on comma/space and lowercases", () => {
  assert.deepEqual(parseBotList("eve-bot-lovinka[bot], my-ci-machine-user"), ["eve-bot-lovinka[bot]", "my-ci-machine-user"]);
  assert.deepEqual(parseBotList("Eve-Bot-Lovinka[bot]"), ["eve-bot-lovinka[bot]"]);
});

// --- summarizeBotApproval ---
const botBase = { configList: ["eve-bot-lovinka[bot]"], requested: [], latestReviews: [] };

test("no bots on the PR → vacuously approved", () => {
  const s = summarizeBotApproval(botBase);
  assert.equal(s.ok, true);
  assert.deepEqual(s.required, []);
});

test("a required bot reviewer must have a latest APPROVED review", () => {
  const requested = ["eve-bot-lovinka"]; // bare login named in config (config path)
  const cfg = ["eve-bot-lovinka"];
  // requested but no approval yet → pending
  const pending = summarizeBotApproval({ ...botBase, configList: cfg, requested });
  assert.equal(pending.ok, false);
  assert.deepEqual(pending.pending, ["eve-bot-lovinka"]);
  // CHANGES_REQUESTED → still pending
  const changes = summarizeBotApproval({ ...botBase, configList: cfg, latestReviews: [{ login: "eve-bot-lovinka", state: "CHANGES_REQUESTED" }] });
  assert.equal(changes.ok, false);
  // APPROVED → ok
  const approved = summarizeBotApproval({ ...botBase, configList: cfg, latestReviews: [{ login: "eve-bot-lovinka", state: "APPROVED" }] });
  assert.equal(approved.ok, true);
});

test("an advisory COMMENTED eve verdict counts as approval only with an approval header", () => {
  const login = "eve-bot-lovinka[bot]";
  const cfg = ["eve-bot-lovinka[bot]"];
  const review = (body: string, state = "COMMENTED") => summarizeBotApproval({ ...botBase, configList: cfg, latestReviews: [{ login, state, body }] });
  // verdictPosture: advisory (ReservineBack) — looks-good and medium-only verdicts are approvals
  assert.equal(review("## 🐉 eve review — ✅ Approved\n> `29b29e4` · 0 actionable findings").ok, true);
  assert.equal(review("## 🐉 eve review — 🟡 Review comments\n> `abc1234` · 2 actionable findings").ok, true);
  // a blocking verdict, a bodiless comment, or the in-progress status block never approve
  assert.equal(review("## 🐉 eve review — 🔴 Changes requested\n> 1 blocking finding").ok, false);
  assert.equal(review("").ok, false);
  assert.equal(review("🐉 **eve review in progress — run 1**").ok, false);
  // the marker never rescues a CHANGES_REQUESTED state
  assert.equal(review("## 🐉 eve review — ✅ Approved", "CHANGES_REQUESTED").ok, false);
  assert.equal(isBotApprovalReview({ login, state: "APPROVED" }), true);
});

test("eve-bot-lovinka only gates when it is on the PR ('if it is in the reviewers')", () => {
  // Configured but absent from the PR entirely → does not block.
  const s = summarizeBotApproval({ ...botBase, configList: ["eve-bot-lovinka"] });
  assert.equal(s.ok, true);
  assert.deepEqual(s.required, []);
});

test("a [bot] reviewer is auto-required without config; a non-[bot] machine user is not", () => {
  // GitHub App bot (login ends [bot]) is auto-detected even when not in config.
  const auto = summarizeBotApproval({ ...botBase, requested: ["some-app[bot]"] });
  assert.deepEqual(auto.pending, ["some-app[bot]"]);
  // A machine user without [bot] suffix and not in config is NOT treated as a required bot.
  const human = summarizeBotApproval({ ...botBase, latestReviews: [{ login: "some-machine-user", state: "COMMENTED" }] });
  assert.equal(human.ok, true);
});

// --- canonicalizeBotLogin (GraphQL strips the [bot] suffix; we restore it via __typename) ---
test("canonicalizeBotLogin restores [bot] for Bot actors, lowercases, leaves Users alone", () => {
  assert.equal(canonicalizeBotLogin("eve-bot-lovinka", "Bot"), "eve-bot-lovinka[bot]");
  assert.equal(canonicalizeBotLogin("SomeApp", "Bot"), "someapp[bot]");
  assert.equal(canonicalizeBotLogin("already[bot]", "Bot"), "already[bot]"); // no double suffix
  assert.equal(canonicalizeBotLogin("LEFTEQ", "User"), "lefteq");
  assert.equal(canonicalizeBotLogin("LEFTEQ", undefined), "lefteq");
});

test("a GitHub App bot (eve-bot-lovinka) auto-gates with NO config after canonicalization", () => {
  // Mirrors the real eve-ai-layer flow: GraphQL returns `eve-bot-lovinka` typename Bot →
  // gatherBotData canonicalizes to `eve-bot-lovinka[bot]` → auto-detected, default config only.
  const login = canonicalizeBotLogin("eve-bot-lovinka", "Bot"); // eve-bot-lovinka[bot]
  const pending = summarizeBotApproval({ ...botBase, requested: [login] });
  assert.equal(pending.ok, false);
  assert.deepEqual(pending.pending, ["eve-bot-lovinka[bot]"]);
  const approved = summarizeBotApproval({ ...botBase, latestReviews: [{ login, state: "APPROVED" }] });
  assert.equal(approved.ok, true);
});

test("config entry matches with or without the [bot] suffix", () => {
  const login = "eve-bot-lovinka[bot]"; // canonicalized review login
  // bare config entry still matches the suffixed login (config path is suffix-insensitive)
  const bare = summarizeBotApproval({ configList: ["eve-bot-lovinka"], requested: [login], latestReviews: [] });
  assert.deepEqual(bare.pending, ["eve-bot-lovinka[bot]"]);
});

const ctx = { slug: "pr-266", branch: "feat-x", worktree: "/r/.worktrees/pr-266", pr: 266 };

test("substituteHookTokens fills {slug}/{branch}/{worktree}/{pr}", () => {
  assert.equal(
    substituteHookTokens("/wk:cleanup {slug} --remove --yes --delete-remote", ctx),
    "/wk:cleanup pr-266 --remove --yes --delete-remote",
  );
  assert.equal(substituteHookTokens("{slug} {branch} {worktree} {pr}", ctx), "pr-266 feat-x /r/.worktrees/pr-266 266");
});

test("substituteHookTokens replaces every occurrence and leaves token-free commands alone", () => {
  assert.equal(substituteHookTokens("echo {pr}-{pr}", ctx), "echo 266-266");
  assert.equal(substituteHookTokens("docker compose down -v", ctx), "docker compose down -v");
});

// --- BEFORE_REVIEW_CMD (pre-review quiesce hook) -----------------------------
// Symmetric with AFTER_MERGE_CMD. These pin the SHIPPED config shape: both keys
// coexist in one file, comments between them do not swallow the next key, and
// the hook resolves for /prm's create path — which runs BEFORE a PR exists, i.e. with pr = 0.

test("a config carrying BOTH hooks parses both, comments and all", () => {
  const cfg = parseConfig([
    "# post-merge teardown",
    "AFTER_MERGE_CMD=/wk:cleanup {slug} --remove --yes --delete-remote",
    "",
    "# pre-review quiesce: runs on loop entry AND each round",
    "BEFORE_REVIEW_CMD=/wk:pause {slug}",
  ].join("\n"));
  assert.equal(cfg.AFTER_MERGE_CMD, "/wk:cleanup {slug} --remove --yes --delete-remote");
  assert.equal(cfg.BEFORE_REVIEW_CMD, "/wk:pause {slug}");
});

test("BEFORE_REVIEW_CMD substitutes like any other hook", () => {
  assert.equal(substituteHookTokens("/wk:pause {slug}", ctx), "/wk:pause pr-266");
});

test("the quiesce hook resolves before a PR exists (prm's create path runs it with pr = 0)", () => {
  const noPr = { slug: "wk-quiesce", branch: "work/wk-quiesce", worktree: "/r/.worktrees/wk-quiesce", pr: 0 };
  assert.equal(substituteHookTokens("/wk:pause {slug}", noPr), "/wk:pause wk-quiesce");
  assert.equal(substituteHookTokens("cmd --pr {pr}", noPr), "cmd --pr 0");
});

test("a project with no quiesce hook yields null, not an error", () => {
  const cfg = parseConfig("AFTER_MERGE_CMD=/wk:cleanup {slug}\n");
  assert.equal(cfg.BEFORE_REVIEW_CMD ?? null, null);
});

// "No checks reported" must not read as "all checks passed" in a repo that HAS
// workflows. Path-filtered jobs that matched nothing, a run that never got a
// runner (Actions outage, 2026-08-26), or a head pushed before anything
// triggered all produce NONE — and under --auto that once stood one gate away
// from merging code CI had never run on.
test("NONE is a pass only where no workflow exists", () => {
  const base = {
    state: "OPEN", mergeable: "MERGEABLE", mergeStateStatus: "CLEAN",
    reviewDecision: "APPROVED", worktreeDirty: false, isDraft: false, botApprovalOk: true,
  } as const;

  const withWorkflows = summarizeGates({ ...base, checks: "NONE", hasWorkflows: true });
  assert.equal(withWorkflows.ciOk, false);
  assert.ok(withWorkflows.failed.includes("ci-absent"),
    "absence must be reported distinctly from a red check, so a caller can tell rerun from investigate");

  const withoutWorkflows = summarizeGates({ ...base, checks: "NONE", hasWorkflows: false });
  assert.equal(withoutWorkflows.ciOk, true, "a repo with no CI must still be mergeable");

  assert.equal(summarizeGates({ ...base, checks: "SUCCESS", hasWorkflows: true }).ciOk, true);
  const red = summarizeGates({ ...base, checks: "FAILURE", hasWorkflows: true });
  assert.ok(red.failed.includes("ci") && !red.failed.includes("ci-absent"),
    "a red check is 'ci', not 'ci-absent'");
});

test("MERGE_METHOD: absent → merge; squash → squash; a typo falls back to merge and is echoed", () => {
  assert.deepEqual(parseMergeMethod(undefined), { method: "merge", invalid: null });
  assert.deepEqual(parseMergeMethod(" Squash "), { method: "squash", invalid: null });
  assert.deepEqual(parseMergeMethod("rebase"), { method: "rebase", invalid: null });
  assert.deepEqual(parseMergeMethod("fast-forward"), { method: "merge", invalid: "fast-forward" });
});

test("MERGE_METHOD: what the repository allows overrides an unset or refused method", () => {
  const squashOnly = { merge: false, squash: true, rebase: false };
  // A squash-only repository needs no config — the old "merge" default was a server refusal.
  assert.deepEqual(parseMergeMethod(undefined, squashOnly), { method: "squash", invalid: null });
  // An explicit method the repository has disabled is drift: fall back, echo it.
  assert.deepEqual(parseMergeMethod("merge", squashOnly), { method: "squash", invalid: "merge" });
  // Where merge is still offered, the historical default stands.
  assert.deepEqual(parseMergeMethod(undefined, { merge: true, squash: true, rebase: true }), { method: "merge", invalid: null });
});

test("MERGE_POLICY: absent → review; self → self; a typo falls back to review and is echoed", () => {
  assert.deepEqual(parseMergePolicy(undefined), { policy: "review", invalid: null });
  assert.deepEqual(parseMergePolicy(" Self "), { policy: "self", invalid: null });
  assert.deepEqual(parseMergePolicy("auto"), { policy: "review", invalid: "auto" });
});

// AFTER_MERGE_STOP_SERVERS widens a kill, so an unrecognized value must never be read as
// the wider scope — it falls back to worktree and the raw value is echoed for the caller.
test("AFTER_MERGE_STOP_SERVERS: absent → worktree; repo/none honoured; a typo falls back and is echoed", () => {
  assert.deepEqual(parseStopServers(undefined), { scope: "worktree", invalid: null });
  assert.deepEqual(parseStopServers(" Repo "), { scope: "repo", invalid: null });
  assert.deepEqual(parseStopServers("none"), { scope: "none", invalid: null });
  assert.deepEqual(parseStopServers("all"), { scope: "worktree", invalid: "all" });
});

// DEFAULT_BRANCH: a machine adopting a new integration branch ahead of the team sets it in
// the gitignored .claude.git.config.local; everyone else keeps following GitHub's default.
test("resolveDefaultBranch: no override → GitHub default; GitHub unknown → main", () => {
  assert.equal(resolveDefaultBranch({}, "master"), "master");
  assert.equal(resolveDefaultBranch({}, undefined), "main");
  assert.equal(resolveDefaultBranch({}, ""), "main");
});

test("resolveDefaultBranch: DEFAULT_BRANCH overlay wins over GitHub's default", () => {
  assert.equal(resolveDefaultBranch({ DEFAULT_BRANCH: "devlp" }, "main"), "devlp");
  assert.equal(resolveDefaultBranch(parseConfig("MERGE_METHOD=squash\nDEFAULT_BRANCH=devlp\n"), "master"), "devlp");
});

test("resolveDefaultBranch: blank DEFAULT_BRANCH is not an override", () => {
  assert.equal(resolveDefaultBranch({ DEFAULT_BRANCH: "  " }, "main"), "main");
});
