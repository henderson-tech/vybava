// worktree.ts — discover-or-create an isolated worktree for a PR head branch.
// Run: node worktree.ts ensure <headRef> <pr> [--repo <abs path>]  → prints JSON {action, path, selfCreated, mainClone}.
// --repo (or GIT_SKILL_REPO) anchors the git calls via repo-root.ts; otherwise ambient cwd.
// Zero npm deps: shells out to `git`, Node stdlib only. Erasable TS only.

export type Worktree = { path: string; head: string | null; branch: string | null };

export function parseWorktreeList(porcelain: string): Worktree[] {
  const out: Worktree[] = [];
  let cur: Worktree | null = null;
  const flush = () => { if (cur) out.push(cur); cur = null; };
  for (const raw of porcelain.split("\n")) {
    const line = raw.trimEnd();
    if (line.startsWith("worktree ")) {
      flush();
      cur = { path: line.slice("worktree ".length), head: null, branch: null };
    } else if (cur && line.startsWith("HEAD ")) {
      cur.head = line.slice("HEAD ".length);
    } else if (cur && line.startsWith("branch ")) {
      cur.branch = line.slice("branch ".length);
    }
  }
  flush();
  return out;
}

export function mainCloneOf(worktrees: Worktree[]): string | null {
  return worktrees[0]?.path ?? null;
}

export function findWorktreeForBranch(worktrees: Worktree[], headRef: string): string | null {
  const want = `refs/heads/${headRef}`;
  for (const w of worktrees) if (w.branch === want) return w.path;
  return null;
}

export type EnsurePlan = {
  action: "reuse" | "create";
  path: string;
  selfCreated: boolean;
  mainClone: string;
  // The local branch holds commits origin lacks; the worktree was left on it, not reset.
  diverged?: boolean;
};

export function ensurePlan(worktrees: Worktree[], headRef: string, pr: number): EnsurePlan {
  const mainClone = mainCloneOf(worktrees);
  if (!mainClone) throw new Error("could not determine the main clone from `git worktree list`");
  const existing = findWorktreeForBranch(worktrees, headRef);
  if (existing) return { action: "reuse", path: existing, selfCreated: false, mainClone };
  return { action: "create", path: `${mainClone}/.worktrees/pr-${pr}`, selfCreated: true, mainClone };
}

import { execFileSync } from "node:child_process";

import { repoRoot } from "./repo-root.ts";

function sh(cmd: string, args: string[]): string {
  return execFileSync(cmd, args, { encoding: "utf8", maxBuffer: 32 * 1024 * 1024, timeout: 120_000, cwd: repoRoot() });
}

// createWorktree anchors on GitHub's refs/pull/<N>/head — it exists for fork and
// same-repo PRs alike, where origin/<headRef> is absent for a fork. A leftover local
// branch of the same name may be days behind (a round seeded from it reviews and
// pushes stale code), so it is fast-forwarded to the PR head; one carrying commits the
// PR head lacks — behind, diverged or strictly ahead — is someone's work: left as is
// and reported (returns true), never reset.
export function createWorktree(git: (cmd: string, args: string[]) => string, path: string, headRef: string, pr: number, localExists: boolean): boolean {
  const prHead = `refs/pr/${pr}`;
  git("git", ["fetch", "origin", `+refs/pull/${pr}/head:${prHead}`]);
  if (!localExists) {
    git("git", ["worktree", "add", "-b", headRef, path, prHead]);
    try {
      git("git", ["fetch", "origin", headRef]); // same-repo PR: track the head branch
      git("git", ["-C", path, "branch", "--set-upstream-to", `origin/${headRef}`]);
    } catch {
      // Fork PR: the head branch is not on origin, so there is nothing to track.
    }
    return false;
  }
  git("git", ["worktree", "add", path, headRef]);
  const localOnly = Number(git("git", ["-C", path, "rev-list", "--count", `${prHead}..HEAD`]).trim());
  if (localOnly > 0) return true;
  git("git", ["-C", path, "merge", "--ff-only", "--quiet", prHead]);
  return false;
}

function localBranchExists(headRef: string): boolean {
  try {
    sh("git", ["show-ref", "--verify", "--quiet", `refs/heads/${headRef}`]);
    return true;
  } catch {
    // `git show-ref --verify` exits non-zero when the ref is absent — that's the answer,
    // not an error. (No local branch yet → we'll create a tracking branch below.)
    return false;
  }
}

async function main(): Promise<void> {
  const [sub, headRef, prRaw] = process.argv.slice(2);
  if (sub !== "ensure" || !headRef || !prRaw) {
    throw new Error("usage: worktree.ts ensure <headRef> <pr>");
  }
  const pr = Number(prRaw);
  if (!Number.isInteger(pr) || pr <= 0) throw new Error(`bad pr number: "${prRaw}"`);

  const worktrees = parseWorktreeList(sh("git", ["worktree", "list", "--porcelain"]));
  const plan = ensurePlan(worktrees, headRef, pr);

  if (plan.action === "create") plan.diverged = createWorktree(sh, plan.path, headRef, pr, localBranchExists(headRef)) || undefined;
  process.stdout.write(JSON.stringify(plan, null, 2) + "\n");
}

if (import.meta.main) {
  main().catch((e) => {
    process.stderr.write(`error: ${(e as Error).message}\n`);
    process.exit(1);
  });
}
