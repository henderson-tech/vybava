import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, mkdirSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  parseConfig, packageManager, installCmdForLockfile, isLocalDbUrl, dbHostOf,
  detectMode, globToRegExp, isGeneratedPath, freezeConfig, DEFAULT_GENERATED_GLOBS, readGitConfig,
} from "../bin/sync-context.ts";

test("parseConfig reads KEY=value, skips comments/blanks, strips quotes", () => {
  const cfg = parseConfig([
    "# comment",
    "",
    "DEFAULT_BRANCH=main",
    "DB_MIGRATE_CMD = pnpm db:migrate ",
    `RESTART_CMD="docker compose restart api"`,
    "MERGE_STRATEGY=rebase",
    "junk-line-without-eq",
  ].join("\n"));
  assert.equal(cfg.DEFAULT_BRANCH, "main");
  assert.equal(cfg.DB_MIGRATE_CMD, "pnpm db:migrate");
  assert.equal(cfg.RESTART_CMD, "docker compose restart api");
  assert.equal(cfg.MERGE_STRATEGY, "rebase");
  assert.equal("junk-line-without-eq" in cfg, false);
});

test("packageManager + installCmd resolve from the lockfile present", () => {
  assert.equal(packageManager(["pnpm-lock.yaml", "package.json"]), "pnpm");
  assert.equal(packageManager(["bun.lock"]), "bun");
  assert.equal(packageManager(["yarn.lock"]), "yarn");
  assert.equal(packageManager(["package-lock.json"]), "npm");
  assert.equal(packageManager(["README.md"]), null);
  assert.equal(installCmdForLockfile(["pnpm-lock.yaml"]), "pnpm install");
  assert.equal(installCmdForLockfile(["package-lock.json"]), "npm install");
  assert.equal(installCmdForLockfile([]), null);
});

test("isLocalDbUrl accepts local hosts, rejects remote/prod", () => {
  assert.equal(isLocalDbUrl("postgres://u:p@localhost:5432/app"), true);
  assert.equal(isLocalDbUrl("postgres://u:p@127.0.0.1:5432/app"), true);
  assert.equal(isLocalDbUrl("postgres://u:p@db:5432/app"), true); // docker-compose service
  assert.equal(isLocalDbUrl("postgres://u:p@host.docker.internal:5432/app"), true);
  assert.equal(isLocalDbUrl("postgres://u:p@db.prod.example.com:5432/app"), false);
  assert.equal(isLocalDbUrl("postgres://u:p@203.0.113.10:5432/app"), false);
  // A credential-shaped @localhost: before the real host must not read as local.
  assert.equal(isLocalDbUrl("postgres://user@localhost:5432@prod.example.com/app"), false);
  assert.equal(isLocalDbUrl("postgres://u:p@[::1]:5432/app"), true);
  assert.equal(isLocalDbUrl(""), false);
});

test("dbHostOf extracts the host without leaking credentials", () => {
  assert.equal(dbHostOf("postgres://user:secret@db.prod.example.com:5432/app"), "db.prod.example.com");
  assert.equal(dbHostOf("postgres://u:p@localhost:5432/app"), "localhost");
  assert.equal(dbHostOf("not-a-url"), null);
});

test("detectMode keys on branch identity, not location", () => {
  assert.equal(detectMode("main", "main"), "main");
  assert.equal(detectMode("master", "master"), "main");
  // A feature branch checked out in the PRIMARY clone is still branch-mode.
  assert.equal(detectMode("work/tz-fix", "main"), "branch");
  // Detached HEAD never masquerades as the default branch.
  assert.equal(detectMode("HEAD", "main"), "branch");
});

test("globToRegExp: * stops at /, ** crosses it, {a,b} alternates", () => {
  assert.equal(globToRegExp("*.ts").test("a.ts"), true);
  assert.equal(globToRegExp("*.ts").test("src/a.ts"), false);
  assert.equal(globToRegExp("**/*.ts").test("src/deep/a.ts"), true);
  assert.equal(globToRegExp("**/generated/**").test("apps/web/generated/api.ts"), true);
  assert.equal(globToRegExp("openapi*.{json,yaml}").test("openapi.json"), true);
  assert.equal(globToRegExp("openapi*.{json,yaml}").test("openapi-v2.yaml"), true);
  assert.equal(globToRegExp("openapi*.{json,yaml}").test("openapi.ts"), false);
  // A literal dot must not act as the regex wildcard.
  assert.equal(globToRegExp("a.ts").test("axts"), false);
});

test("isGeneratedPath: config prefixes, default globs, and hand-written near-misses", () => {
  const cfg = ["packages/api-client/src/generated", "apps/web/src/api.gen.ts"];
  assert.equal(isGeneratedPath("packages/api-client/src/generated/hooks.ts", cfg), true);
  assert.equal(isGeneratedPath("packages/api-client/src/generated", cfg), true);
  assert.equal(isGeneratedPath("apps/web/src/api.gen.ts", cfg), true);
  // Prefix match must respect segment boundaries — not a bare startsWith.
  assert.equal(isGeneratedPath("packages/api-client/src/generated-by-hand.ts", cfg), false);
  assert.equal(isGeneratedPath("packages/api-client/src/client.ts", cfg), false);

  const d = DEFAULT_GENERATED_GLOBS;
  assert.equal(isGeneratedPath("apps/web/generated/api.ts", d), true);
  assert.equal(isGeneratedPath("src/__generated__/gql.ts", d), true);
  assert.equal(isGeneratedPath("src/deep/client.gen.ts", d), true);
  assert.equal(isGeneratedPath("src/types.generated.d.ts", d), true);
  assert.equal(isGeneratedPath("openapi.json", d), true);
  assert.equal(isGeneratedPath("docs/openapi-v1.yaml", d), true);
  assert.equal(isGeneratedPath("prisma/client/index.d.ts", d), true);
  // Hand-written code that merely mentions the words stays hand-merged.
  assert.equal(isGeneratedPath("src/generateReport.ts", d), false);
  assert.equal(isGeneratedPath("src/pricing.ts", d), false);
  assert.equal(isGeneratedPath("src/generator/rules.ts", d), false);
});

test("freezeConfig appends only unset keys, never overwrites, and is idempotent", () => {
  const existing = "# hand written\nDEFAULT_BRANCH=develop\n";
  const resolved = {
    DEFAULT_BRANCH: "main",          // already set → user's value wins, not written
    INSTALL_CMD: "bun install",
    REGEN_CMD: null,                  // unresolved → never written
    VERIFY_CMD: "",                   // empty → never written
    GENERATED_PATHS: "**/generated/**",
  };
  const first = freezeConfig(existing, resolved, "2026-07-29");
  assert.deepEqual(first.added, ["INSTALL_CMD", "GENERATED_PATHS"]);
  assert.match(first.text, /^DEFAULT_BRANCH=develop$/m);
  assert.match(first.text, /^INSTALL_CMD=bun install$/m);
  assert.match(first.text, /^GENERATED_PATHS=\*\*\/generated\/\*\*$/m);
  assert.equal(/REGEN_CMD|VERIFY_CMD/.test(first.text), false);
  assert.match(first.text, /# auto-detected by \/sync 2026-07-29/);

  // Re-freezing the frozen file adds nothing and leaves the text byte-identical.
  const second = freezeConfig(first.text, resolved, "2026-07-30");
  assert.deepEqual(second.added, []);
  assert.equal(second.text, first.text);
});

test("freezeConfig on an empty config writes every resolved key", () => {
  const { text, added } = freezeConfig("", { INSTALL_CMD: "pnpm install", RESTART_CMD: null }, "2026-07-29");
  assert.deepEqual(added, ["INSTALL_CMD"]);
  assert.match(text, /^INSTALL_CMD=pnpm install$/m);
  // Round-trips through the parser it will be read back with.
  assert.equal(parseConfig(text).INSTALL_CMD, "pnpm install");
});

test("readGitConfig: .claude.git.config.local overlays the committed file, key by key", () => {
  const root = mkdtempSync(join(tmpdir(), "gitcfg-"));
  mkdirSync(join(root, ".claude"));
  writeFileSync(join(root, ".claude/.claude.git.config"), "DEFAULT_BRANCH=main\nMERGE_METHOD=squash\n");
  writeFileSync(join(root, ".claude/.claude.git.config.local"), "DEFAULT_BRANCH=devlp\n");
  assert.deepEqual(readGitConfig(root), { cfg: { DEFAULT_BRANCH: "devlp", MERGE_METHOD: "squash" }, found: true });
  assert.deepEqual(readGitConfig(join(root, "nope")), { cfg: {}, found: false });
});
