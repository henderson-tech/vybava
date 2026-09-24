import { test } from "node:test";
import assert from "node:assert/strict";
import { classifyPath, classifyStatus } from "../bin/classify-paths.ts";

test("env files are secret", () => {
  assert.equal(classifyPath(".env"), "secret");
  assert.equal(classifyPath(".env.local"), "secret");
  assert.equal(classifyPath("apps/api/.env.production"), "secret");
  assert.equal(classifyPath("config/prod.env"), "secret");
});

test("env templates/examples are committable (not secret)", () => {
  assert.equal(classifyPath(".env.example"), "commit");
  assert.equal(classifyPath(".env.sample"), "commit");
  assert.equal(classifyPath("apps/web/.env.template"), "commit");
});

test("keys, certs, credentials are secret", () => {
  for (const p of [
    "server.key", "cert.pem", "keystore.p12", "id_rsa", "deploy/id_ed25519",
    "credentials", "aws-credentials.json", "secrets.json", ".npmrc", "auth.token",
  ]) {
    assert.equal(classifyPath(p), "secret", p);
  }
});

test("public key is committable", () => {
  assert.equal(classifyPath("id_rsa.pub"), "commit");
});

test("source files that merely mention token/secret are committed (no false positive)", () => {
  assert.equal(classifyPath("src/auth/tokenizer.ts"), "commit");
  assert.equal(classifyPath("src/lib/secretManager.service.ts"), "commit");
});

test("build/test artifacts are skipped", () => {
  for (const p of [
    "allure-results/x.json", "allure-report/index.html", "playwright-report/index.html",
    "test-results/run/trace.zip", "coverage/lcov.info", "node_modules/x/index.js",
    ".DS_Store", "logs/app.log", "apps/api/__pycache__/m.pyc",
  ]) {
    assert.equal(classifyPath(p), "artifact", p);
  }
});

test("normal source is commit", () => {
  assert.equal(classifyPath("src/index.ts"), "commit");
  assert.equal(classifyPath("README.md"), "commit");
});

test("classifyStatus partitions a path list, preserving order", () => {
  const r = classifyStatus(["src/a.ts", ".env", "allure-results/r.json", "README.md", "server.key"]);
  assert.deepEqual(r.commit, ["src/a.ts", "README.md"]);
  assert.deepEqual(r.secrets, [".env", "server.key"]);
  assert.deepEqual(r.artifacts, ["allure-results/r.json"]);
});
