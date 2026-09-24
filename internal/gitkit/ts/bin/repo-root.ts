// repo-root.ts — resolve ONE explicit repository root for every git/gh call a skill makes.
// Zero deps. Erasable TS only.
//
// Why this exists: the harness Bash tool keeps a PERSISTENT shell whose cwd survives
// between tool calls (and is sometimes reset under you). A helper that lets git infer the
// repo from ambient cwd therefore answers about whatever repo the shell happens to be
// parked in — and git does not complain. `git status --porcelain` in the wrong repo is a
// valid, silent, WRONG answer, and /sync acts on that answer. Anchoring is not a
// nicety here; an unanchored helper can classify one repo's dirty files while the caller
// commits or discards in another.
//
// Contract: callers pass `--repo <abs-path>` (or `--repo=<abs>`, or GIT_SKILL_REPO) when
// they know the target; otherwise the ambient cwd's toplevel is used, exactly as before.
// Resolution is LAZY and memoized so importing a module for its pure functions (the tests
// do this) never shells out or throws outside a repo.

import { execFileSync } from "node:child_process";
import { existsSync, statSync } from "node:fs";

let cached: string | undefined;

/** Read an explicit anchor from argv (`--repo <p>` / `--repo=<p>`) or GIT_SKILL_REPO. */
export function explicitRepoArg(argv: string[] = process.argv.slice(2)): string | undefined {
  const i = argv.indexOf("--repo");
  if (i !== -1 && argv[i + 1]) return argv[i + 1];
  const eq = argv.find((a) => a.startsWith("--repo="));
  if (eq) return eq.slice("--repo=".length);
  const env = process.env.GIT_SKILL_REPO;
  return env && env.trim() ? env.trim() : undefined;
}

/**
 * Absolute toplevel of the repository this process should operate on.
 *
 * An explicit anchor wins over ambient cwd. A bad anchor throws loudly rather than
 * silently falling back — a wrong-repo answer is the failure mode we are eliminating.
 */
export function repoRoot(argv?: string[]): string {
  if (cached) return cached;
  const explicit = explicitRepoArg(argv);
  if (explicit !== undefined) {
    if (!existsSync(explicit) || !statSync(explicit).isDirectory()) {
      throw new Error(`--repo is not an existing directory: ${explicit}`);
    }
  }
  let out: string;
  try {
    out = execFileSync("git", ["rev-parse", "--show-toplevel"], {
      encoding: "utf8",
      cwd: explicit ?? process.cwd(),
      stdio: ["ignore", "pipe", "pipe"],
    });
  } catch {
    throw new Error(`not inside a git repository: ${explicit ?? process.cwd()}`);
  }
  cached = out.trim();
  return cached;
}

/** Test seam — drop the memoized root. */
export function resetRepoRootCache(): void {
  cached = undefined;
}
