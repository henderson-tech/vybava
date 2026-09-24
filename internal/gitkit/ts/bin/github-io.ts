// github-io.ts — mechanical GitHub I/O the skills call. Zero npm deps.
// Run: node github-io.ts <subcommand> --key value ...
// Subcommands: reply, resolve-thread, comment, react, review, create-pr,
//              detect-workflows, find-run, watch-run, failed-logs, rerun-failed

import { execFileSync } from "node:child_process";
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";

import { repoRoot } from "./repo-root.ts";

const RESOLVE_THREAD_MUTATION =
  "mutation($threadId:ID!){ resolveReviewThread(input:{threadId:$threadId}){ thread { isResolved } } }";

// Running the script bare is how a caller discovers its interface. Until
// 2026-08-08 that answered `Unknown github-io subcommand: undefined`, which
// teaches nothing — hence this synopsis.
const USAGE = `github-io — mechanical GitHub I/O for the git skills.

Usage: node github-io.ts <subcommand> --key value ...

  reply             --owner O --repo R --pr N --commentId ID --body TEXT
  comment           --owner O --repo R --pr N --body TEXT
  react             --owner O --repo R --commentId ID [--content +1]
  review            --owner O --repo R --pr N --event request-changes|comment --body TEXT
  resolve-thread    --threadId PRRT_...
  create-pr         --head BRANCH --base BRANCH [--title T] [--body B] [--draft] [--label L]
  find-run          --sha SHA
  watch-run         --runId ID
  failed-logs       --runId ID
  rerun-failed      --runId ID
  detect-workflows  (no flags — scans ./.github/workflows)

Flags are camelCase (--commentId, --threadId, --runId); owner/repo are never
inferred from the cwd. \`review\` cannot approve, by design.

The harness shell is zsh, which does NOT word-split unquoted \$VAR — packing
flags into one variable sends them as a SINGLE argument. Use an array:
  FLAGS=(--owner o --repo r --pr 1); node github-io.ts reply "\${FLAGS[@]}" --commentId 9 --body X
`;

function req(o: Record<string, string>, key: string): string {
  const v = o[key];
  if (v == null || v === "") throw new Error(`missing required field: ${key}`);
  return v;
}

export function buildCommand(sub: string, o: Record<string, string>): string[] {
  switch (sub) {
    case "find-run":
      return ["run", "list", "--commit", req(o, "sha"),
        "--json", "databaseId,status,conclusion,workflowName,headSha", "--limit", "20"];
    case "watch-run":
      return ["run", "watch", req(o, "runId"), "--exit-status"];
    case "failed-logs":
      return ["run", "view", req(o, "runId"), "--log-failed"];
    case "rerun-failed":
      return ["run", "rerun", req(o, "runId"), "--failed"];
    case "reply":
      return ["api", "--method", "POST",
        `repos/${req(o, "owner")}/${req(o, "repo")}/pulls/${req(o, "pr")}/comments/${req(o, "commentId")}/replies`,
        "-f", `body=${req(o, "body")}`];
    case "resolve-thread":
      return ["api", "graphql", "-f", `query=${RESOLVE_THREAD_MUTATION}`, "-f", `threadId=${req(o, "threadId")}`];
    // ── Closure for the NON-THREAD surfaces ────────────────────────────────
    // A review's summary body and a PR conversation comment have no review
    // thread, so `resolve-thread` cannot touch them and `reply` (which posts into
    // a review thread) does not apply either. GitHub gives them exactly two
    // affordances, and together they are what "handled" means for those surfaces:
    case "comment":
      // A PR-level comment. Quote what you are answering — this lands at the
      // bottom of the conversation, not under the comment it addresses.
      return ["api", "--method", "POST",
        `repos/${req(o, "owner")}/${req(o, "repo")}/issues/${req(o, "pr")}/comments`,
        "-f", `body=${req(o, "body")}`];
    case "react":
      // An idempotent, non-noisy "seen/actioned" marker on a conversation
      // comment. GitHub exposes reactions for ISSUE comments only — there is no
      // reactions endpoint for a review's summary body, so a review summary is
      // closed by the quoting `comment` alone.
      return ["api", "--method", "POST",
        `repos/${req(o, "owner")}/${req(o, "repo")}/issues/comments/${req(o, "commentId")}/reactions`,
        "-f", `content=${o.content || "+1"}`];
    case "review": {
      // A verdict-carrying PR review — what `comment` structurally cannot be. Needed by
      // the `--auto` regression audit (`auto-audit.md`): a plain conversation
      // comment is invisible to an autonomous PR author, whereas CHANGES_REQUESTED is the
      // one signal eve peacemaker's passive review loop (D6) matches to its claim and
      // answers with a revision. Blocking a machine-authored PR with a comment alone
      // means nothing happens; blocking it with a review makes the author fix it.
      //
      // APPROVE IS DELIBERATELY UNREACHABLE. "Never self-approve" is a hard rule in
      // merge/SKILL.md, and `--auto` removes the human who used to be the one enforcing
      // it. Encoding the ban in the argv builder means no caller can reach it by
      // forgetting — the affordance does not exist, rather than existing and being
      // discouraged.
      const event = req(o, "event");
      if (event !== "request-changes" && event !== "comment") {
        throw new Error(`review event must be request-changes|comment (never approve): got ${event}`);
      }
      return ["pr", "review", req(o, "pr"), `--${event}`, "--body", req(o, "body"),
        "--repo", `${req(o, "owner")}/${req(o, "repo")}`];
    }
    case "create-pr":
      // Ready-for-review by default (no --draft) per the global PR-defaults rule;
      // --draft is opt-in only (callers pass it explicitly to override the default).
      //
      // --fill stays as the FALLBACK (title/body from commits). gh lets --title
      // and --body sit alongside it and gives them precedence, so passing both is
      // correct: an explicit title wins, and anything not supplied is still
      // autofilled. Forwarding these was missing until 2026-07-26 — buildCommand
      // rebuilt the argv from scratch, so a caller's --title/--body never reached
      // gh at all and the PR silently got a commit-derived body.
      return ["pr", "create", "--head", req(o, "head"), "--base", req(o, "base"), "--fill",
        ...(o.title ? ["--title", o.title] : []),
        ...(o.body ? ["--body", o.body] : []),
        ...("draft" in o ? ["--draft"] : []),
        // MERGE_POLICY=self repos: `eve-ignore` keeps eve off a PR nobody will wait on.
        ...(o.label ? ["--label", o.label] : [])];
    default:
      throw new Error(`Unknown github-io subcommand: ${sub}`);
  }
}

// detect-workflows is filesystem-based, not gh-backed.
export function detectWorkflows(cwd: string): { name: string; path: string }[] {
  const dir = join(cwd, ".github", "workflows");
  let files: string[] = [];
  try {
    files = readdirSync(dir).filter((f) => f.endsWith(".yml") || f.endsWith(".yaml"));
  } catch (e) {
    if ((e as NodeJS.ErrnoException).code === "ENOENT") return []; // no workflows dir → empty, not an error
    throw e;
  }
  return files.map((f) => {
    const text = readFileSync(join(dir, f), "utf8");
    const m = text.match(/^name:\s*(.+)$/m);
    return { name: m ? m[1].trim().replace(/^['"]|['"]$/g, "") : f, path: `.github/workflows/${f}` };
  });
}

export function parseFlags(argv: string[]): Record<string, string> {
  const o: Record<string, string> = {};
  for (let i = 0; i < argv.length; i++) {
    if (!argv[i].startsWith("--")) continue;
    const key = argv[i].slice(2);
    // A key with whitespace means several flags arrived glued into ONE argv
    // token. Overwhelmingly that is zsh (the harness shell) not word-splitting
    // an unquoted $VAR: `O="--owner o --repo r"; node github-io.ts reply $O`
    // passes one argument, every real flag is lost, and the old failure mode was
    // a baffling `missing required field: owner` when owner was right there
    // (2026-08-08, PR #1066 reply round). Name the real cause instead.
    if (/\s/.test(key)) {
      throw new Error(
        `flag arrived as ONE argument with embedded spaces: "--${key}"\n` +
        `  The harness shell is zsh, which does NOT word-split unquoted $VAR.\n` +
        `  Pass the flags literally, or use an array:\n` +
        `    FLAGS=(--owner o --repo r --pr 1); node github-io.ts <sub> "\${FLAGS[@]}"`,
      );
    }
    const next = argv[i + 1];
    // A valueless flag (next token is another flag, or there is none) records "".
    // Without this, `--draft --title X` would swallow "--title" as draft's value —
    // which is why callers used to be told to pass --draft last. Now order is free.
    o[key] = next != null && !next.startsWith("--") ? argv[++i] : "";
  }
  return o;
}

async function main(): Promise<void> {
  const [sub, ...rest] = process.argv.slice(2);
  // Explicit help → stdout, exit 0. A bare invocation still exits 1, so a script
  // whose `$SUB` expanded to nothing fails loudly instead of looking successful.
  if (sub === "help" || sub === "--help" || sub === "-h") {
    process.stdout.write(USAGE);
    return;
  }
  if (sub === undefined) {
    process.stderr.write(USAGE);
    process.exit(1);
  }
  if (sub === "detect-workflows") {
    process.stdout.write(JSON.stringify(detectWorkflows(process.cwd()), null, 2) + "\n");
    return;
  }
  const argv = buildCommand(sub, parseFlags(rest));
  // repoRoot([]) — NEVER let it sniff process.argv: github-io's own `--repo`
  // is a GitHub repo NAME (owner/repo split across flags), not a directory
  // anchor, and the default argv scan misreads it and throws. GIT_SKILL_REPO
  // env anchoring still applies; otherwise the ambient cwd's toplevel.
  const out = execFileSync("gh", argv, { encoding: "utf8", stdio: ["ignore", "pipe", "inherit"], maxBuffer: 64 * 1024 * 1024, timeout: 60_000, cwd: repoRoot([]) });
  process.stdout.write(out);
}

if (import.meta.main) {
  main().catch((e) => {
    process.stderr.write(`error: ${(e as Error).message}\n`);
    process.exit(1);
  });
}
