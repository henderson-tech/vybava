import { test } from "node:test";
import assert from "node:assert/strict";
import { buildCommand, parseFlags } from "../bin/github-io.ts";

test("find-run lists runs for a commit", () => {
  assert.deepEqual(buildCommand("find-run", { sha: "abc123" }), [
    "run", "list", "--commit", "abc123",
    "--json", "databaseId,status,conclusion,workflowName,headSha", "--limit", "20",
  ]);
});

test("watch-run watches with exit status", () => {
  assert.deepEqual(buildCommand("watch-run", { runId: "99" }), ["run", "watch", "99", "--exit-status"]);
});

test("failed-logs pulls failed job logs", () => {
  assert.deepEqual(buildCommand("failed-logs", { runId: "99" }), ["run", "view", "99", "--log-failed"]);
});

test("rerun-failed reruns only failed jobs", () => {
  assert.deepEqual(buildCommand("rerun-failed", { runId: "99" }), ["run", "rerun", "99", "--failed"]);
});

test("reply posts to the review-comment replies endpoint", () => {
  assert.deepEqual(buildCommand("reply", { owner: "o", repo: "r", pr: "5", commentId: "11", body: "Fixed in abc." }), [
    "api", "--method", "POST", "repos/o/r/pulls/5/comments/11/replies", "-f", "body=Fixed in abc.",
  ]);
});

test("resolve-thread issues the GraphQL mutation with the thread id", () => {
  const argv = buildCommand("resolve-thread", { threadId: "RT_1" });
  assert.equal(argv[0], "api");
  assert.equal(argv[1], "graphql");
  assert.ok(argv.some((a) => a === "-f"));
  assert.ok(argv.some((a) => a === "threadId=RT_1"));
  assert.ok(argv.join(" ").includes("resolveReviewThread"));
});

test("create-pr opens a ready-for-review PR with --fill (no --draft)", () => {
  const argv = buildCommand("create-pr", { head: "feat/x", base: "main" });
  assert.deepEqual(argv, ["pr", "create", "--head", "feat/x", "--base", "main", "--fill"]);
  assert.ok(!argv.includes("--draft"));
});

test("create-pr opens a draft PR when --draft is passed explicitly", () => {
  const argv = buildCommand("create-pr", { head: "feat/x", base: "main", draft: "" });
  assert.deepEqual(argv, ["pr", "create", "--head", "feat/x", "--base", "main", "--fill", "--draft"]);
});

test("create-pr forwards an explicit title and body (they win over --fill)", () => {
  const argv = buildCommand("create-pr", { head: "feat/x", base: "main", title: "Fix the thing", body: "Why it broke." });
  assert.deepEqual(argv, [
    "pr", "create", "--head", "feat/x", "--base", "main", "--fill",
    "--title", "Fix the thing", "--body", "Why it broke.",
  ]);
});

test("create-pr keeps --fill as the fallback when only one of title/body is given", () => {
  const argv = buildCommand("create-pr", { head: "feat/x", base: "main", title: "Only a title" });
  assert.deepEqual(argv, ["pr", "create", "--head", "feat/x", "--base", "main", "--fill", "--title", "Only a title"]);
  assert.ok(!argv.includes("--body"));
});

test("create-pr ignores empty title/body rather than emitting a bare flag", () => {
  const argv = buildCommand("create-pr", { head: "feat/x", base: "main", title: "", body: "" });
  assert.deepEqual(argv, ["pr", "create", "--head", "feat/x", "--base", "main", "--fill"]);
});

test("review requests changes on a PR", () => {
  assert.deepEqual(
    buildCommand("review", { owner: "o", repo: "r", pr: "5", event: "request-changes", body: "Breaks callers." }),
    ["pr", "review", "5", "--request-changes", "--body", "Breaks callers.", "--repo", "o/r"],
  );
});

test("review can post a plain commenting review", () => {
  assert.deepEqual(
    buildCommand("review", { owner: "o", repo: "r", pr: "5", event: "comment", body: "Notes." }),
    ["pr", "review", "5", "--comment", "--body", "Notes.", "--repo", "o/r"],
  );
});

test("review REFUSES to approve — never self-approve is enforced in the builder", () => {
  assert.throws(
    () => buildCommand("review", { owner: "o", repo: "r", pr: "5", event: "approve", body: "lgtm" }),
    /never approve/,
  );
});

test("parseFlags treats a valueless flag as boolean, whatever its position", () => {
  // --draft used to swallow the next flag as its value, so callers were told to
  // pass it last. Both orders must now parse identically.
  assert.deepEqual(parseFlags(["--draft", "--title", "T"]), { draft: "", title: "T" });
  assert.deepEqual(parseFlags(["--title", "T", "--draft"]), { title: "T", draft: "" });
});

test("parseFlags names zsh word-splitting when flags arrive glued into one token", () => {
  // zsh does not split unquoted $VAR, so `reply $O` sends one argument. The old
  // behaviour was `missing required field: owner` with --owner plainly present.
  assert.throws(
    () => parseFlags(["--owner FixIt-Technologies --repo FixIt --pr 1066", "--commentId", "11"]),
    /ONE argument with embedded spaces[\s\S]*does NOT word-split/,
  );
});

test("missing required field throws", () => {
  assert.throws(() => buildCommand("watch-run", {}), /missing required field: runId/);
});

test("unknown subcommand throws", () => {
  assert.throws(() => buildCommand("nope", {}), /Unknown github-io subcommand/);
});
