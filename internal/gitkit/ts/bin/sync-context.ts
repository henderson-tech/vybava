// sync-context.ts — resolve /sync's per-project plan from
// <repo>/.claude/.claude.git.config (KEY=value) merged with auto-detection.
// Run: node sync-context.ts --repo <abs> [--freeze]  → plan JSON on stdout.
// Zero npm deps: shells out to `git`, Node stdlib only. Erasable TS only.
//
// --freeze writes every resolved-but-unconfigured key back into the config file so the
// NEXT run is a read + execute with no discovery. Discovery (which script is the codegen,
// which paths are generated) is the slow part of /sync; git is not.

export type GitConfig = Record<string, string>;

// The per-repo git contract: <root>/.claude/.claude.git.config (committed, shared with
// every collaborator) overlaid by <root>/.claude/.claude.git.config.local (gitignored —
// one machine's own defaults, e.g. a DEFAULT_BRANCH the rest of the team has not adopted).
// A key in .local wins; either file may be absent.
export function readGitConfig(root: string): { cfg: GitConfig; found: boolean } {
  const merged: GitConfig = {};
  let found = false;
  for (const name of [".claude/.claude.git.config", ".claude/.claude.git.config.local"]) {
    const p = join(root, name);
    if (!existsSync(p)) continue;
    found = true;
    Object.assign(merged, parseConfig(readFileSync(p, "utf8")));
  }
  return { cfg: merged, found };
}

export function parseConfig(text: string): GitConfig {
  const out: GitConfig = {};
  for (const raw of text.split("\n")) {
    const line = raw.trim();
    if (!line || line.startsWith("#")) continue;
    const eq = line.indexOf("=");
    if (eq === -1) continue;
    const key = line.slice(0, eq).trim();
    let val = line.slice(eq + 1).trim();
    if ((val.startsWith('"') && val.endsWith('"')) || (val.startsWith("'") && val.endsWith("'"))) {
      val = val.slice(1, -1);
    }
    if (key) out[key] = val;
  }
  return out;
}

export function packageManager(files: string[]): "pnpm" | "bun" | "yarn" | "npm" | null {
  if (files.includes("pnpm-lock.yaml")) return "pnpm";
  if (files.includes("bun.lock") || files.includes("bun.lockb")) return "bun";
  if (files.includes("yarn.lock")) return "yarn";
  if (files.includes("package-lock.json")) return "npm";
  return null;
}

export function installCmdForLockfile(files: string[]): string | null {
  const pm = packageManager(files);
  return pm ? `${pm} install` : null;
}

export function isLocalDbUrl(url: string): boolean {
  if (!url) return false;
  return /@(localhost|127\.0\.0\.1|0\.0\.0\.0|\[::1\]|host\.docker\.internal|db)[:\/]/i.test(url);
}

function scriptCmd(pm: string, name: string): string {
  return pm === "npm" ? `npm run ${name}` : pm === "yarn" ? `yarn ${name}` : `${pm} run ${name}`;
}

function pickScript(scripts: Record<string, string>, candidates: string[]): string | null {
  for (const c of candidates) if (scripts[c]) return c;
  return null;
}

export function dbHostOf(url: string): string | null {
  return url.match(/@([^:\/?@]+)/)?.[1] ?? null;
}

/**
 * Fallback list of "this file is generated, never hand-merge it" globs, used when the repo
 * has not set GENERATED_PATHS. Deliberately conservative: a false positive here means
 * regenerating over someone's hand-written code, so only unmistakable codegen conventions.
 */
export const DEFAULT_GENERATED_GLOBS = [
  "**/generated/**",
  "**/__generated__/**",
  "**/*.gen.*",
  "**/*.generated.*",
  "**/openapi*.json",
  "**/openapi*.yaml",
  "**/openapi*.yml",
  "**/schema.graphql",
  "**/graphql.schema.json",
  "**/prisma/client/**",
];

/** Translate one glob into an anchored RegExp. `*` stops at `/`, `**` crosses it. */
export function globToRegExp(pattern: string): RegExp {
  let out = "";
  for (let i = 0; i < pattern.length; i++) {
    const c = pattern[i];
    if (c === "*") {
      if (pattern[i + 1] === "*") {
        // `**/` may match zero segments so `**/x` also matches a bare `x`.
        if (pattern[i + 2] === "/") { out += "(?:.*/)?"; i += 2; } else { out += ".*"; i += 1; }
      } else out += "[^/]*";
    } else if (c === "?") out += "[^/]";
    else if (c === "{") {
      const close = pattern.indexOf("}", i);
      if (close === -1) out += "\\{";
      else {
        out += `(?:${pattern.slice(i + 1, close).split(",").map((s) => s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")).join("|")})`;
        i = close;
      }
    } else out += c.replace(/[.+^${}()|[\]\\]/g, "\\$&");
  }
  return new RegExp(`^${out}$`);
}

/**
 * Is this repo-relative path generated output?
 *
 * A pattern with no glob metacharacter is a path prefix (`packages/api/src/generated`
 * covers everything beneath it). A pattern with no `/` is also matched against the
 * basename, so `*.gen.ts` catches `src/deep/client.gen.ts`.
 */
export function isGeneratedPath(path: string, patterns: string[]): boolean {
  const p = path.replace(/^\.\//, "");
  const base = p.split("/").pop() ?? p;
  for (const raw of patterns) {
    const pattern = raw.trim().replace(/^\.\//, "").replace(/\/$/, "");
    if (!pattern) continue;
    if (!/[*?{]/.test(pattern)) {
      if (p === pattern || p.startsWith(`${pattern}/`)) return true;
      continue;
    }
    const re = globToRegExp(pattern);
    if (re.test(p)) return true;
    if (!pattern.includes("/") && re.test(base)) return true;
  }
  return false;
}

/** Which half of /sync runs. Branch identity decides — never the worktree location. */
export function detectMode(currentBranch: string, defaultBranch: string): "main" | "branch" {
  return currentBranch === defaultBranch ? "main" : "branch";
}

/**
 * Append the keys that were auto-detected but not written down yet, so the next run needs
 * no discovery. Never rewrites a key the file already sets — the user's config wins, and a
 * freeze must be idempotent.
 */
export function freezeConfig(
  existing: string,
  resolved: Record<string, string | null>,
  today: string,
): { text: string; added: string[] } {
  const have = parseConfig(existing);
  const added: string[] = [];
  let block = "";
  for (const [key, value] of Object.entries(resolved)) {
    if (value == null || value === "" || key in have) continue;
    block += `${key}=${value}\n`;
    added.push(key);
  }
  if (!added.length) return { text: existing, added };
  const head = existing && !existing.endsWith("\n") ? `${existing}\n` : existing;
  return { text: `${head}\n# auto-detected by /sync ${today} — edit freely, /sync never overwrites\n${block}`, added };
}

import { execFileSync } from "node:child_process";
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";

import { repoRoot } from "./repo-root.ts";

async function main(): Promise<void> {
  // Anchored: honours an explicit --repo/GIT_SKILL_REPO so a drifted shell cwd cannot
  // silently resolve (and then migrate/install against) a DIFFERENT repo. See repo-root.ts.
  const root = repoRoot();
  const at = (p: string) => join(root, p);

  const configPath = at(".claude/.claude.git.config"); // --freeze writes the committed file
  const { cfg, found: configFound } = readGitConfig(root);

  const LOCKFILES = ["pnpm-lock.yaml", "bun.lock", "bun.lockb", "yarn.lock", "package-lock.json"];
  const lockfiles = LOCKFILES.filter((f) => existsSync(at(f)));
  const pm = packageManager(lockfiles);

  let scripts: Record<string, string> = {};
  if (existsSync(at("package.json"))) {
    try {
      scripts = JSON.parse(readFileSync(at("package.json"), "utf8")).scripts ?? {};
    } catch (e) {
      // malformed package.json — recover to no-scripts rather than crashing the whole plan.
      process.stderr.write(`warn: could not parse package.json (${(e as Error).message})\n`);
    }
  }

  let defaultBranch = cfg.DEFAULT_BRANCH ?? "";
  if (!defaultBranch) {
    try {
      defaultBranch = execFileSync("git", ["symbolic-ref", "--short", "refs/remotes/origin/HEAD"], { encoding: "utf8", stdio: ["ignore", "pipe", "ignore"], cwd: root })
        .trim().replace(/^origin\//, "");
    } catch {
      defaultBranch = "main"; // origin/HEAD not set — fall back to main.
    }
  }

  const migrateName = pm ? pickScript(scripts, ["db:migrate", "migrate", "migration:run", "prisma:migrate"]) : null;
  const backupName = pm ? pickScript(scripts, ["db:backup", "backup"]) : null;
  const regenName = pm ? pickScript(scripts, ["api:generate", "codegen", "gen:types", "generate", "orval", "openapi:generate", "prisma:generate"]) : null;
  const verifyName = pm ? pickScript(scripts, ["typecheck", "type-check", "tsc", "check", "build"]) : null;

  // Branch identity picks the mode; worktree-ness is reported but never decides. A detached
  // HEAD reports as "HEAD" and can match no branch, so it lands in branch-mode and the skill
  // stops on its own guard rather than silently treating it as the default branch.
  const gitOut = (args: string[]): string | null => {
    try {
      return execFileSync("git", args, { encoding: "utf8", stdio: ["ignore", "pipe", "ignore"], cwd: root }).trim();
    } catch {
      return null;
    }
  };
  const currentBranch = gitOut(["rev-parse", "--abbrev-ref", "HEAD"]) ?? "HEAD";
  const upstream = gitOut(["rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}"]);
  const gitDir = gitOut(["rev-parse", "--absolute-git-dir"]);
  const gitCommonDir = gitOut(["rev-parse", "--path-format=absolute", "--git-common-dir"]);
  const isWorktree = !!gitDir && !!gitCommonDir && gitDir !== gitCommonDir;

  // Deterministic local-DB guard: read the DB-URL var from env / .env, emit only
  // localDbOk + host (NEVER the URL/credentials). null = unknown → the skill must STOP.
  const localDbUrlVar = cfg.LOCAL_DB_URL_VAR ?? "DATABASE_URL";
  let dbUrlRaw: string | null = process.env[localDbUrlVar] ?? null;
  if (!dbUrlRaw) {
    for (const f of [".env.local", ".env"]) {
      if (!existsSync(at(f))) continue;
      const esc = localDbUrlVar.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
      const m = readFileSync(at(f), "utf8").match(new RegExp(`^${esc}=(.*)$`, "m"));
      if (m) { dbUrlRaw = m[1].trim().replace(/^["']|["']$/g, ""); break; }
    }
  }
  const dbHost = dbUrlRaw ? dbHostOf(dbUrlRaw) : null;
  const localDbOk = dbUrlRaw ? isLocalDbUrl(dbUrlRaw) : null;

  const generatedPaths = cfg.GENERATED_PATHS
    ? cfg.GENERATED_PATHS.split(",").map((s) => s.trim()).filter(Boolean)
    : DEFAULT_GENERATED_GLOBS;
  const regenCmd = cfg.REGEN_CMD ?? (pm && regenName ? scriptCmd(pm, regenName) : null);
  const verifyCmd = cfg.VERIFY_CMD ?? (pm && verifyName ? scriptCmd(pm, verifyName) : null);

  // The keys worth freezing: each one is either an exploration the model would otherwise
  // redo, or a detection that could drift. Null values are never written.
  const freezable: Record<string, string | null> = {
    DEFAULT_BRANCH: defaultBranch,
    INSTALL_CMD: cfg.INSTALL_CMD ?? installCmdForLockfile(lockfiles),
    GENERATED_PATHS: generatedPaths.join(","),
    REGEN_CMD: regenCmd,
    VERIFY_CMD: verifyCmd,
    DB_MIGRATE_CMD: cfg.DB_MIGRATE_CMD ?? (pm && migrateName ? scriptCmd(pm, migrateName) : null),
    DB_BACKUP_CMD: cfg.DB_BACKUP_CMD ?? (pm && backupName ? scriptCmd(pm, backupName) : null),
    RESTART_CMD: cfg.RESTART_CMD ?? null,
  };
  // "Complete" = the exploration-heavy keys are written down, so the fast path applies.
  const FAST_PATH_KEYS = ["DEFAULT_BRANCH", "INSTALL_CMD", "GENERATED_PATHS", "REGEN_CMD", "VERIFY_CMD"];
  const missingKeys = FAST_PATH_KEYS.filter((k) => !(k in cfg) && freezable[k] != null);

  if (process.argv.includes("--freeze")) {
    const before = configFound ? readFileSync(configPath, "utf8") : "";
    const today = new Date().toISOString().slice(0, 10);
    const { text, added } = freezeConfig(before, freezable, today);
    if (added.length) {
      mkdirSync(dirname(configPath), { recursive: true });
      writeFileSync(configPath, text);
    }
    process.stderr.write(`freeze: ${added.length ? `wrote ${added.join(", ")} to ${configPath}` : "nothing to add"}\n`);
  }

  const plan = {
    configFound,
    configPath,
    mode: detectMode(currentBranch, defaultBranch),
    currentBranch,
    upstream,
    isWorktree,
    defaultBranch,
    generatedPaths,
    generatedFromConfig: !!cfg.GENERATED_PATHS,
    regenCmd,
    verifyCmd,
    missingKeys,
    configComplete: missingKeys.length === 0,
    mergeStrategy: (cfg.MERGE_STRATEGY ?? "merge").toLowerCase(),
    packageManager: pm,
    installCmd: cfg.INSTALL_CMD ?? installCmdForLockfile(lockfiles),
    lockfiles,
    migrateCmd: cfg.DB_MIGRATE_CMD ?? (pm && migrateName ? scriptCmd(pm, migrateName) : null),
    backupCmd: cfg.DB_BACKUP_CMD ?? (pm && backupName ? scriptCmd(pm, backupName) : null),
    migrationsPaths: (cfg.MIGRATIONS_PATHS ?? "migrations,prisma/migrations,apps/api/migrations,db/migrate")
      .split(",").map((s) => s.trim()).filter(Boolean),
    restartCmd: cfg.RESTART_CMD ?? null,
    hasComposeFile: existsSync(at("docker-compose.yml")) || existsSync(at("docker-compose.yaml")) || existsSync(at("compose.yml")),
    localDbUrlVar,
    dbHost,
    localDbOk,
    runAfterSync: cfg.RUN_AFTER_SYNC ?? null,
  };
  process.stdout.write(JSON.stringify(plan, null, 2) + "\n");
}

if (import.meta.main) {
  main().catch((e) => {
    process.stderr.write(`error: ${(e as Error).message}\n`);
    process.exit(1);
  });
}
