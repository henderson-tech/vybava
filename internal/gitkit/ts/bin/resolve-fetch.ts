// resolve-fetch.ts — deterministic PR resolution + comment fetch/bucket.
// Run: node resolve-fetch.ts [PR selector] [flags] [--repo <abs path>]  → prints a JSON envelope to stdout.
// --repo (or GIT_SKILL_REPO) anchors the git/gh calls via repo-root.ts; otherwise ambient cwd.
// Zero npm deps: shells out to `gh`/`git`, Node stdlib only. Erasable TS syntax only.

export type Selector =
  | { kind: "current" }
  | { kind: "number"; pr: number }
  | { kind: "numberInRepo"; pr: number; owner: string; repo: string }
  | { kind: "url"; owner: string; repo: string; pr: number }
  | { kind: "latestByAuthor"; author: string };

export type Flags = {
  once: boolean;
  includeResolved: boolean;
  /** Skip review summaries + PR conversation comments (the pre-2026-07-26 behaviour). */
  noConversation: boolean;
  every?: string;
};

export type ParsedArgs = { selector: Selector; flags: Flags };

export function parsePrArgs(argv: string[]): ParsedArgs {
  const flags: Flags = { once: false, includeResolved: false, noConversation: false };
  const positional: string[] = [];
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a === "--once") flags.once = true;
    else if (a === "--include-resolved") flags.includeResolved = true;
    else if (a === "--no-conversation") flags.noConversation = true;
    else if (a === "--every") flags.every = argv[++i];
    // --repo is consumed by repo-root.ts straight from argv; strip both forms
    // here so the path never leaks into the PR selector.
    else if (a === "--repo") i++;
    else if (a.startsWith("--repo=")) continue;
    else positional.push(a);
  }
  const joined = positional.join(" ").trim();
  let selector: Selector;
  if (joined === "") {
    selector = { kind: "current" };
  } else {
    const url = joined.match(/^https?:\/\/github\.com\/([^/]+)\/([^/]+)\/pull\/(\d+)/);
    const inRepo = joined.match(/^#?(\d+)\s+in\s+([^/\s]+)\/([^/\s]+)$/);
    const latest = joined.match(/^latest\s+by\s+@?(\S+)$/i);
    const num = joined.match(/^#?(\d+)$/);
    if (url) selector = { kind: "url", owner: url[1], repo: url[2], pr: Number(url[3]) };
    else if (inRepo) selector = { kind: "numberInRepo", pr: Number(inRepo[1]), owner: inRepo[2], repo: inRepo[3] };
    else if (latest) selector = { kind: "latestByAuthor", author: latest[1] };
    else if (num) selector = { kind: "number", pr: Number(num[1]) };
    else throw new Error(`Unrecognized PR selector: "${joined}"`);
  }
  return { selector, flags };
}

// "No PR number" means "this checkout's PR" — works from any branch or git worktree.
// On a branch we look up by head branch; on a detached HEAD (worktree at a bare commit,
// where `git branch --show-current` is empty) we look up by the current commit SHA, so
// we never run `gh pr list --head ""` and silently grab an arbitrary open PR.
export function choosePrLookup(branch: string): { by: "head"; branch: string } | { by: "sha" } {
  const b = branch.trim();
  return b === "" ? { by: "sha" } : { by: "head", branch: b };
}

const FILTERED_BOTS = ["dependabot", "renovate", "codecov", "vercel", "netlify", "github-actions"];

export function isFilteredBot(login: string): boolean {
  const l = login.toLowerCase();
  if (l.endsWith("[bot]")) return true;
  return FILTERED_BOTS.includes(l);
}

export type ThreadComment = {
  databaseId: number;
  path: string | null;
  line: number | null;
  originalLine: number | null;
  body: string;
  url: string;
  author: { login: string } | null;
  replyTo: { databaseId: number } | null;
};

export type ReviewThread = {
  id: string;
  isResolved: boolean;
  isOutdated: boolean;
  comments: { nodes: ThreadComment[] };
};

export type Finding = {
  id: number;
  by: string;
  file: string;
  /**
   * Where the finding lives. The first two are review THREADS (resolvable); the
   * last two have no thread at all. Never branch on this to decide closure —
   * branch on `resolvable`.
   */
  surface: "inline" | "review-thread" | "review-summary" | "conversation";
  body: string;
  thread: string;
  /**
   * The review thread to resolve, or null when the surface has none.
   *
   * Only INLINE review threads are resolvable — `resolveReviewThread` takes a
   * thread id, and neither a review's summary body nor a PR conversation comment
   * has one. A null here is not a bug to route around: it means closure for this
   * finding is a reply plus an acknowledging reaction, never a resolve call.
   */
  threadId: string | null;
  resolvable: boolean;
  url: string;
};

export type Skipped = {
  bots: number;
  self: number;
  resolved: number;
  outdated: number;
  /** Non-thread bodies that carried no ask: empty, or a known bot status/summary block. */
  informational: number;
};

export type BucketResult = {
  findings: Finding[];
  skipped: Skipped;
};

/**
 * Bot bodies that are STATUS, not a request. Surfacing these would open every
 * round with fake work — a review bot typically posts a walkthrough plus an
 * "Actionable comments posted: N" block on every push, while its real findings
 * arrive as inline threads. Matched on the body, not the author, so a bot that
 * does ask for something still reaches the round.
 */
const BOT_STATUS_PATTERNS = [
  // Generic review-bot walkthrough / status blocks.
  /^<!--\s*walkthrough/i,
  /actionable comments posted:/i,
  /^\s*##\s*walkthrough/im,
  /review (?:status|details)\s*<!--/i,
  /<summary>.*(?:walkthrough|files? (?:selected )?for processing|review details).*<\/summary>/is,
  // eve review bot. Its real findings arrive as INLINE threads; everything it
  // posts to the summary/conversation is a status header. Found empirically on
  // FixIt #702 (2026-07-26): without these, a normal eve-reviewed PR opened every
  // round with 8 fake findings — 3 review-summary headers plus 5 "delta review
  // completed" notices.
  /^#{1,3}\s*(?:🐉\s*)?eve review\b/im,
  /\beve review\s*[—–-]\s*(?:✅|🟡|🔴|⚪|approve|request|review comments|no findings)/i,
  /\bdelta review for\b[\s\S]*\bcompleted and posted\b/i,
  /\breviewed and \*{0,2}approved\*{0,2}\b/i,
];

export function isBotStatusBody(body: string): boolean {
  return BOT_STATUS_PATTERNS.some((re) => re.test(body));
}

export function bucketFindings(
  threads: ReviewThread[],
  prAuthor: string,
  opts: { includeResolved: boolean },
): BucketResult {
  const findings: Finding[] = [];
  const skipped: Skipped = { bots: 0, self: 0, resolved: 0, outdated: 0, informational: 0 };
  for (const t of threads) {
    if (t.isResolved && !opts.includeResolved) { skipped.resolved++; continue; }
    const nodes = t.comments?.nodes ?? [];
    const root = nodes.find((c) => c.replyTo == null) ?? nodes[0];
    if (!root || !root.author) continue;
    const login = root.author.login;
    if (login === prAuthor) { skipped.self++; continue; }
    if (isFilteredBot(login)) { skipped.bots++; continue; }
    if (t.isOutdated && !opts.includeResolved && !(root.body ?? "").trim()) { skipped.outdated++; continue; }
    const replies = nodes.length - 1;
    const lastBy = replies > 0 ? nodes[nodes.length - 1].author?.login ?? "unknown" : null;
    findings.push({
      id: root.databaseId,
      by: `@${login}`,
      // A pathless review thread is a FILE- or PR-level review thread. It still
      // has a threadId, so it is resolvable — it must not be confused with the
      // non-thread "conversation" surface below, which is not.
      file: root.path ? `${root.path}:${root.line ?? root.originalLine ?? "?"}` : "(review thread)",
      surface: root.path ? "inline" : "review-thread",
      body: (root.body ?? "").slice(0, 400),
      thread: replies === 0 ? "no replies" : `${replies} replies — last by @${lastBy}`,
      threadId: t.id,
      resolvable: true,
      url: root.url,
    });
  }
  return { findings, skipped };
}

export type NonThreadComment = {
  databaseId: number;
  body: string | null;
  url: string;
  author: { login: string } | null;
  /** Reviews only — an APPROVED/COMMENTED verdict with an empty body is not work. */
  state?: string;
};

/**
 * Bucket the two surfaces that are NOT inline review threads:
 *
 *  - **review summaries** — the body a reviewer writes on the review itself
 *    ("LGTM, but please also…"). Distinct from its inline comments and easy to
 *    lose: a request that appears only here has no thread, so nothing ever marks
 *    it done.
 *  - **PR conversation comments** — plain comments on the PR. Where a human
 *    typically asks for something that spans files ("can this also cover X?").
 *
 * Neither is resolvable, so both come back `resolvable: false`. They were counted
 * and discarded until 2026-07-26; a reviewer asking for something in the PR
 * conversation was silently ignored while inline nits got fixed.
 */
export function bucketNonThread(
  reviews: NonThreadComment[],
  comments: NonThreadComment[],
  prAuthor: string,
): BucketResult {
  const findings: Finding[] = [];
  const skipped: Skipped = { bots: 0, self: 0, resolved: 0, outdated: 0, informational: 0 };
  const sources: { surface: Finding["surface"]; items: NonThreadComment[] }[] = [
    { surface: "review-summary", items: reviews },
    { surface: "conversation", items: comments },
  ];
  for (const { surface, items } of sources) {
    for (const c of items ?? []) {
      const body = (c.body ?? "").trim();
      // An approval with no prose is the commonest review by far.
      if (!body) { skipped.informational++; continue; }
      if (!c.author) continue;
      const login = c.author.login;
      if (login === prAuthor) { skipped.self++; continue; }
      if (isFilteredBot(login)) { skipped.bots++; continue; }
      if (isBotStatusBody(body)) { skipped.informational++; continue; }
      findings.push({
        id: c.databaseId,
        by: `@${login}`,
        file: surface === "review-summary" ? "(review summary)" : "(PR conversation)",
        surface,
        body: body.slice(0, 400),
        thread: "not a thread — reply + react to close",
        threadId: null,
        resolvable: false,
        url: c.url,
      });
    }
  }
  return { findings, skipped };
}

import { execFileSync } from "node:child_process";

import { repoRoot } from "./repo-root.ts";
import { readGitConfig } from "./sync-context.ts";
import { resolveDefaultBranch } from "./merge-precheck.ts";
import { dirname } from "node:path";

function sh(cmd: string, args: string[]): string {
  return execFileSync(cmd, args, { encoding: "utf8", maxBuffer: 64 * 1024 * 1024, timeout: 120_000, cwd: repoRoot() });
}

const QUERY = `
query($owner:String!, $repo:String!, $pr:Int!, $threadCursor:String) {
  repository(owner:$owner, name:$repo) {
    pullRequest(number:$pr) {
      number title state url isDraft merged
      headRefName headRefOid baseRefName
      author { login }
      reviewThreads(first: 100, after: $threadCursor) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id isResolved isOutdated
          comments(first: 50) {
            nodes {
              databaseId path line originalLine body url
              author { login }
              replyTo { databaseId }
            }
          }
        }
      }
      reviews(first: 50) {
        nodes { databaseId body url state author { login } }
      }
      comments(first: 50) {
        nodes { databaseId body url author { login } }
      }
    }
  }
  rateLimit { remaining cost resetAt }
}`;

// The git contract's per-machine overlay (.claude.git.config.local) is gitignored, so it
// only exists in the MAIN clone — never in a worktree. Resolve the main clone from the
// common git dir before reading it.
function mainCloneOf(root: string): string {
  const commonDir = sh("git", ["-C", root, "rev-parse", "--path-format=absolute", "--git-common-dir"]).trim();
  return dirname(commonDir);
}

function currentRepo(): { owner: string; repo: string; defaultBranch: string } {
  const out = sh("gh", ["repo", "view", "--json", "nameWithOwner,defaultBranchRef"]);
  const j = JSON.parse(out);
  const [owner, repo] = j.nameWithOwner.split("/");
  const { cfg } = readGitConfig(mainCloneOf(repoRoot()));
  return { owner, repo, defaultBranch: resolveDefaultBranch(cfg, j.defaultBranchRef?.name) };
}

type Target =
  | { owner: string; repo: string; pr: number }
  | { owner: string; repo: string; noPr: true; branch: string; defaultBranch: string };

function resolveTarget(sel: Selector): Target {
  switch (sel.kind) {
    case "url":
    case "numberInRepo":
      return { owner: sel.owner, repo: sel.repo, pr: sel.pr };
    case "number": {
      const { owner, repo } = currentRepo();
      return { owner, repo, pr: sel.pr };
    }
    case "latestByAuthor": {
      const { owner, repo } = currentRepo();
      const pr = Number(sh("gh", ["pr", "list", "--author", sel.author, "--limit", "1", "--json", "number", "--jq", ".[0].number"]).trim());
      if (!pr) throw new Error(`No open PR by @${sel.author} in ${owner}/${repo}`);
      return { owner, repo, pr };
    }
    case "current": {
      const { owner, repo, defaultBranch } = currentRepo();
      const branch = sh("git", ["branch", "--show-current"]).trim();
      const lookup = choosePrLookup(branch);
      let pr: number;
      if (lookup.by === "head") {
        pr = Number(sh("gh", ["pr", "list", "--head", lookup.branch, "--json", "number", "--jq", ".[0].number"]).trim());
      } else {
        // Detached HEAD (e.g. worktree at a bare commit): find the PR for this exact commit.
        const sha = sh("git", ["rev-parse", "HEAD"]).trim();
        pr = Number(sh("gh", ["api", `repos/${owner}/${repo}/commits/${sha}/pulls`, "--jq", ".[0].number"]).trim());
      }
      if (!pr) {
        if (lookup.by === "sha") {
          throw new Error("Detached HEAD with no PR for this commit. Checkout a branch or pass a PR number explicitly.");
        }
        return { owner, repo, noPr: true, branch, defaultBranch };
      }
      return { owner, repo, pr };
    }
  }
}

function fetchThreads(owner: string, repo: string, pr: number) {
  let cursor: string | null = null;
  let meta: any = null;
  let rateLimit: unknown = null;
  let reviews: NonThreadComment[] = [];
  let comments: NonThreadComment[] = [];
  const threads: ReviewThread[] = [];
  for (;;) {
    const vars = ["-f", `owner=${owner}`, "-f", `repo=${repo}`, "-F", `pr=${pr}`];
    if (cursor) vars.push("-f", `threadCursor=${cursor}`);
    const out = sh("gh", ["api", "graphql", "-f", `query=${QUERY}`, ...vars]);
    const root = JSON.parse(out);
    const data = root.data.repository.pullRequest;
    rateLimit = root.data.rateLimit;
    if (!meta) {
      meta = {
        title: data.title, state: data.state, merged: data.merged,
        headRef: data.headRefName, headSha: data.headRefOid, baseRef: data.baseRefName,
        author: data.author?.login ?? "",
      };
      reviews = data.reviews?.nodes ?? [];
      comments = data.comments?.nodes ?? [];
    }
    threads.push(...data.reviewThreads.nodes);
    if (data.reviewThreads.pageInfo.hasNextPage) cursor = data.reviewThreads.pageInfo.endCursor;
    else break;
  }
  return { meta, threads, rateLimit, reviews, comments };
}

function mirrorPr(owner: string, repo: string, pr: number): string {
  const ref = `pull/${pr}/head:refs/pr/${pr}`;
  // cwd-anchored: these fetches WRITE refs/pr/* — unanchored they would land in whatever
  // repo the persistent shell is parked in. See repo-root.ts.
  const cwd = repoRoot();
  try {
    execFileSync("git", ["fetch", "origin", ref], { stdio: "ignore", timeout: 120_000, cwd });
  } catch {
    // origin may be absent or point elsewhere (fork); fall back to the canonical URL.
    // This fetch is allowed to throw — a genuine network failure must surface, not be swallowed.
    execFileSync("git", ["fetch", `https://github.com/${owner}/${repo}.git`, ref], { stdio: "ignore", timeout: 120_000, cwd });
  }
  return execFileSync("git", ["rev-parse", `refs/pr/${pr}`], { encoding: "utf8", cwd }).trim();
}

async function main(): Promise<void> {
  const { selector, flags } = parsePrArgs(process.argv.slice(2));
  const target = resolveTarget(selector);
  if ("noPr" in target) {
    const onDefaultBranch = target.branch === target.defaultBranch;
    process.stdout.write(JSON.stringify({
      noPr: true, owner: target.owner, repo: target.repo,
      branch: target.branch, defaultBranch: target.defaultBranch, onDefaultBranch,
    }, null, 2) + "\n");
    return;
  }
  const { owner, repo, pr } = target;
  const { meta, threads, rateLimit, reviews, comments } = fetchThreads(owner, repo, pr);
  const inline = bucketFindings(threads, meta.author, {
    includeResolved: flags.includeResolved,
  });
  // Review summaries + PR conversation comments. Counted and thrown away before
  // 2026-07-26, which meant a reviewer asking for something in the PR
  // conversation was silently ignored while inline nits got fixed.
  const nonThread = flags.noConversation
    ? { findings: [], skipped: { bots: 0, self: 0, resolved: 0, outdated: 0, informational: 0 } }
    : bucketNonThread(reviews, comments, meta.author);
  // Resolvable findings first: they carry the fix-and-resolve loop, and a round
  // that runs out of budget should have spent it on those.
  const findings = [...inline.findings, ...nonThread.findings];
  const skipped: Skipped = {
    bots: inline.skipped.bots + nonThread.skipped.bots,
    self: inline.skipped.self + nonThread.skipped.self,
    resolved: inline.skipped.resolved,
    outdated: inline.skipped.outdated,
    informational: nonThread.skipped.informational,
  };
  let headSha = meta.headSha;
  try {
    headSha = mirrorPr(owner, repo, pr);
  } catch (e) {
    process.stderr.write(`warn: could not mirror PR locally (${(e as Error).message}); using GraphQL head SHA\n`);
  }
  if (flags.noConversation) {
    process.stderr.write("note: --no-conversation — review summaries and PR conversation comments were not fetched\n");
  } else if (nonThread.findings.length > 0) {
    const n = nonThread.findings.length;
    process.stderr.write(
      `note: ${n} non-thread finding${n === 1 ? "" : "s"} (review summary / PR conversation) — ` +
        "these have NO resolvable thread: close each with a reply + reaction, never a resolve call\n",
    );
  }
  const envelope = {
    owner, repo, pr,
    title: meta.title, state: meta.state, merged: meta.merged,
    headRef: meta.headRef, headSha, baseRef: meta.baseRef,
    findings, skipped, rateLimit,
    // Explicit split so a round can see at a glance whether it owes any
    // reply-and-react closures on top of the resolvable threads.
    counts: {
      total: findings.length,
      resolvable: findings.filter((f) => f.resolvable).length,
      nonThread: nonThread.findings.length,
    },
  };
  process.stdout.write(JSON.stringify(envelope, null, 2) + "\n");
}

if (import.meta.main) {
  main().catch((e) => {
    process.stderr.write(`error: ${(e as Error).message}\n`);
    process.exit(1);
  });
}
