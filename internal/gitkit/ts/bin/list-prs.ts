// list-prs.ts — select the current repo's open PRs authored by the viewer that need action.
// Run: node list-prs.ts [--repo <abs path>]  → prints JSON [{pr, headRef, reason}] to stdout.
// --repo (or GIT_SKILL_REPO) anchors the gh calls via repo-root.ts; otherwise ambient cwd.
// Zero npm deps: shells out to `gh`, Node stdlib only. Erasable TS only.

export type PrNode = {
  number: number;
  headRefName: string;
  reviewDecision: string | null;
  author: { login: string } | null;
  reviewThreads: { nodes: { isResolved: boolean }[] };
};

export function countUnresolved(threads: { isResolved: boolean }[]): number {
  return threads.filter((t) => !t.isResolved).length;
}

export function qualifies(reviewDecision: string | null, unresolved: number): boolean {
  return (reviewDecision ?? "").toUpperCase() === "CHANGES_REQUESTED" || unresolved > 0;
}

export function reasonFor(reviewDecision: string | null, unresolved: number): string {
  const parts: string[] = [];
  if ((reviewDecision ?? "").toUpperCase() === "CHANGES_REQUESTED") parts.push("CHANGES_REQUESTED");
  if (unresolved > 0) parts.push(`${unresolved} unresolved thread${unresolved === 1 ? "" : "s"}`);
  return parts.join(" + ");
}

export type Selected = { pr: number; headRef: string; reason: string };

export function selectPrs(nodes: PrNode[], viewerLogin: string): Selected[] {
  const out: Selected[] = [];
  for (const n of nodes) {
    if (n.author?.login !== viewerLogin) continue;
    const unresolved = countUnresolved(n.reviewThreads?.nodes ?? []);
    if (!qualifies(n.reviewDecision, unresolved)) continue;
    out.push({ pr: n.number, headRef: n.headRefName, reason: reasonFor(n.reviewDecision, unresolved) });
  }
  return out;
}

import { execFileSync } from "node:child_process";

import { repoRoot } from "./repo-root.ts";

function sh(cmd: string, args: string[]): string {
  return execFileSync(cmd, args, { encoding: "utf8", maxBuffer: 32 * 1024 * 1024, timeout: 60_000, cwd: repoRoot() });
}

const QUERY = `
query($owner:String!, $repo:String!) {
  viewer { login }
  repository(owner:$owner, name:$repo) {
    pullRequests(states: OPEN, first: 50, orderBy: {field: UPDATED_AT, direction: DESC}) {
      nodes {
        number headRefName reviewDecision
        author { login }
        reviewThreads(first: 100) { nodes { isResolved } }
      }
    }
  }
}`;

async function main(): Promise<void> {
  const repo = JSON.parse(sh("gh", ["repo", "view", "--json", "nameWithOwner"]));
  const [owner, name] = repo.nameWithOwner.split("/");
  const out = sh("gh", ["api", "graphql", "-f", `query=${QUERY}`, "-f", `owner=${owner}`, "-f", `repo=${name}`]);
  const root = JSON.parse(out);
  const viewer: string = root.data.viewer.login;
  const nodes: PrNode[] = root.data.repository.pullRequests.nodes;
  process.stdout.write(JSON.stringify(selectPrs(nodes, viewer), null, 2) + "\n");
}

if (import.meta.main) {
  main().catch((e) => {
    process.stderr.write(`error: ${(e as Error).message}\n`);
    process.exit(1);
  });
}
