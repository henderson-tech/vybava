// classify-paths.ts — partition working-tree paths into commit / secret / artifact.
// Run: node classify-paths.ts [--repo <abs path>]  → reads `git status --porcelain`, prints JSON partition.
// --repo (or GIT_SKILL_REPO) anchors the git calls via repo-root.ts; otherwise ambient cwd.
// Used by /git:commit to skip (never commit) secret-shaped files + build/test artifacts,
// while committing everything else. Zero deps. Erasable TS only.

import { execFileSync } from "node:child_process";

import { repoRoot } from "./repo-root.ts";

export type Klass = "secret" | "artifact" | "commit";

const SECRET_EXT = [".pem", ".key", ".p12", ".pfx", ".keystore", ".jks", ".asc", ".gpg", ".secret", ".token"];
const SECRET_BASENAMES = new Set([".netrc", ".npmrc", ".pypirc", ".htpasswd", ".dockercfg", ".boto"]);
const ENV_TEMPLATE_SUFFIXES = [".example", ".sample", ".template", ".dist", ".defaults"];
const ARTIFACT_SEGMENTS = new Set([
  "node_modules", "allure-results", "allure-report", "playwright-report", "test-results",
  "coverage", ".nyc_output", "__pycache__", ".pytest_cache", ".cache", ".next", ".turbo",
]);
const ARTIFACT_BASENAMES = new Set([".ds_store", "thumbs.db"]);

export function classifyPath(p: string): Klass {
  const lower = p.toLowerCase();
  const segs = lower.split("/");
  const base = segs[segs.length - 1];

  // .env is secret — but .env.example / .sample / .template are committable templates.
  const isEnvTemplate = ENV_TEMPLATE_SUFFIXES.some((s) => base.endsWith(s));
  const isEnv = base === ".env" || base.startsWith(".env.") || base.endsWith(".env");
  if (isEnv && !isEnvTemplate) return "secret";

  if (/^id_(rsa|dsa|ecdsa|ed25519)\b/.test(base) && !base.endsWith(".pub")) return "secret";
  if (SECRET_EXT.some((e) => base.endsWith(e))) return "secret";
  if (SECRET_BASENAMES.has(base)) return "secret";
  if (base === "credentials" || base.startsWith("credentials.")) return "secret";
  if (base === "secrets" || base.startsWith("secrets.")) return "secret";
  if (base.startsWith("service-account") && base.endsWith(".json")) return "secret";
  if (base.startsWith("gha-creds-")) return "secret";
  if (base.includes("credentials") && base.endsWith(".json")) return "secret";

  if (segs.some((s) => ARTIFACT_SEGMENTS.has(s))) return "artifact";
  if (ARTIFACT_BASENAMES.has(base)) return "artifact";
  if (base.endsWith(".log")) return "artifact";

  return "commit";
}

export function classifyStatus(paths: string[]): { commit: string[]; secrets: string[]; artifacts: string[] } {
  const out = { commit: [] as string[], secrets: [] as string[], artifacts: [] as string[] };
  for (const p of paths) {
    const k = classifyPath(p);
    if (k === "secret") out.secrets.push(p);
    else if (k === "artifact") out.artifacts.push(p);
    else out.commit.push(p);
  }
  return out;
}

function gitStatusPaths(): string[] {
  // cwd-anchored: an ambient cwd would let a drifted shell classify ANOTHER repo's
  // dirty files while the caller commits/discards in this one. See repo-root.ts.
  const out = execFileSync("git", ["status", "--porcelain"], { encoding: "utf8", maxBuffer: 64 * 1024 * 1024, cwd: repoRoot() });
  const paths: string[] = [];
  for (const line of out.split("\n")) {
    if (!line.trim()) continue;
    let p = line.slice(3); // strip the 2-char "XY" status + space
    const arrow = p.indexOf(" -> ");
    if (arrow !== -1) p = p.slice(arrow + 4); // rename: take the new path
    p = p.replace(/^"|"$/g, ""); // unquote git-quoted paths with special chars
    paths.push(p);
  }
  return paths;
}

async function main(): Promise<void> {
  process.stdout.write(JSON.stringify(classifyStatus(gitStatusPaths()), null, 2) + "\n");
}

if (import.meta.main) {
  main().catch((e) => {
    process.stderr.write(`error: ${(e as Error).message}\n`);
    process.exit(1);
  });
}
