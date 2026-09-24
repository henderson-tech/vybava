import { test } from "node:test";
import assert from "node:assert/strict";
import { parsePrArgs } from "../bin/resolve-fetch.ts";

test("empty args → current branch", () => {
  assert.deepEqual(parsePrArgs([]).selector, { kind: "current" });
});

test("bare number", () => {
  assert.deepEqual(parsePrArgs(["142"]).selector, { kind: "number", pr: 142 });
});

test("hash number", () => {
  assert.deepEqual(parsePrArgs(["#142"]).selector, { kind: "number", pr: 142 });
});

test("number in owner/repo", () => {
  assert.deepEqual(parsePrArgs(["142", "in", "leftEq/fixit"]).selector, {
    kind: "numberInRepo", pr: 142, owner: "leftEq", repo: "fixit",
  });
});

test("full PR url", () => {
  assert.deepEqual(
    parsePrArgs(["https://github.com/leftEq/fixit/pull/142"]).selector,
    { kind: "url", owner: "leftEq", repo: "fixit", pr: 142 },
  );
});

test("latest by author", () => {
  assert.deepEqual(parsePrArgs(["latest", "by", "@alice"]).selector, {
    kind: "latestByAuthor", author: "alice",
  });
});

test("flags are parsed and stripped from selector", () => {
  const r = parsePrArgs(["142", "--once", "--every", "5m", "--include-resolved"]);
  assert.deepEqual(r.selector, { kind: "number", pr: 142 });
  assert.deepEqual(r.flags, {
    once: true, includeResolved: true, noConversation: false, every: "5m",
  });
});

test("unrecognized selector throws", () => {
  assert.throws(() => parsePrArgs(["garble", "garble"]), /Unrecognized PR selector/);
});

import {
  isFilteredBot, bucketFindings,
  bucketNonThread, isBotStatusBody, type NonThreadComment,
} from "../bin/resolve-fetch.ts";

test("generic [bot] suffix is filtered", () => {
  assert.equal(isFilteredBot("dependabot[bot]"), true);
  assert.equal(isFilteredBot("renovate"), true);
  assert.equal(isFilteredBot("olivia"), false);
});

function thread(over: Partial<any> = {}): any {
  return {
    id: "RT_1", isResolved: false, isOutdated: false,
    comments: { nodes: [{
      databaseId: 11, path: "src/a.ts", line: 5, originalLine: 5,
      body: "use a guard here", url: "https://x/11",
      author: { login: "alice" }, replyTo: null,
    }] },
    ...over,
  };
}

test("a plain human inline comment becomes one human finding", () => {
  const r = bucketFindings([thread()], "olivia", { includeResolved: false });
  assert.equal(r.findings.length, 1);
  assert.equal(r.findings[0].file, "src/a.ts:5");
  assert.equal(r.findings[0].by, "@alice");
});

test("PR author's own thread is dropped as self", () => {
  const r = bucketFindings([thread()], "alice", { includeResolved: false });
  assert.equal(r.findings.length, 0);
  assert.equal(r.skipped.self, 1);
});

test("resolved threads dropped unless includeResolved", () => {
  const t = thread({ isResolved: true });
  assert.equal(bucketFindings([t], "olivia", { includeResolved: false }).findings.length, 0);
  assert.equal(bucketFindings([t], "olivia", { includeResolved: true }).findings.length, 1);
});

test("a GitHub-App [bot] thread is skipped as a filtered bot", () => {
  // The eve review bot posts as a User-typename login (no `[bot]` suffix), so it
  // still reaches the round; a plain GitHub App does not.
  const t = thread({ comments: { nodes: [{
    databaseId: 22, path: "src/b.ts", line: 9, originalLine: 9,
    body: "_⚠️ Potential issue_", url: "https://x/22",
    author: { login: "some-app[bot]" }, replyTo: null,
  }] } });
  const r = bucketFindings([t], "olivia", { includeResolved: false });
  assert.equal(r.findings.length, 0);
  assert.equal(r.skipped.bots, 1);
});

import { choosePrLookup } from "../bin/resolve-fetch.ts";

test("current branch → look up the PR by head branch", () => {
  assert.deepEqual(choosePrLookup("feat/offer-race"), { by: "head", branch: "feat/offer-race" });
});

test("detached HEAD (empty branch) → look up by commit SHA, never an empty --head", () => {
  assert.deepEqual(choosePrLookup(""), { by: "sha" });
  assert.deepEqual(choosePrLookup("   "), { by: "sha" });
});

test("branch name is trimmed before use", () => {
  assert.deepEqual(choosePrLookup("  feat/x \n"), { by: "head", branch: "feat/x" });
});

// ── Non-thread surfaces (review summaries + PR conversation) ────────────────
// Added 2026-07-26: these were COUNTED and discarded, so a reviewer asking for
// something in the PR conversation was silently ignored while inline nits got
// fixed. See bucketNonThread.

const review = (o: Partial<NonThreadComment> & { databaseId: number }): NonThreadComment => ({
  body: null, url: `https://x/#r${o.databaseId}`, author: { login: "alice" }, ...o,
});

test("--no-conversation flag parses", () => {
  assert.equal(parsePrArgs(["--no-conversation"]).flags.noConversation, true);
  assert.equal(parsePrArgs([]).flags.noConversation, false);
});

test("review summary with prose becomes a finding, not resolvable", () => {
  const { findings } = bucketNonThread(
    [review({ databaseId: 1, body: "LGTM but please also cover the null case", state: "COMMENTED" })],
    [], "bob",
  );
  assert.equal(findings.length, 1);
  assert.equal(findings[0].surface, "review-summary");
  assert.equal(findings[0].resolvable, false);
  assert.equal(findings[0].threadId, null);
  assert.equal(findings[0].by, "@alice");
});

test("an approval with an empty body is informational, not work", () => {
  const { findings, skipped } = bucketNonThread(
    [review({ databaseId: 2, body: "", state: "APPROVED" }), review({ databaseId: 3, body: null })],
    [], "bob",
  );
  assert.equal(findings.length, 0);
  assert.equal(skipped.informational, 2);
});

test("PR conversation comment becomes a finding", () => {
  const { findings } = bucketNonThread(
    [], [review({ databaseId: 4, body: "Can this also handle the sk-SK locale?" })],
    "bob",
  );
  assert.equal(findings.length, 1);
  assert.equal(findings[0].surface, "conversation");
  assert.equal(findings[0].file, "(PR conversation)");
});

test("the PR author's own comments are skipped as self", () => {
  const { findings, skipped } = bucketNonThread(
    [], [review({ databaseId: 5, body: "note to self", author: { login: "bob" } })],
    "bob",
  );
  assert.equal(findings.length, 0);
  assert.equal(skipped.self, 1);
});

test("review-bot walkthrough/status bodies never become work", () => {
  // Generic status shapes a review bot posts on every push; surfacing them would
  // open each round with fake findings.
  for (const body of [
    "<!-- walkthrough_start -->\nsome html",
    "**Actionable comments posted: 0**",
    "## Walkthrough\nThis change does…",
    "<details><summary>Review details</summary>stuff</details>",
  ]) {
    assert.equal(isBotStatusBody(body), true, `should be status: ${body.slice(0, 30)}`);
  }
  const { findings, skipped } = bucketNonThread(
    [], [review({ databaseId: 6, body: "**Actionable comments posted: 3**", author: { login: "eve-bot-lovinka" } })],
    "bob",
  );
  assert.equal(findings.length, 0);
  assert.equal(skipped.informational, 1);
});

test("a real review-bot ask in the conversation still surfaces", () => {
  const { findings } = bucketNonThread(
    [], [review({ databaseId: 7, body: "Please rebase — main moved.", author: { login: "eve-bot-lovinka" } })],
    "bob",
  );
  assert.equal(findings.length, 1);
  assert.equal(findings[0].by, "@eve-bot-lovinka");
});

test("filtered bots are skipped on the non-thread surfaces too", () => {
  const { findings, skipped } = bucketNonThread(
    [], [review({ databaseId: 9, body: "Deploy preview ready", author: { login: "vercel[bot]" } })],
    "bob",
  );
  assert.equal(findings.length, 0);
  assert.equal(skipped.bots, 1);
});

test("a pathless review thread stays resolvable and is NOT the conversation surface", () => {
  // Regression guard: file-level review threads used to be labelled
  // "conversation", which now names an unresolvable surface. Confusing the two
  // would make the round try to resolve something that has no thread.
  const { findings } = bucketFindings(
    [{
      id: "T1", isResolved: false, isOutdated: false,
      comments: { nodes: [{
        databaseId: 10, path: null, line: null, originalLine: null,
        body: "whole-PR concern", url: "https://x/#d10",
        author: { login: "alice" }, replyTo: null,
      }] },
    }],
    "bob", { includeResolved: false },
  );
  assert.equal(findings[0].surface, "review-thread");
  assert.equal(findings[0].resolvable, true);
  assert.equal(findings[0].threadId, "T1");
});

test("eve-bot status shapes are filtered (found live on FixIt #702)", () => {
  // Without these the noise filter only knew the generic walkthrough shapes, so a
  // normal eve-reviewed PR opened every round with 8 fake findings.
  for (const body of [
    "## 🐉 eve review — 🟡 Review comments\n\ndetails…",
    "### eve review — ✅ APPROVE",
    "🐉 **eve review — ✅ APPROVE · 8 findings**",
    "Delta review for **PR #702** completed and posted.",
    "Delta review for **LEFTEQ/FixIt #702** completed and posted.",
    "PR #702 reviewed and **approved**.",
  ]) {
    assert.equal(isBotStatusBody(body), true, `should be status: ${body.slice(0, 40)}`);
  }
});

test("NEGATIVE CONTROL: a real ask is never filtered as status", () => {
  // The filter must not swallow work. These are the shapes that MUST survive —
  // including a human reply that merely mentions the word "review".
  for (const body of [
    "Can this also handle the sk-SK locale?",
    "Please rebase — main moved.",
    "LGTM but the null case needs a test before I approve.",
    "I reviewed this locally and the migration ordering looks wrong.",
    "One more thing: the eve review flagged something you didn't answer.",
  ]) {
    assert.equal(isBotStatusBody(body), false, `must NOT be status: ${body.slice(0, 40)}`);
  }
  const { findings } = bucketNonThread(
    [], [review({ databaseId: 20, body: "Can this also handle the sk-SK locale?" })],
    "bob",
  );
  assert.equal(findings.length, 1);
});
