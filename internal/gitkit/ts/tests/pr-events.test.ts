import { test } from "node:test";
import assert from "node:assert/strict";
import { computeEvents, parseRepoFlag } from "../bin/pr-events.ts";

test("first poll (no prev) on an OPEN PR is a silent baseline — no events", () => {
  assert.deepEqual(
    computeEvents(null, { state: "OPEN", ci: "PENDING", commentIds: [1, 2] }),
    { events: [], done: false },
  );
});

test("first poll on an already-merged PR emits 'merged' and is done", () => {
  assert.deepEqual(
    computeEvents(null, { state: "MERGED", ci: "SUCCESS", commentIds: [] }),
    { events: ["merged"], done: true },
  );
});

test("new comment ids since the last poll each emit a 'comment <id>' event", () => {
  assert.deepEqual(
    computeEvents(
      { state: "OPEN", ci: "PENDING", commentIds: [1] },
      { state: "OPEN", ci: "PENDING", commentIds: [1, 2, 3] },
    ),
    { events: ["comment 2", "comment 3"], done: false },
  );
});

test("a CI state flip emits one 'ci <prev>-><curr>' event", () => {
  assert.deepEqual(
    computeEvents(
      { state: "OPEN", ci: "PENDING", commentIds: [1] },
      { state: "OPEN", ci: "SUCCESS", commentIds: [1] },
    ),
    { events: ["ci PENDING->SUCCESS"], done: false },
  );
});

test("merge while watching emits 'merged' and is done", () => {
  assert.deepEqual(
    computeEvents(
      { state: "OPEN", ci: "SUCCESS", commentIds: [1] },
      { state: "MERGED", ci: "SUCCESS", commentIds: [1] },
    ),
    { events: ["merged"], done: true },
  );
});

test("close while watching emits 'closed' and is done", () => {
  assert.deepEqual(
    computeEvents(
      { state: "OPEN", ci: "PENDING", commentIds: [] },
      { state: "CLOSED", ci: "PENDING", commentIds: [] },
    ),
    { events: ["closed"], done: true },
  );
});

test("no change between polls emits nothing", () => {
  const snap = { state: "OPEN", ci: "SUCCESS", commentIds: [1, 2] };
  assert.deepEqual(computeEvents(snap, { ...snap, commentIds: [...snap.commentIds] }), { events: [], done: false });
});

test("a new comment AND a CI flip in the same poll emit both, comments first", () => {
  assert.deepEqual(
    computeEvents(
      { state: "OPEN", ci: "PENDING", commentIds: [1] },
      { state: "OPEN", ci: "FAILURE", commentIds: [1, 9] },
    ),
    { events: ["comment 9", "ci PENDING->FAILURE"], done: false },
  );
});

test("a review-state change emits 'review <STATE> by <login>' (approval with no comments)", () => {
  assert.deepEqual(
    computeEvents(
      { state: "OPEN", ci: "SUCCESS", commentIds: [], reviews: {} },
      { state: "OPEN", ci: "SUCCESS", commentIds: [], reviews: { "eve-bot-lovinka": "APPROVED" } },
    ),
    { events: ["review APPROVED by eve-bot-lovinka"], done: false },
  );
});

test("an unchanged review state stays silent; a flip re-fires", () => {
  assert.deepEqual(
    computeEvents(
      { state: "OPEN", ci: "SUCCESS", commentIds: [], reviews: { a: "CHANGES_REQUESTED" } },
      { state: "OPEN", ci: "SUCCESS", commentIds: [], reviews: { a: "CHANGES_REQUESTED" } },
    ),
    { events: [], done: false },
  );
  assert.deepEqual(
    computeEvents(
      { state: "OPEN", ci: "SUCCESS", commentIds: [], reviews: { a: "CHANGES_REQUESTED" } },
      { state: "OPEN", ci: "SUCCESS", commentIds: [], reviews: { a: "APPROVED" } },
    ),
    { events: ["review APPROVED by a"], done: false },
  );
});

test("snapshots without a reviews field stay backward compatible (no events)", () => {
  assert.deepEqual(
    computeEvents(
      { state: "OPEN", ci: "SUCCESS", commentIds: [] },
      { state: "OPEN", ci: "SUCCESS", commentIds: [] },
    ),
    { events: [], done: false },
  );
});

test("a head move emits 'push <sha7>' (teammate push / force-push)", () => {
  assert.deepEqual(
    computeEvents(
      { state: "OPEN", ci: "SUCCESS", commentIds: [], headSha: "aaaaaaa1111" },
      { state: "OPEN", ci: "SUCCESS", commentIds: [], headSha: "bbbbbbb2222" },
    ),
    { events: ["push bbbbbbb"], done: false },
  );
});

test("mergeable flips fire; snapshots without the field stay silent", () => {
  assert.deepEqual(
    computeEvents(
      { state: "OPEN", ci: "SUCCESS", commentIds: [], mergeable: "MERGEABLE" },
      { state: "OPEN", ci: "SUCCESS", commentIds: [], mergeable: "CONFLICTING" },
    ),
    { events: ["mergeable MERGEABLE->CONFLICTING"], done: false },
  );
  assert.deepEqual(
    computeEvents(
      { state: "OPEN", ci: "SUCCESS", commentIds: [] },
      { state: "OPEN", ci: "SUCCESS", commentIds: [], mergeable: "CONFLICTING" },
    ),
    { events: [], done: false },
  );
});

test("draft flips emit 'draft' / 'ready'", () => {
  assert.deepEqual(
    computeEvents(
      { state: "OPEN", ci: "SUCCESS", commentIds: [], isDraft: false },
      { state: "OPEN", ci: "SUCCESS", commentIds: [], isDraft: true },
    ),
    { events: ["draft"], done: false },
  );
  assert.deepEqual(
    computeEvents(
      { state: "OPEN", ci: "SUCCESS", commentIds: [], isDraft: true },
      { state: "OPEN", ci: "SUCCESS", commentIds: [], isDraft: false },
    ),
    { events: ["ready"], done: false },
  );
});

// --repo accepts both forms (the owner/name-only validation crashed the prm
// Monitor when the skill-documented path form was passed, then the owner/name
// form silently starved every poll via repoRoot's directory check).
test("parseRepoFlag: absolute path form defers to repoRoot anchoring", () => {
  assert.deepEqual(parseRepoFlag("/Users/x/Work/repo"), { nameWithOwner: "", stripFromArgv: false });
  assert.deepEqual(parseRepoFlag("./repo"), { nameWithOwner: "", stripFromArgv: false });
  assert.deepEqual(parseRepoFlag(""), { nameWithOwner: "", stripFromArgv: false });
});

test("parseRepoFlag: owner/name form pins the target and must be stripped from argv", () => {
  assert.deepEqual(parseRepoFlag("LEFTEQ/FixIt"), { nameWithOwner: "LEFTEQ/FixIt", stripFromArgv: true });
});

test("parseRepoFlag: anything else throws loudly at startup", () => {
  assert.throws(() => parseRepoFlag("a/b/c"), /owner\/name or an absolute repo path/);
  assert.throws(() => parseRepoFlag("just-a-name"), /owner\/name or an absolute repo path/);
});
