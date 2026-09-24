#!/usr/bin/env node
// Resolve a project's BEFORE_REVIEW_CMD — the pre-review quiesce hook.
// Run: node before-review.ts [--repo <abs path of the checkout>]  → prints JSON to stdout.
// --repo (or GIT_SKILL_REPO) anchors the git calls via repo-root.ts; otherwise ambient cwd.
//
// Symmetric with AFTER_MERGE_CMD (resolved in merge-precheck.ts), but this one
// must be usable BEFORE a PR exists (/prm creates the PR after quiescing) and
// cheap enough to re-run every /prm round. So it is deliberately git-only: no
// `gh` call, no network, no PR number required.
//
// WHY the hook exists: entering a review loop invalidates whatever the checkout
// has RUNNING. Merge-from-main, dependency installs and codegen thrash the disk
// while dev servers, bundlers and emulators keep doing work that is already
// stale — and clients like Expo's dev client cannot survive those operations
// anyway, so they must be restarted regardless. Stopping first is faster than
// recovering after.
//
// Output: JSON { configFound, configPath, beforeReviewCmd, resolvedBeforeReviewCmd,
//                slug, branch, worktree, mainClone, isWorktree }
// `resolvedBeforeReviewCmd` is null when the project registered no hook — that is
// the normal case for most repos and means "skip the step", never an error.
//
// Usage:  node ~/.claude/lib/git/bin/before-review.ts [--pr <n>]
import { execFileSync } from "node:child_process";
import { existsSync, readFileSync } from "node:fs";
import { basename, join } from "node:path";
import { substituteHookTokens } from "./merge-precheck.ts";
import { readGitConfig } from "./sync-context.ts";
import { repoRoot } from "./repo-root.ts";

function sh(cmd: string, args: string[]): string {
  return execFileSync(cmd, args, { encoding: "utf8", timeout: 30_000, cwd: repoRoot() });
}

function main(): void {
  const prArgIndex = process.argv.indexOf("--pr");
  const pr = prArgIndex !== -1 ? Number.parseInt(process.argv[prArgIndex + 1] ?? "", 10) : 0;

  const worktree = sh("git", ["rev-parse", "--show-toplevel"]).trim();
  const branch = sh("git", ["rev-parse", "--abbrev-ref", "HEAD"]).trim();
  const first = sh("git", ["worktree", "list", "--porcelain"]).trim().split("\n")
    .find((l) => l.startsWith("worktree "));
  const mainClone = first ? first.slice("worktree ".length).trim() : worktree;

  const configPath = join(mainClone, ".claude/.claude.git.config");
  const { cfg, found: configFound } = readGitConfig(mainClone);

  const slug = basename(worktree);
  const beforeReviewCmd = cfg.BEFORE_REVIEW_CMD ?? null;
  const resolvedBeforeReviewCmd = beforeReviewCmd
    ? substituteHookTokens(beforeReviewCmd, {
      slug,
      branch,
      worktree,
      pr: Number.isInteger(pr) ? pr : 0,
    })
    : null;

  process.stdout.write(JSON.stringify({
    configFound,
    configPath,
    beforeReviewCmd,
    resolvedBeforeReviewCmd,
    slug,
    branch,
    worktree,
    mainClone,
    isWorktree: worktree !== mainClone,
  }, null, 2) + "\n");
}

if (import.meta.main) {
  try {
    main();
  } catch (e) {
    process.stderr.write(`error: ${(e as Error).message}\n`);
    process.exit(1);
  }
}
