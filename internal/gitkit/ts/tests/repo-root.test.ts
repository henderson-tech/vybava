import { test } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { mkdtempSync, mkdirSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { explicitRepoArg, repoRoot, resetRepoRootCache } from "../bin/repo-root.ts";

test("explicitRepoArg reads --repo <path>", () => {
  assert.equal(explicitRepoArg(["--repo", "/tmp/x"]), "/tmp/x");
});

test("explicitRepoArg reads --repo=<path>", () => {
  assert.equal(explicitRepoArg(["--repo=/tmp/y"]), "/tmp/y");
});

test("explicitRepoArg is undefined when absent", () => {
  const saved = process.env.GIT_SKILL_REPO;
  delete process.env.GIT_SKILL_REPO;
  try {
    assert.equal(explicitRepoArg(["--pr", "7"]), undefined);
  } finally {
    if (saved !== undefined) process.env.GIT_SKILL_REPO = saved;
  }
});

test("explicitRepoArg falls back to GIT_SKILL_REPO", () => {
  const saved = process.env.GIT_SKILL_REPO;
  process.env.GIT_SKILL_REPO = "/tmp/z";
  try {
    assert.equal(explicitRepoArg([]), "/tmp/z");
  } finally {
    if (saved === undefined) delete process.env.GIT_SKILL_REPO;
    else process.env.GIT_SKILL_REPO = saved;
  }
});

test("a nonexistent --repo throws instead of silently using cwd", () => {
  resetRepoRootCache();
  assert.throws(() => repoRoot(["--repo", "/definitely/not/here"]), /not an existing directory/);
  resetRepoRootCache();
});

// The whole point: an explicit anchor must win over the process cwd. Build a throwaway
// repo, resolve against it while cwd is elsewhere, and assert we get the anchor's root.
test("an explicit --repo wins over the ambient cwd", () => {
  const dir = mkdtempSync(join(tmpdir(), "repo-root-test-"));
  execFileSync("git", ["init", "-q"], { cwd: dir });
  const expected = execFileSync("git", ["rev-parse", "--show-toplevel"], { cwd: dir, encoding: "utf8" }).trim();

  resetRepoRootCache();
  try {
    assert.equal(repoRoot(["--repo", dir]), expected);
    assert.notEqual(repoRoot(["--repo", dir]), process.cwd());
  } finally {
    resetRepoRootCache();
  }
});

// A path INSIDE the repo must resolve up to the toplevel, not return itself — that is the
// drift case: a helper parked in apps/web still has to answer about the repository.
test("--repo pointing into a subdirectory resolves to the toplevel", () => {
  const dir = mkdtempSync(join(tmpdir(), "repo-root-sub-"));
  execFileSync("git", ["init", "-q"], { cwd: dir });
  const expected = execFileSync("git", ["rev-parse", "--show-toplevel"], { cwd: dir, encoding: "utf8" }).trim();
  const sub = join(dir, "apps", "web");
  mkdirSync(sub, { recursive: true });

  resetRepoRootCache();
  try {
    assert.equal(repoRoot(["--repo", sub]), expected);
  } finally {
    resetRepoRootCache();
  }
});

test("repoRoot memoizes so one process cannot straddle two repos", () => {
  const a = mkdtempSync(join(tmpdir(), "repo-root-a-"));
  const b = mkdtempSync(join(tmpdir(), "repo-root-b-"));
  execFileSync("git", ["init", "-q"], { cwd: a });
  execFileSync("git", ["init", "-q"], { cwd: b });

  resetRepoRootCache();
  try {
    const first = repoRoot(["--repo", a]);
    assert.equal(repoRoot(["--repo", b]), first, "second call must reuse the memoized root");
  } finally {
    resetRepoRootCache();
  }
});
