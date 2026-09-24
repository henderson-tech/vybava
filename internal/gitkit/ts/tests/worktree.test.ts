import { test } from "node:test";
import assert from "node:assert/strict";
import { parseWorktreeList, mainCloneOf, findWorktreeForBranch, ensurePlan } from "../bin/worktree.ts";

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
