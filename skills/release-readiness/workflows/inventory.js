export const meta = {
  name: 'release-readiness-inventory',
  description: 'Inventory a release range per journey cluster, then attribute every commit with a completeness critic',
  whenToUse: 'Phase 1 of the release-readiness skill. args = the run directory\'s inventory-args.json (readiness init writes it; the orchestrator fills clusters).',
  phases: [
    { title: 'Inventory', detail: 'one read-only reader per journey cluster' },
    { title: 'Critic', detail: 'attribute every commit of every range' },
  ],
}

// args: {project, devices: [id], repos: [{repo, path, github, range, commits}],
//        clusters: [{name, focus}], outputDir}
// Returns {inventories: [cluster], critic} — the shape of inventory.json. The
// orchestrator writes it with `jq '.result' <workflow output file>`, never by
// reading the result into its own context.
if (!args || !Array.isArray(args.repos) || args.repos.length === 0) {
  throw new Error('args.repos is empty: pass inventory-args.json from `readiness init`')
}
if (!Array.isArray(args.clusters) || args.clusters.length === 0) {
  throw new Error('args.clusters is empty: fill clusters [{name, focus}] in inventory-args.json first')
}
if (args.clusters.length > 4) {
  log(`${args.clusters.length} clusters: more than the 4-per-phase guideline; only run it this wide when the human asked for scale`)
}

const STR = { type: 'string' }
const STRS = { type: 'array', items: STR }
const FEATURE = {
  type: 'object',
  properties: {
    name: STR, summary: STR, refs: STRS, surfaces: STRS, roles: STRS, devices: STRS,
    existingTests: STRS, gaps: STRS, risk: { type: 'string', enum: ['low', 'medium', 'high'] }, repos: STRS,
  },
  required: ['name', 'summary', 'refs', 'surfaces', 'roles', 'devices', 'existingTests', 'gaps', 'risk', 'repos'],
}
const JOURNEY = {
  type: 'object',
  properties: { name: STR, steps: STR, roles: STRS, devices: STRS, dataNeeds: STR },
  required: ['name', 'steps', 'roles', 'devices', 'dataNeeds'],
}
const CLUSTER = {
  type: 'object',
  properties: {
    lane: STR,
    features: { type: 'array', items: FEATURE },
    journeys: { type: 'array', items: JOURNEY },
    attributedRefs: STRS,
    excludedTooling: STRS,
    openQuestions: STRS,
  },
  required: ['lane', 'features', 'journeys', 'attributedRefs', 'excludedTooling', 'openQuestions'],
}
const CRITIC = {
  type: 'object',
  properties: {
    notes: STR,
    overlaps: {
      type: 'array',
      items: { type: 'object', properties: { ref: STR, lanes: STRS, owner: STR }, required: ['ref', 'lanes', 'owner'] },
    },
    toolingCount: { type: 'integer' },
    unassigned: {
      type: 'array',
      items: {
        type: 'object',
        properties: { ref: STR, repo: STR, subject: STR, lane: STR },
        required: ['ref', 'repo', 'subject', 'lane'],
      },
    },
  },
  required: ['notes', 'overlaps', 'toolingCount', 'unassigned'],
}

const RULES = `READ-ONLY: never edit, commit, push, start servers, simulators or test runs. Read files with ranges (never more than 200 lines without offset/limit); prefer \`git log\`, \`git show --stat\`, \`gh pr view <n> -R <github> --json title,body,files\` and rg. Refs: <repo id>#<pr> for a commit whose subject ends in "(#N)" (squash-merged PR), otherwise <repo id>@<first 9 of the sha>.`
const REPOS = args.repos
  .map(r => `- ${r.repo}: ${r.path} (${r.github}), range ${r.range}, ${r.commits} non-merge commits. List them: git -C ${r.path} log --no-merges --format='%h %s' ${r.range}`)
  .join('\n')
const DEVICES = (args.devices || []).join(', ') || 'the release devices'

function readerPrompt(c) {
  const others = args.clusters.filter(o => o.name !== c.name).map(o => `"${o.name}"`).join(', ') || 'none'
  return `You inventory ONE journey cluster of the ${args.project} release: "${c.name}". Its focus: ${c.focus}
The other clusters (not yours): ${others}.

Repos and their production..integration ranges:
${REPOS}

1. List every range's commits and take the ones that belong to YOUR cluster, judged by subject, touched paths (\`git show --stat\`) and PR body. A merge-commit PR's constituents count through that PR.
2. Group them into user-facing FEATURES, not commits. For each feature give:
   - name;
   - summary: what changed for users or operators, 2 to 5 concrete sentences;
   - refs;
   - surfaces: routes, screens, endpoints, jobs, config;
   - roles;
   - devices: the subset of ${DEVICES} it must be checked on;
   - existingTests: repo paths of the specs and tests that cover it (find them);
   - gaps: behaviours no test covers, risky edges, never-exercised paths, data, migration and deploy risks, each one checkable sentence;
   - risk: low, medium or high;
   - repos: the repo ids it touches.
3. Propose end-to-end JOURNEYS for your cluster. For each give:
   - name;
   - steps: numbered steps a tester follows, including where the roles interact;
   - roles;
   - devices;
   - dataNeeds: seed data, keys, inboxes, flags.
4. Fill in the lists:
   - attributedRefs: EVERY ref you claim.
   - excludedTooling: CI, dev-env, docs and test-only commits of your cluster, one reason each (they ship nothing).
   - openQuestions: release decisions only a human can make (flags, production data, sign-offs, deploy order), each with its evidence.
Be exhaustive inside your cluster; the critic lists every commit nobody claimed. Set "lane" to "${c.name}".
${RULES}`
}

phase('Inventory')
const read = await parallel(args.clusters.map(c => () =>
  agent(readerPrompt(c), { label: `inventory:${c.name}`, phase: 'Inventory', schema: CLUSTER })))
const inventories = read.map((inv, i) => {
  if (inv) return inv
  log(`reader for cluster "${args.clusters[i].name}" returned nothing; its slot in inventory.json says so`)
  return {
    lane: args.clusters[i].name, features: [], journeys: [], attributedRefs: [], excludedTooling: [],
    openQuestions: [`The inventory reader for this cluster returned nothing. Re-run it before lanes are cut.`],
  }
})

// The critic needs every cluster's claims at once: this barrier is the point.
phase('Critic')
const claims = inventories
  .map(inv => `- "${inv.lane}": ${inv.attributedRefs.length} refs: ${inv.attributedRefs.join(', ') || '(none)'}; ${inv.excludedTooling.length} tooling exclusions`)
  .join('\n')
const critic = await agent(`You are the completeness critic of the ${args.project} release inventory. Read every range yourself:
${REPOS}

What the clusters claimed:
${claims}

1. Attribute EVERY non-merge commit of every range to one of four buckets:
   - claimed exactly (the ref matches);
   - covered through a claimed PR (merge-commit or umbrella PRs whose constituents carry no number);
   - tooling (CI, dev env, docs, test-only);
   - UNASSIGNED. For each unassigned FUNCTIONAL commit give {ref, repo, subject, lane}, where lane is the best-fitting cluster name.
2. Phantom claims: refs a cluster claimed that are in no range (open PRs, other branches). They are not in the release.
3. overlaps: refs claimed by more than one cluster, as {ref, lanes, owner}. The owner is one cluster, or "split-by-commit (…)".
4. toolingCount: the number of commits that ship nothing.
5. notes:
   - the counts per repo (commits, claimed, covered, tooling, unassigned);
   - the phantom claims and umbrella caveats;
   - release-relevant details the clusters missed: env or config contract changes, migrations, flags, security-relevant commits hidden behind umbrella refs.
${RULES}`, { label: 'critic', phase: 'Critic', schema: CRITIC })
if (!critic) log('critic returned nothing: every commit is unattributed until it is re-run')

return {
  inventories,
  critic: critic || { notes: 'The critic returned nothing. Re-run it before lanes are cut.', overlaps: [], toolingCount: 0, unassigned: [] },
}
