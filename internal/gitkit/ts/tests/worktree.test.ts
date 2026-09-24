import { test } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { parseWorktreeList, mainCloneOf, findWorktreeForBranch, ensurePlan, createWorktree } from "../bin/worktree.ts";

const PORCELAIN = [
  "worktree /Users/me/repo",
  "HEAD aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "branch refs/heads/main",
  "",
  "worktree /Users/me/repo/.worktrees/pr-201",
  "HEAD bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
  "branch refs/heads/feat-offer-race",
  "",
  "worktree /Users/me/repo/.worktrees/detached",
  "HEAD cccccccccccccccccccccccccccccccccccccccc",
  "detached",
  "",
].join("\n");

test("parseWorktreeList parses path/HEAD/branch blocks, branch null when detached", () => {
  const wts = parseWorktreeList(PORCELAIN);
  assert.equal(wts.length, 3);
  assert.deepEqual(wts[0], { path: "/Users/me/repo", head: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", branch: "refs/heads/main" });
  assert.equal(wts[1].branch, "refs/heads/feat-offer-race");
  assert.equal(wts[2].branch, null);
});

test("mainCloneOf returns the first worktree (the primary)", () => {
  assert.equal(mainCloneOf(parseWorktreeList(PORCELAIN)), "/Users/me/repo");
  assert.equal(mainCloneOf([]), null);
});

test("findWorktreeForBranch matches refs/heads/<headRef>", () => {
  const wts = parseWorktreeList(PORCELAIN);
  assert.equal(findWorktreeForBranch(wts, "feat-offer-race"), "/Users/me/repo/.worktrees/pr-201");
  assert.equal(findWorktreeForBranch(wts, "nope"), null);
});

test("ensurePlan reuses an existing worktree for the branch", () => {
  const wts = parseWorktreeList(PORCELAIN);
  assert.deepEqual(ensurePlan(wts, "feat-offer-race", 201), {
    action: "reuse", path: "/Users/me/repo/.worktrees/pr-201", selfCreated: false, mainClone: "/Users/me/repo",
  });
});

test("ensurePlan creates .worktrees/pr-<N> under the main clone when none exists", () => {
  const wts = parseWorktreeList(PORCELAIN);
  assert.deepEqual(ensurePlan(wts, "feat-new", 202), {
    action: "create", path: "/Users/me/repo/.worktrees/pr-202", selfCreated: true, mainClone: "/Users/me/repo",
  });
});

test("ensurePlan throws when the main clone can't be determined", () => {
  assert.throws(() => ensurePlan([], "feat-x", 1), /main clone/);
});

// A bare "GitHub" whose PR 7 head is published at refs/pull/7/head, like the real one.
function prFixture(opts: { fork?: boolean } = {}) {
  const root = mkdtempSync(join(tmpdir(), "gitkit-wt-"));
  const git = (cwd: string, ...args: string[]) =>
    execFileSync("git", ["-c", "user.name=t", "-c", "user.email=t@example.com", ...args], { cwd, encoding: "utf8" }).trim();
  git(root, "init", "-q", "--bare", "-b", "main", "origin.git");
  git(root, "clone", "-q", "origin.git", "author");
  const author = join(root, "author");
  const publish = () => {
    if (!opts.fork) git(author, "push", "-q", "origin", "HEAD:feature");
    git(author, "push", "-q", "-f", "origin", "HEAD:refs/pull/7/head");
  };
  git(author, "commit", "-q", "--allow-empty", "-m", "c1");
  git(author, "push", "-q", "origin", "HEAD:main");
  publish();
  git(root, "clone", "-q", "origin.git", "reviewer");
  const reviewer = join(root, "reviewer");
  const run = (localExists: boolean) =>
    createWorktree((_cmd, args) => git(reviewer, ...args), join(root, "wt"), "feature", 7, localExists);
  return { root, git, author, reviewer, publish, run, wt: join(root, "wt") };
}

test("createWorktree fast-forwards a stale local branch to the PR head", () => {
  const f = prFixture();
  f.git(f.reviewer, "branch", "-q", "feature", "origin/feature"); // local copy, about to go stale
  f.git(f.author, "commit", "-q", "--allow-empty", "-m", "c2");
  f.publish();
  assert.equal(f.run(true), false);
  assert.equal(f.git(f.wt, "rev-parse", "HEAD"), f.git(f.author, "rev-parse", "HEAD"));
});

test("createWorktree reports a local branch AHEAD of the PR head as diverged, untouched", () => {
  const f = prFixture();
  f.git(f.reviewer, "branch", "-q", "feature", "origin/feature");
  f.git(f.reviewer, "worktree", "add", "-q", join(f.root, "scratch"), "feature");
  f.git(join(f.root, "scratch"), "commit", "-q", "--allow-empty", "-m", "unpushed");
  const local = f.git(join(f.root, "scratch"), "rev-parse", "HEAD");
  f.git(f.reviewer, "worktree", "remove", join(f.root, "scratch"));
  assert.equal(f.run(true), true);
  assert.equal(f.git(f.wt, "rev-parse", "HEAD"), local);
});

test("createWorktree opens a fork PR whose head branch is not on origin", () => {
  const f = prFixture({ fork: true });
  assert.equal(f.run(false), false);
  assert.equal(f.git(f.wt, "rev-parse", "HEAD"), f.git(f.author, "rev-parse", "HEAD"));
});
