import { test } from "node:test";
import assert from "node:assert/strict";
import { skipCategory } from "../bin/tdd-classify.ts";

test("DB migrations are skip:migration", () => {
  assert.equal(skipCategory("apps/api/migrations/1718900000_add_col.ts"), "migration");
  assert.equal(skipCategory("prisma/migrations/20240101_init/migration.sql"), "migration");
  assert.equal(skipCategory("packages/db/src/migration/0007-foo.ts"), "migration");
});

test("lockfiles are skip:deps (but package.json is not)", () => {
  assert.equal(skipCategory("pnpm-lock.yaml"), "deps");
  assert.equal(skipCategory("apps/web/package-lock.json"), "deps");
  assert.equal(skipCategory("bun.lock"), "deps");
  assert.equal(skipCategory("go.sum"), "deps");
  assert.equal(skipCategory("package.json"), null); // a version bump here still gets judgment
});

test("CI/CD config is skip:ci (and wins over generic yaml)", () => {
  assert.equal(skipCategory(".github/workflows/ci.yml"), "ci");
  assert.equal(skipCategory(".gitlab-ci.yml"), "ci");
  assert.equal(skipCategory(".circleci/config.yml"), "ci");
});

test("infra-as-code is skip:iac", () => {
  assert.equal(skipCategory("Dockerfile"), "iac");
  assert.equal(skipCategory("apps/api/Dockerfile.prod"), "iac");
  assert.equal(skipCategory("docker-compose.yml"), "iac");
  assert.equal(skipCategory("infra/main.tf"), "iac");
  assert.equal(skipCategory("nginx/site.conf"), "iac");
  assert.equal(skipCategory(".env.production"), "iac");
});

test("generated / build output is skip:generated", () => {
  assert.equal(skipCategory("src/api/types.generated.ts"), "generated");
  assert.equal(skipCategory("dist/index.js"), "generated");
  assert.equal(skipCategory("apps/web/src/orval.d.ts"), "generated");
  assert.equal(skipCategory("src/__generated__/schema.ts"), "generated");
});

test("docs/text files are skip:docs", () => {
  assert.equal(skipCategory("README.md"), "docs");
  assert.equal(skipCategory("docs/guide.mdx"), "docs");
  assert.equal(skipCategory("CHANGELOG.txt"), "docs");
});

test("real source code returns null (→ apply the behavioral gates)", () => {
  assert.equal(skipCategory("apps/api/src/offer.service.ts"), null);
  assert.equal(skipCategory("src/utils/money.ts"), null);
  assert.equal(skipCategory("packages/core/lib/rateLimit.ts"), null);
});
