// pr-events.ts — the self-terminating PR watcher for `prm`'s native Monitor.
// Run (under Monitor, persistent): node pr-events.ts <pr> [--every-seconds N]
//   → polls the PR and prints ONE event line per state delta, then EXITS when the
//     PR is merged/closed (the watch self-terminates — nothing to reap).
//
// Event vocabulary (one per line; the prm loop maps each to an action):
//   comment <id>                       new review-thread OR conversation comment
//   ci <prev>-><curr>                  CI rollup flip
//   review <STATE> by <login>          reviewer verdict change (APPROVED / CHANGES_REQUESTED / DISMISSED)
//   push <sha7>                        the PR head moved (incl. force-push)
//   mergeable <prev>-><curr>           MERGEABLE <-> CONFLICTING flips (UNKNOWN churn suppressed)
//   draft / ready                      draft state flips
//   merged / closed                    terminal — the watcher exits
//
// Durability: every `gh` call is timeout-bounded (a hung network call must not
// wedge the watch), startup retries, and poll failures back off exponentially
// (rate-limit-looking failures jump straight to the max backoff).
// Zero npm deps: shells out to `gh`, Node stdlib only. Erasable TS only.

// reviews: latest review STATE per reviewer login (APPROVED / CHANGES_REQUESTED /
// DISMISSED). commentIds covers BOTH unresolved review-thread comments and
// conversation (issue) comments — distinct databaseId spaces, one event stream.
// All new fields are optional for backward compatibility with older snapshots.
export type Snapshot = {
  state: string;
  ci: string;
  commentIds: number[];
  reviews?: Record<string, string>;
  headSha?: string;
  mergeable?: string; // MERGEABLE | CONFLICTING (UNKNOWN is carried over, never stored)
  isDraft?: boolean;
};
export type Events = { events: string[]; done: boolean };

function terminalEvent(state: string): string {
  return state.toLowerCase(); // "merged" | "closed"
}

export function computeEvents(prev: Snapshot | null, curr: Snapshot): Events {
  const events: string[] = [];
  const terminal = curr.state.toUpperCase() !== "OPEN";

  if (prev === null) {
    // Baseline poll: the main session already handled the existing backlog, so we emit
    // nothing for pre-existing comments — only fire if the PR is ALREADY terminal.
    if (terminal) return { events: [terminalEvent(curr.state)], done: true };
    return { events: [], done: false };
  }

  const seen = new Set(prev.commentIds);
  for (const id of curr.commentIds) if (!seen.has(id)) events.push(`comment ${id}`);
  if (prev.ci !== curr.ci) events.push(`ci ${prev.ci}->${curr.ci}`);
  // Review-state transitions: an APPROVED review with no inline comments used to
  // produce NO event (no new thread, no CI flip) and the watch stayed silent on
  // the exact "ready to merge" signal the loop exists for.
  const prevReviews = prev.reviews ?? {};
  for (const [login, state] of Object.entries(curr.reviews ?? {})) {
    if (prevReviews[login] !== state) events.push(`review ${state} by ${login}`);
  }
  // Head movement (a teammate/bot pushed, or a force-push rewrote the branch).
  if (prev.headSha !== undefined && curr.headSha !== undefined && prev.headSha !== curr.headSha) {
    events.push(`push ${curr.headSha.slice(0, 7)}`);
  }
  // Mergeability: only real MERGEABLE <-> CONFLICTING flips. GitHub reports
  // UNKNOWN while recomputing after every push — fetchSnapshot carries the
  // previous value forward instead of storing UNKNOWN, so no churn here.
  if (
    prev.mergeable !== undefined &&
    curr.mergeable !== undefined &&
    prev.mergeable !== curr.mergeable
  ) {
    events.push(`mergeable ${prev.mergeable}->${curr.mergeable}`);
  }
  if (prev.isDraft !== undefined && curr.isDraft !== undefined && prev.isDraft !== curr.isDraft) {
    events.push(curr.isDraft ? "draft" : "ready");
  }
  if (terminal) {
    events.push(terminalEvent(curr.state));
    return { events, done: true };
  }
  return { events, done: false };
}

import { execFileSync } from "node:child_process";

import { repoRoot } from "./repo-root.ts";

// Timeout-bounded: a hung gh/network call must never wedge the watch — the
// caller treats the throw as a failed poll and backs off.
function sh(cmd: string, args: string[]): string {
  return execFileSync(cmd, args, {
    encoding: "utf8",
    maxBuffer: 32 * 1024 * 1024,
    timeout: 120_000,
    cwd: repoRoot(),
  });
}

const QUERY = `
query($owner:String!, $repo:String!, $pr:Int!) {
  repository(owner:$owner, name:$repo) {
    pullRequest(number:$pr) {
      state
      commits(last: 1) { nodes { commit { statusCheckRollup { state } } } }
      reviewThreads(first: 100) {
        nodes { isResolved comments(first: 1) { nodes { databaseId } } }
      }
      latestReviews(first: 50) { nodes { state author { login } } }
      headRefOid
      mergeable
      isDraft
      comments(last: 50) { nodes { databaseId } }
    }
  }
}`;

function fetchSnapshot(owner: string, repo: string, pr: number): Snapshot {
  const out = sh("gh", ["api", "graphql", "-f", `query=${QUERY}`, "-f", `owner=${owner}`, "-f", `repo=${repo}`, "-F", `pr=${pr}`]);
  const d = JSON.parse(out).data.repository.pullRequest;
  const ci: string = d.commits?.nodes?.[0]?.commit?.statusCheckRollup?.state ?? "NONE";
  const commentIds: number[] = [];
  for (const t of d.reviewThreads?.nodes ?? []) {
    if (t.isResolved) continue;
    const id = t.comments?.nodes?.[0]?.databaseId;
    if (typeof id === "number") commentIds.push(id);
  }
  for (const c of d.comments?.nodes ?? []) {
    if (typeof c.databaseId === "number") commentIds.push(c.databaseId);
  }
  const reviews: Record<string, string> = {};
  for (const r of d.latestReviews?.nodes ?? []) {
    const login = r.author?.login;
    // COMMENTED reviews are already surfaced through their comment ids.
    if (typeof login === "string" && typeof r.state === "string" && r.state !== "COMMENTED") {
      reviews[login] = r.state;
    }
  }
  // UNKNOWN = GitHub is recomputing (fires after every push) — carry the last
  // known value forward so computeEvents only sees real flips.
  const mergeable: string =
    d.mergeable === "UNKNOWN" ? (prevMergeable ?? "MERGEABLE") : (d.mergeable ?? "MERGEABLE");
  prevMergeable = mergeable;
  return {
    state: d.state,
    ci,
    commentIds,
    reviews,
    headSha: typeof d.headRefOid === "string" ? d.headRefOid : undefined,
    mergeable,
    isDraft: d.isDraft === true,
  };
}
let prevMergeable: string | undefined;

/**
 * `--repo` accepts BOTH forms every other lib/git script uses:
 *  - an absolute (or ./-relative) repo path → the anchor for repoRoot(), which
 *    reads the same flag from argv; owner/name then comes from the startup
 *    `gh repo view` lookup run in that anchored dir → nameWithOwner stays "".
 *  - `owner/name` → pins the API target directly, but MUST be stripped from
 *    process.argv before the first repoRoot() call (repoRoot throws on a
 *    non-directory --repo value, which would fail every poll silently).
 * Anything else throws loudly at startup.
 */
export function parseRepoFlag(value: string): { nameWithOwner: string; stripFromArgv: boolean } {
  if (value === "" || value.startsWith("/") || value.startsWith(".")) {
    return { nameWithOwner: "", stripFromArgv: false };
  }
  if (/^[^/\s]+\/[^/\s]+$/.test(value)) return { nameWithOwner: value, stripFromArgv: true };
  throw new Error(`--repo expects owner/name or an absolute repo path, got: ${value}`);
}

const sleep = (ms: number) => new Promise<void>((r) => setTimeout(r, ms));

async function main(): Promise<void> {
  const argv = process.argv.slice(2);
  const pr = Number(argv.find((a) => !a.startsWith("--")));
  if (!Number.isInteger(pr) || pr <= 0) throw new Error("usage: pr-events.ts <pr> [--repo <abs repo path>|owner/name] [--every-seconds N]");
  const everyIdx = argv.indexOf("--every-seconds");
  const everyMs = Math.max(30, everyIdx >= 0 ? Number(argv[everyIdx + 1]) : 30) * 1000;

  // --repo owner/name pins the target repo explicitly. Without it the repo is
  // derived from the cwd — which silently watched the WRONG repo's PR number
  // when a cross-repo watch was started from another project's directory
  // (burned live 2026-07-24: watching FE#340 from the BE repo polled BE#340,
  // an old closed PR, and the watch self-terminated with a bogus `closed`).
  const repoIdx = argv.indexOf("--repo");
  const parsed = parseRepoFlag(repoIdx >= 0 ? String(argv[repoIdx + 1] ?? "") : "");
  let nameWithOwner = parsed.nameWithOwner;
  if (parsed.stripFromArgv) {
    // owner/name form pins the API target directly — but repoRoot() also reads
    // --repo and throws on a non-directory value, which would fail EVERY poll
    // (silently: each throw reads as a transient failure and backs off forever).
    // Strip the pair from process.argv so repoRoot falls back to ambient cwd.
    const i = process.argv.indexOf("--repo");
    if (i !== -1) process.argv.splice(i, 2);
  }

  // Startup is retried too — a transient gh hiccup at launch must not kill a
  // watch that was meant to run for hours.
  for (let attempt = 1; nameWithOwner === ""; attempt++) {
    try {
      nameWithOwner = JSON.parse(sh("gh", ["repo", "view", "--json", "nameWithOwner"])).nameWithOwner;
      break;
    } catch (e) {
      if (attempt >= 5) throw e;
      process.stderr.write(`warn: startup repo lookup failed (attempt ${attempt}/5); retrying\n`);
      await sleep(Math.min(attempt * 15_000, 60_000));
    }
  }
  const [owner, repo] = nameWithOwner.split("/");

  let prev: Snapshot | null = null;
  let failures = 0;
  for (;;) {
    let curr: Snapshot;
    try {
      curr = fetchSnapshot(owner, repo, pr);
      failures = 0;
    } catch (e) {
      // One failed poll shouldn't kill the watch (transient gh/network) — log to
      // stderr (NOT the event stream) and back off exponentially; rate-limit-
      // looking failures jump straight to the max backoff.
      failures++;
      const msg = (e as Error).message ?? "";
      const rateLimited = /rate limit|429|403/i.test(msg);
      const factor = rateLimited ? 8 : Math.min(2 ** Math.min(failures, 3), 8);
      process.stderr.write(`warn: poll failed (${msg}); backoff x${factor}\n`);
      await sleep(everyMs * factor);
      continue;
    }
    const { events, done } = computeEvents(prev, curr);
    for (const e of events) process.stdout.write(e + "\n");
    prev = curr;
    if (done) break;
    await sleep(everyMs);
  }
}

if (import.meta.main) {
  main().catch((e) => {
    process.stderr.write(`error: ${(e as Error).message}\n`);
    process.exit(1);
  });
}
