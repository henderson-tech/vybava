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

test("createWorktree fast-forwards a stale local branch to the fetched head", () => {
  const root = mkdtempSync(join(tmpdir(), "gitkit-wt-"));
  const git = (cwd: string, ...args: string[]) =>
    execFileSync("git", ["-c", "user.name=t", "-c", "user.email=t@example.com", ...args], { cwd, encoding: "utf8" }).trim();
  git(root, "init", "-q", "--bare", "-b", "main", "origin.git");
  git(root, "clone", "-q", "origin.git", "author");
  const author = join(root, "author");
  git(author, "commit", "-q", "--allow-empty", "-m", "c1");
  git(author, "push", "-q", "origin", "HEAD:feature");
  git(root, "clone", "-q", "origin.git", "reviewer");
  const reviewer = join(root, "reviewer");
  git(reviewer, "branch", "-q", "feature", "origin/feature"); // local copy, about to go stale
  git(author, "commit", "-q", "--allow-empty", "-m", "c2");
  git(author, "push", "-q", "origin", "HEAD:feature");

  const path = join(root, "wt");
  const diverged = createWorktree((cmd, args) => git(reviewer, ...args), path, "feature", true);
  assert.equal(diverged, false);
  assert.equal(git(path, "rev-parse", "HEAD"), git(author, "rev-parse", "HEAD"));
});
