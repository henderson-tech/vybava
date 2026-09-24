// merge-precheck.ts — read-only pre-merge gate gathering for /git:merge.
// Run: node merge-precheck.ts [PR] [--repo <abs path of the checkout>]  → prints JSON {pr, gates, paths, checks, raw} to stdout.
// --repo (or GIT_SKILL_REPO) anchors EVERY git/gh call via repo-root.ts — without it the
// ambient shell cwd decides which repo (and which worktree context) gets resolved.
// Zero npm deps: shells out to `gh`/`git`, Node stdlib only. Erasable TS only.

export type ChecksState = "SUCCESS" | "PENDING" | "FAILURE" | "NONE";

export function summarizeChecks(
  rollup: { state?: string; conclusion?: string; status?: string }[] | null,
): ChecksState {
  const nodes = rollup ?? [];
  if (nodes.length === 0) return "NONE";
  const norm = (s?: string) => (s ?? "").toUpperCase();
  let pending = false;
  for (const n of nodes) {
    // Commit StatusContext nodes (e.g. a review bot's "Review completed") have no CheckRun
    // lifecycle fields — their `state` IS the terminal result, not a QUEUED/COMPLETED status.
    // Don't run it through the CheckRun logic below, or a green StatusContext (state=SUCCESS,
    // which !== "COMPLETED") gets misread as PENDING.
    if (n.status == null && n.conclusion == null) {
      const st = norm(n.state);
      if (st === "FAILURE" || st === "ERROR") return "FAILURE";
      if (st && st !== "SUCCESS") pending = true; // PENDING / EXPECTED / anything non-terminal
      continue;
    }
    const concl = norm(n.conclusion);
    const status = norm(n.status || n.state);
    if (["FAILURE", "ERROR", "CANCELLED", "TIMED_OUT", "ACTION_REQUIRED", "STARTUP_FAILURE"].includes(concl)) return "FAILURE";
    if (status === "FAILURE" || status === "ERROR") return "FAILURE";
    if (["QUEUED", "IN_PROGRESS", "PENDING", "WAITING", "REQUESTED"].includes(status) || (status !== "COMPLETED" && concl === "")) pending = true;
  }
  return pending ? "PENDING" : "SUCCESS";
}

export type GateInput = {
  state: string;
  mergeable: string;
  mergeStateStatus: string;
  reviewDecision: string;
  checks: ChecksState;
  worktreeDirty: boolean;
  isDraft: boolean;
  botApprovalOk: boolean;
  /** Does the repo define any GitHub Actions workflow? See ciOk below. */
  hasWorkflows: boolean;
};

export type GateSummary = {
  openOk: boolean; draftOk: boolean; cleanOk: boolean;
  mergeableOk: boolean; ciOk: boolean; approvedOk: boolean; botApprovalOk: boolean;
  allPass: boolean; failed: string[];
};

/** True when the repo defines at least one GitHub Actions workflow. */
export function repoHasWorkflows(mainClone: string): boolean {
  const dir = join(mainClone, ".github", "workflows");
  if (!existsSync(dir)) return false;
  return readdirSync(dir).some((f) => f.endsWith(".yml") || f.endsWith(".yaml"));
}

export function summarizeGates(g: GateInput): GateSummary {
  const openOk = g.state.toUpperCase() === "OPEN";
  const draftOk = !g.isDraft;
  const cleanOk = !g.worktreeDirty;
  const mergeableOk = g.mergeable.toUpperCase() !== "CONFLICTING" && g.mergeStateStatus.toUpperCase() !== "DIRTY";
  // "NONE" means GitHub reported no checks AT ALL for this head. That is
  // legitimate in a repo with no workflows — otherwise nothing there could ever
  // merge. It is a lie in a repo that HAS workflows: path-filtered jobs that
  // matched nothing, a run that never got a runner (an Actions outage), or a
  // head pushed before any workflow triggered all produce NONE, and treating it
  // as green means "no test ran" reads identically to "every test passed".
  // Under --auto that is the difference between a verified merge and an
  // unverified one, so absence is only a pass where absence is expected.
  const ciOk = g.checks === "SUCCESS" || (g.checks === "NONE" && !g.hasWorkflows);
  const rd = g.reviewDecision.toUpperCase();
  const approvedOk = rd === "APPROVED" || rd === "";
  const botApprovalOk = g.botApprovalOk;
  const failed: string[] = [];
  if (!openOk) failed.push("open");
  if (!draftOk) failed.push("draft");
  if (!cleanOk) failed.push("clean");
  if (!mergeableOk) failed.push("conflict");
  // Distinguished on purpose: "ci" is a red check, "ci-absent" is no check at
  // all. A caller deciding whether to wait or to escalate needs to tell them
  // apart, and the fix differs — rerun vs. work out why nothing ran.
  if (!ciOk) failed.push(g.checks === "NONE" ? "ci-absent" : "ci");
  if (!approvedOk) failed.push("review");
  if (!botApprovalOk) failed.push("botReview");
  return { openOk, draftOk, cleanOk, mergeableOk, ciOk, approvedOk, botApprovalOk, allPass: failed.length === 0, failed };
}

// --- Bot-approval gate (decision: hard gate, both flows) ---------------------
// A "required bot" must approve before a PR is mergeable: it is approved when its LATEST
// review is APPROVED (or an advisory COMMENTED verdict carrying an approval marker, see
// isBotApprovalReview). The required set = REQUIRED_BOT_REVIEWERS config (default
// eve-bot-lovinka[bot]) ∪ any auto-detected `[bot]` reviewer — but a bot only GATES when
// it is actually on the PR (a requested reviewer, or it has submitted a review).
// "if it is in the reviewers": a bot absent from the PR never blocks.

export type LatestReview = { login: string; state: string; body?: string };

// eve's `verdictPosture: advisory` repos (e.g. ReservineBack) post EVERY verdict as a neutral
// COMMENTED review; only the header carries the outcome. These are the two headers eve emits
// for the verdicts that a gating repo would post as APPROVE (looks-good, and medium-only
// "needs-changes" which APPROVEs with comments). "🔴 Changes requested" is deliberately absent.
const ADVISORY_APPROVAL_MARKERS = ["eve review — ✅ Approved", "eve review — 🟡 Review comments"];

export function isBotApprovalReview(review: LatestReview): boolean {
  if (review.state === "APPROVED") return true;
  if (review.state !== "COMMENTED" || !review.body) return false;
  return ADVISORY_APPROVAL_MARKERS.some((m) => review.body!.includes(m));
}

export type BotApprovalInput = {
  configList: string[];          // REQUIRED_BOT_REVIEWERS, lowercased (parsed by parseBotList)
  requested: string[];           // requested-reviewer logins, canonicalized (Bot → `[bot]` form)
  latestReviews: LatestReview[]; // latest review per author: login canonicalized, state UPPER
};

export type BotApprovalSummary = { ok: boolean; required: string[]; pending: string[] };

// MERGE_POLICY: how a PR reaches the default branch once its gate is green.
//   review (default) — wait for the human/bot review loop (merge.md gates as written).
//   self             — solo-owner repo: the PR is the trail, not a review; ensure-pr labels
//                      it `eve-ignore` (no eve review) and merge.md admin-merges as soon as
//                      clean + CI + no-conflict pass, skipping the review round entirely.
// Any other value → "review" (the safe reading) and the raw value is echoed so the caller
// can flag the typo instead of silently running the strict flow.
export const MERGE_POLICIES = ["review", "self"] as const;
export type MergePolicy = (typeof MERGE_POLICIES)[number];
export function parseMergePolicy(raw: string | undefined): { policy: MergePolicy; invalid: string | null } {
  const v = (raw ?? "").trim().toLowerCase();
  if (!v) return { policy: "review", invalid: null };
  return (MERGE_POLICIES as readonly string[]).includes(v)
    ? { policy: v as MergePolicy, invalid: null }
    : { policy: "review", invalid: raw ?? null };
}

// MERGE_METHOD: which `gh pr merge` flag lands the PR — merge (a merge commit), squash
// (one commit titled after the PR), rebase. A repository that has disabled a method
// refuses it at merge time, so when `allowed` is known the choice is derived from it
// rather than assumed: keep merge where it is still offered, else squash, else rebase.
// That makes a squash-only repository work with no config at all — the old hardcoded
// "merge" default tripped the server refusal on every one of them.
// An explicit MERGE_METHOD wins, EXCEPT when the repository has disabled it: naming a
// method the server will refuse is drift, so it falls back and the raw value is echoed
// as `invalid` — the same channel a typo uses — for the caller to flag.
export const MERGE_METHODS = ["merge", "squash", "rebase"] as const;
export type MergeMethod = (typeof MERGE_METHODS)[number];
export type AllowedMergeMethods = Partial<Record<MergeMethod, boolean>>;
export function parseMergeMethod(
  raw: string | undefined,
  allowed?: AllowedMergeMethods,
): { method: MergeMethod; invalid: string | null } {
  // No signal from the repository → the historical default, unchanged.
  const fallback = (): MergeMethod =>
    !allowed ? "merge" : allowed.merge ? "merge" : allowed.squash ? "squash" : allowed.rebase ? "rebase" : "merge";
  const v = (raw ?? "").trim().toLowerCase();
  if (!v) return { method: fallback(), invalid: null };
  if (!(MERGE_METHODS as readonly string[]).includes(v)) return { method: fallback(), invalid: raw ?? null };
  const method = v as MergeMethod;
  if (allowed && allowed[method] === false) return { method: fallback(), invalid: raw ?? null };
  return { method, invalid: null };
}

// AFTER_MERGE_STOP_SERVERS: which dev servers teardown stops, on top of whichever
// cleanup path ran (the AFTER_MERGE_CMD hook or the generic one).
//   worktree (default) — only the merged worktree's, which generic cleanup already does.
//   repo               — also the ones whose cwd is the MAIN CLONE, for repos that work
//                        from the default branch or leave a preview server there.
//   none               — stop nothing, even in generic cleanup.
// Same command-line filter and never-kill list in every mode; never Docker, and the
// main clone itself is never removed. A typo → "worktree" (the conservative reading),
// echoed as `invalid` so the caller flags it rather than silently widening a kill.
export const STOP_SERVERS_SCOPES = ["worktree", "repo", "none"] as const;
export type StopServersScope = (typeof STOP_SERVERS_SCOPES)[number];
export function parseStopServers(raw: string | undefined): { scope: StopServersScope; invalid: string | null } {
  const v = (raw ?? "").trim().toLowerCase();
  if (!v) return { scope: "worktree", invalid: null };
  return (STOP_SERVERS_SCOPES as readonly string[]).includes(v)
    ? { scope: v as StopServersScope, invalid: null }
    : { scope: "worktree", invalid: raw ?? null };
}

// REQUIRED_BOT_REVIEWERS is comma/space separated. Absent/empty → default eve-bot-lovinka[bot].
export function parseBotList(raw: string | undefined): string[] {
  const items = (raw ?? "").split(/[,\s]+/).map((s) => s.trim().toLowerCase()).filter(Boolean);
  return items.length ? items : ["eve-bot-lovinka[bot]"];
}

const stripBot = (l: string): string => (l.endsWith("[bot]") ? l.slice(0, -"[bot]".length) : l);

// GitHub's GraphQL API returns a Bot actor's `login` WITHOUT the `[bot]` suffix that REST and
// the UI show (e.g. `eve-bot-lovinka`). Canonicalize to the `[bot]` form using
// the actor's `__typename`, so the rest of the pipeline (and config) rely on one suffixed
// identity. Non-bot actors (Users) are lowercased, unchanged. Without this, auto-detection
// (which keys off the `[bot]` suffix) would NEVER fire on our own GraphQL data.
export function canonicalizeBotLogin(login: string, typename: string | undefined): string {
  const l = login.toLowerCase();
  return typename === "Bot" && !l.endsWith("[bot]") ? `${l}[bot]` : l;
}

// A login is a "bot" for gating if it is the canonicalized GitHub-App form (ends `[bot]`) OR
// the project named it in config — the config path covers machine-USER bots (typename User,
// e.g. a PAT account) that carry no `[bot]` suffix and so can't be auto-detected. Config
// matching ignores the `[bot]` suffix on both sides, so a config entry written either as
// `eve-bot-lovinka` or `eve-bot-lovinka[bot]` matches.
function isBotLogin(login: string, configSet: Set<string>): boolean {
  return login.endsWith("[bot]") || configSet.has(stripBot(login));
}

export function summarizeBotApproval(input: BotApprovalInput): BotApprovalSummary {
  const configSet = new Set(input.configList.map((s) => stripBot(s.toLowerCase())));
  const present = new Set<string>();
  for (const r of input.requested) if (isBotLogin(r, configSet)) present.add(r);
  for (const rv of input.latestReviews) if (isBotLogin(rv.login, configSet)) present.add(rv.login);

  const pending: string[] = [];
  for (const bot of present) {
    const latest = input.latestReviews.find((r) => r.login === bot);
    if (!latest || !isBotApprovalReview(latest)) pending.push(bot);
  }
  return { ok: pending.length === 0, required: [...present].sort(), pending: pending.sort() };
}

export type HookContext = { slug: string; branch: string; worktree: string; pr: number };

// Resolve {slug}/{branch}/{worktree}/{pr} in a configured AFTER_MERGE_CMD. Every occurrence
// of each token is replaced; a command with no tokens is returned unchanged.
// The branch a green PR lands on and the seat the main clone is expected to sit on.
// GitHub's default branch is the team-wide answer; a machine adopting a new integration
// branch ahead of the team overrides it with DEFAULT_BRANCH in the gitignored
// .claude/.claude.git.config.local (same overlay /sync reads). Blank/absent → GitHub's.
export function resolveDefaultBranch(cfg: Record<string, string>, githubDefault: string | undefined | null): string {
  const override = (cfg.DEFAULT_BRANCH ?? "").trim();
  if (override) return override;
  return (githubDefault ?? "").trim() || "main";
}

export function substituteHookTokens(cmd: string, ctx: HookContext): string {
  return cmd
    .replaceAll("{slug}", ctx.slug)
    .replaceAll("{branch}", ctx.branch)
    .replaceAll("{worktree}", ctx.worktree)
    .replaceAll("{pr}", String(ctx.pr));
}

import { execFileSync } from "node:child_process";
import { existsSync, readFileSync, readdirSync} from "node:fs";
import { basename, join } from "node:path";
import { readGitConfig } from "./sync-context.ts";
import { repoRoot } from "./repo-root.ts";

function sh(cmd: string, args: string[]): string {
  return execFileSync(cmd, args, { encoding: "utf8", maxBuffer: 32 * 1024 * 1024, timeout: 60_000, cwd: repoRoot() });
}

// One GraphQL round for everything the bot-approval gate needs: requested reviewers and the
// latest review per author (first page, 50 each — no pagination; a PR with >50 of either
// would be pathological). Review threads are prm's business, not this gate's.
const GATE_QUERY = `
query($owner:String!, $repo:String!, $pr:Int!) {
  repository(owner:$owner, name:$repo) {
    pullRequest(number:$pr) {
      reviewRequests(first:50) {
        nodes { requestedReviewer { __typename ... on User { login } ... on Bot { login } ... on Mannequin { login } } }
      }
      latestReviews(first:50) { nodes { author { login __typename } state body } }
    }
  }
}`;

function gatherBotData(owner: string, repo: string, pr: number): Omit<BotApprovalInput, "configList"> {
  const requested: string[] = [];
  const latestReviews: LatestReview[] = [];
  const vars = ["-f", `owner=${owner}`, "-f", `repo=${repo}`, "-F", `pr=${pr}`];
  const node = JSON.parse(sh("gh", ["api", "graphql", "-f", `query=${GATE_QUERY}`, ...vars])).data.repository.pullRequest;
  for (const n of node.reviewRequests?.nodes ?? []) {
    const rr = n.requestedReviewer;
    if (rr?.login) requested.push(canonicalizeBotLogin(String(rr.login), rr.__typename));
  }
  for (const n of node.latestReviews?.nodes ?? []) {
    const a = n.author;
    if (a?.login) latestReviews.push({ login: canonicalizeBotLogin(String(a.login), a.__typename), state: String(n.state ?? "").toUpperCase(), body: typeof n.body === "string" ? n.body : undefined });
  }
  return { requested, latestReviews };
}

// `headRef` (the PR's head branch) is what makes this safe. Resolving the worktree from
// the ambient cwd — or from --repo, which /prm and /merge pin to the MAIN clone by
// design — reports the main checkout whenever the caller is not standing inside the
// worktree. {slug} is basename(worktree), so AFTER_MERGE_CMD then resolves to e.g.
// `/wk:cleanup FixIt --remove --yes --delete-remote`: the teardown hook handed the one
// directory it must never touch. Observed on FixIt PR #896, 2026-08-02.
// So: prefer the worktree that actually holds the PR's head branch, and report that
// worktree's branch/dirtiness — the `cleanOk` gate exists to guard the directory that
// is about to be REMOVED, not whichever one the shell happens to sit in.
function gatherPaths(headRef?: string) {
  const cwdTop = sh("git", ["rev-parse", "--show-toplevel"]).trim();
  const listing = sh("git", ["worktree", "list", "--porcelain"]).trim();
  // The main clone is the FIRST entry of `git worktree list` (the primary worktree).
  const first = listing.split("\n").find((l) => l.startsWith("worktree "));
  const mainClone = first ? first.slice("worktree ".length).trim() : cwdTop;

  let worktree = cwdTop;
  if (headRef) {
    let current = "";
    for (const line of listing.split("\n")) {
      if (line.startsWith("worktree ")) current = line.slice("worktree ".length).trim();
      else if (current && line.trim() === `branch refs/heads/${headRef}`) {
        worktree = current;
        break;
      }
    }
  }

  const branch = sh("git", ["-C", worktree, "rev-parse", "--abbrev-ref", "HEAD"]).trim();
  const dirty = sh("git", ["-C", worktree, "status", "--porcelain"]).trim().length > 0;
  return { worktree, branch, mainClone, dirty, isWorktree: worktree !== mainClone };
}

async function main(): Promise<void> {
  const prArg = process.argv.slice(2).filter((a) => !a.startsWith("--"))[0];
  const repo = JSON.parse(sh("gh", [
    "repo", "view", "--json",
    "nameWithOwner,defaultBranchRef,mergeCommitAllowed,squashMergeAllowed,rebaseMergeAllowed",
  ]));
  const [owner, name] = repo.nameWithOwner.split("/");
  const fields = "number,state,mergeable,mergeStateStatus,reviewDecision,statusCheckRollup,headRefName,baseRefName,url,title,isDraft";
  const pr = JSON.parse(sh("gh", ["pr", "view", ...(prArg ? [prArg] : []), "--json", fields]));
  const paths = gatherPaths(pr.headRefName);
  const checks = summarizeChecks(pr.statusCheckRollup);

  // Bot-approval gate. The required-bot list comes from the SAME .claude.git.config that
  // holds AFTER_MERGE_CMD (read once below). Only OPEN PRs are worth a GraphQL round.
  const { cfg } = readGitConfig(paths.mainClone);
  const defaultBranch = resolveDefaultBranch(cfg, repo.defaultBranchRef?.name);
  const requiredBotReviewers = parseBotList(cfg.REQUIRED_BOT_REVIEWERS);
  const mergePolicy = parseMergePolicy(cfg.MERGE_POLICY);
  const mergeMethod = parseMergeMethod(cfg.MERGE_METHOD, {
    merge: repo.mergeCommitAllowed, squash: repo.squashMergeAllowed, rebase: repo.rebaseMergeAllowed,
  });
  const botData = pr.state?.toUpperCase() === "OPEN"
    ? gatherBotData(owner, name, pr.number)
    : { requested: [], latestReviews: [] };
  const botApproval = summarizeBotApproval({ configList: requiredBotReviewers, ...botData });

  const gates = summarizeGates({
    state: pr.state, mergeable: pr.mergeable, mergeStateStatus: pr.mergeStateStatus,
    reviewDecision: pr.reviewDecision ?? "", checks, worktreeDirty: paths.dirty, isDraft: pr.isDraft,
    botApprovalOk: botApproval.ok,
    hasWorkflows: repoHasWorkflows(paths.mainClone),
  });

  // Post-merge teardown hook (decision #6): a project may register AFTER_MERGE_CMD in
  // <mainClone>/.claude/.claude.git.config. When set, git:merge runs the resolved command
  // INSTEAD of its generic `worktree remove + branch -d`. {slug}=worktree basename.
  const slug = basename(paths.worktree);
  const hookCtx = { slug, branch: paths.branch, worktree: paths.worktree, pr: pr.number };
  // Only ever hand the teardown hook a real worktree. When no worktree holds the head
  // branch, `worktree` falls back to the cwd checkout — usually the MAIN clone — and a
  // resolved command like `/wk:cleanup FixIt --remove --yes` would delete the user's
  // primary checkout. Callers are told to run this hook INSTEAD of the generic path,
  // so it bypasses the isWorktree guard; the only safe answer here is to emit nothing.
  // There is also nothing to tear down in that case, so null is correct, not merely safe.
  const afterMergeCmd = cfg.AFTER_MERGE_CMD ?? null;
  const resolvedAfterMergeCmd = afterMergeCmd && paths.isWorktree
    ? substituteHookTokens(afterMergeCmd, hookCtx)
    : null;

  // Pre-review quiesce hook (symmetric with AFTER_MERGE_CMD): a project may register
  // BEFORE_REVIEW_CMD to stop whatever the worktree has RUNNING before a review loop
  // starts. Entering the loop invalidates it — merge-from-main, dependency installs and
  // codegen all thrash the disk while dev servers/emulators do stale work. /prm
  // runs this on entering the loop AND at the start of each round, so it must be
  // idempotent and cheap (FixIt's `/wk:pause` is both).
  const stopServers = parseStopServers(cfg.AFTER_MERGE_STOP_SERVERS);

  const beforeReviewCmd = cfg.BEFORE_REVIEW_CMD ?? null;
  const resolvedBeforeReviewCmd = beforeReviewCmd
    ? substituteHookTokens(beforeReviewCmd, hookCtx)
    : null;

  process.stdout.write(JSON.stringify({
    owner, repo: name, pr: pr.number, url: pr.url, title: pr.title,
    headRef: pr.headRefName, baseRef: pr.baseRefName,
    branch: paths.branch, defaultBranch, onDefaultBranch: paths.branch === defaultBranch,
    worktree: paths.worktree, mainClone: paths.mainClone, isWorktree: paths.isWorktree, slug,
    checks, gates, botApproval, requiredBotReviewers,
    mergePolicy: mergePolicy.policy, mergePolicyInvalid: mergePolicy.invalid,
    mergeMethod: mergeMethod.method, mergeMethodInvalid: mergeMethod.invalid,
    afterMergeCmd, resolvedAfterMergeCmd,
    stopServers: stopServers.scope, stopServersInvalid: stopServers.invalid,
    beforeReviewCmd, resolvedBeforeReviewCmd,
    raw: { state: pr.state, mergeable: pr.mergeable, mergeStateStatus: pr.mergeStateStatus, reviewDecision: pr.reviewDecision, isDraft: pr.isDraft },
  }, null, 2) + "\n");
}

if (import.meta.main) {
  main().catch((e) => {
    process.stderr.write(`error: ${(e as Error).message}\n`);
    process.exit(1);
  });
}
