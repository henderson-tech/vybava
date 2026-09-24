// tdd-classify.ts — the deterministic, path-based half of the two-gate TDD classifier.
// Given a finding's file path, return the hard-skip category (always direct-fix, no test)
// or null (→ apply the behavioral gates: is it a defect? is it unit/intg-reproducible?).
// Run: node tdd-classify.ts <path>  → prints the category or "null". Erasable TS, zero deps.

export type SkipCategory = "migration" | "deps" | "ci" | "iac" | "generated" | "docs" | null;

// Ordered most-specific → least: ci before generic yaml/iac; migration before everything.
export function skipCategory(path: string): SkipCategory {
  const p = path.toLowerCase();

  if (/(^|\/)(migrations?|__migrations__)\//.test(p) || /(^|\/)migration\//.test(p)) return "migration";

  if (/(^|\/)(package-lock\.json|pnpm-lock\.yaml|yarn\.lock|bun\.lockb?|composer\.lock|gemfile\.lock|cargo\.lock|go\.sum|poetry\.lock)$/.test(p)) return "deps";

  if (/(^|\/)\.github\/workflows\//.test(p) || /(^|\/)\.gitlab-ci\.ya?ml$/.test(p) || /(^|\/)\.(circleci|buildkite)\//.test(p) || /(^|\/)azure-pipelines\.ya?ml$/.test(p)) return "ci";

  if (/(^|\/)dockerfile(\.[a-z0-9]+)?$/.test(p) || /docker-compose[^/]*\.ya?ml$/.test(p) || /\.(tf|tfvars)$/.test(p) || /(^|\/)(nginx|k8s|kubernetes|helm|terraform|ansible)\//.test(p) || /(^|\/)\.env(\.[a-z0-9]+)?$/.test(p)) return "iac";

  if (/\.(generated|gen)\.[a-z]+$/.test(p) || /\.d\.ts$/.test(p) || /(^|\/)(dist|build|out|\.next|coverage|__generated__|generated)\//.test(p)) return "generated";

  if (/\.(md|mdx|rst|txt)$/.test(p) || /(^|\/)docs?\//.test(p)) return "docs";

  return null;
}

async function main(): Promise<void> {
  const path = process.argv.slice(2).find((a) => !a.startsWith("--"));
  if (!path) throw new Error("usage: tdd-classify.ts <path>");
  process.stdout.write(`${skipCategory(path) ?? "null"}\n`);
}

if (import.meta.main) {
  main().catch((e) => {
    process.stderr.write(`error: ${(e as Error).message}\n`);
    process.exit(1);
  });
}
