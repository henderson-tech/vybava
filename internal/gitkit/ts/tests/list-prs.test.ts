import { test } from "node:test";
import assert from "node:assert/strict";
import { countUnresolved, qualifies, reasonFor, selectPrs } from "../bin/list-prs.ts";

test("countUnresolved counts only unresolved threads", () => {
  assert.equal(countUnresolved([{ isResolved: false }, { isResolved: true }, { isResolved: false }]), 2);
  assert.equal(countUnresolved([]), 0);
});

test("qualifies on CHANGES_REQUESTED or any unresolved thread", () => {
  assert.equal(qualifies("CHANGES_REQUESTED", 0), true);
  assert.equal(qualifies("APPROVED", 1), true);
  assert.equal(qualifies(null, 3), true);
  assert.equal(qualifies("APPROVED", 0), false);
  assert.equal(qualifies(null, 0), false);
});

test("reasonFor describes why a PR qualifies", () => {
  assert.equal(reasonFor("CHANGES_REQUESTED", 0), "CHANGES_REQUESTED");
  assert.equal(reasonFor("APPROVED", 1), "1 unresolved thread");
  assert.equal(reasonFor("CHANGES_REQUESTED", 2), "CHANGES_REQUESTED + 2 unresolved threads");
});

test("selectPrs keeps only viewer-authored, qualifying PRs", () => {
  const nodes = [
    { number: 1, headRefName: "feat-a", reviewDecision: "CHANGES_REQUESTED", author: { login: "me" }, reviewThreads: { nodes: [] } },
    { number: 2, headRefName: "feat-b", reviewDecision: "APPROVED", author: { login: "me" }, reviewThreads: { nodes: [{ isResolved: false }] } },
    { number: 3, headRefName: "feat-c", reviewDecision: "APPROVED", author: { login: "me" }, reviewThreads: { nodes: [{ isResolved: true }] } },
    { number: 4, headRefName: "feat-d", reviewDecision: "CHANGES_REQUESTED", author: { login: "someone-else" }, reviewThreads: { nodes: [] } },
  ];
  assert.deepEqual(selectPrs(nodes, "me"), [
    { pr: 1, headRef: "feat-a", reason: "CHANGES_REQUESTED" },
    { pr: 2, headRef: "feat-b", reason: "1 unresolved thread" },
  ]);
});
