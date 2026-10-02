import { atom, read, update } from 'claude-code'
import type { EngineInterface, Register, Timer } from 'claude-code'

import type { LazarusJob, LazarusKind, LazarusStall } from '../types'
import type { Envelope, JobStatus, Ledger, LedgerEvent, LedgerJob } from '../types/fleet.gen'

const mine = atom({ plugin: 'lazarus', key: 'mine' } as const, [] as LazarusJob[])
const stoppedByModel = atom({ plugin: 'lazarus', key: 'stoppedByModel' } as const, [] as string[])
const dead = atom({ plugin: 'lazarus', key: 'dead' } as const, [] as LazarusJob[])
const diedAt = atom({ plugin: 'lazarus', key: 'diedAt' } as const, 0)
const stall = atom({ plugin: 'lazarus', key: 'stall' } as const, null as LazarusStall | null)
const lastTurnAt = atom({ plugin: 'lazarus', key: 'lastTurnAt' } as const, 0)

const PANE = 'lazarus-jobs'
const LIMIT_ID = 'usage-limit'
const JITTER_MAX_MS = 60_000
const RUN_TIMEOUT_MS = 15_000
const FOLLOWED: readonly LazarusKind[] = ['workflow', 'shell', 'monitor', 'agent']
const AUTH_ERRORS = new Set(['authentication_failed', 'oauth_org_not_allowed', 'account_on_hold', 'verification_required', 'billing_error'])
const TASK_ID = /<task-id>([^<]+)<\/task-id>/
const TASK_STATUS = /<status>([^<]+)<\/status>/
const WORDS: Record<LazarusKind, readonly [string, string]> = {
  workflow: ['workflow', 'workflows'],
  shell: ['shell', 'shells'],
  monitor: ['monitor', 'monitors'],
  agent: ['agent', 'agents'],
}

// The one timer lazarus keeps: the wake at a usage limit's reset. A reload
// drops it with the module; session.start re-arms it from the ledger.
let limitTimer: Timer | null = null
let ledgerWarned = false

// lazarus records the background jobs the main loop launches in the
// per-session ledger (`vybava fleet ledger`), so that after a crash or a
// restart one key hands what died back to the model. Every tool call passes
// through unchanged; the ledger is written only when a background job
// starts or ends, never on a timer.
export const register: Register = on => {
  on('session.start', async ($, e, next) => {
    await $.command.register({
      name: 'park',
      description: 'Is it safe to restart? Lists the background jobs a restart would kill',
      immediate: true,
    })
    await scan($, await $.session.id())
    return next(e)
  })

  // `claude --resume` and /resume land here; /resume fires no session.start.
  on('classic.SessionStart', async ($, e, next) => {
    const ran = await next(e)
    if (e.source === 'resume') {
      await scan($, e.session_id)
    }
    return ran
  })

  on('turn.start', async ($, e, next) => {
    const now = await $.clock.now()
    await update($, lastTurnAt, () => now)
    const stalled = await read($, stall)
    if (stalled?.kind === 'limit') {
      // The engine's own auto-continue, the person or our wake started it.
      await clearStall($, 'completed')
    }
    return next(e)
  })

  on('turn.complete', async ($, e, next) => {
    const ran = await next(e)
    if (e.agentId === undefined && e.reason === 'answer' && (await read($, stall))?.kind === 'auth') {
      await update($, stall, () => null)
    }
    return ran
  })

  on('tool.call', { tool: 'Workflow' }, async ($, e, next) => {
    const ran = await next(e)
    if (e.agentId === undefined && ran.deny === undefined && ran.isError !== true && ran.result.status === 'async_launched') {
      await launched($, {
        kind: 'workflow',
        id: ran.result.taskId,
        runId: ran.result.runId,
        scriptPath: ran.result.scriptPath,
        description: ran.result.workflowName ?? 'workflow',
        startedAt: await $.clock.now(),
      })
    }
    return ran
  })

  on('tool.call', { tool: 'Bash' }, async ($, e, next) => {
    const ran = await next(e)
    if (e.agentId === undefined && e.run_in_background === true && ran.deny === undefined && ran.isError !== true) {
      const taskId = ran.result.backgroundTaskId
      if (taskId !== undefined) {
        const description = e.description ?? e.command.trim().split('\n')[0] ?? 'shell'
        await launched($, { kind: 'shell', id: taskId, description, startedAt: await $.clock.now() }, e.command)
      }
    }
    return ran
  })

  on('tool.call', { tool: 'Monitor' }, async ($, e, next) => {
    const ran = await next(e)
    if (e.agentId === undefined && ran.deny === undefined && ran.isError !== true) {
      await launched($, { kind: 'monitor', id: ran.result.taskId, description: e.description, startedAt: await $.clock.now() }, e.command)
    }
    return ran
  })

  on('tool.call', { tool: 'Agent' }, async ($, e, next) => {
    const ran = await next(e)
    // A model's background Agent call answers `async_launched` with its id; a
    // plugin's spawn answers an AgentCallRecord, which has no status.
    if (e.agentId === undefined && ran.deny === undefined && ran.isError !== true && 'status' in ran.result && ran.result.status === 'async_launched') {
      await launched($, { kind: 'agent', id: ran.result.agentId, description: e.description, startedAt: await $.clock.now() })
    }
    return ran
  })

  on('tool.call', { tool: 'TaskStop' }, async ($, e, next) => {
    const id = e.task_id ?? e.shell_id
    if (e.agentId === undefined && id !== undefined) {
      await update($, stoppedByModel, ids => [...ids.filter(one => one !== id), id].slice(-200))
    }
    return next(e)
  })

  on('prompt.submit', async ($, e, next) => {
    if (e.origin?.kind === 'task-notification') {
      const id = TASK_ID.exec(e.text)?.[1]
      const status = TASK_STATUS.exec(e.text)?.[1]
      if (id !== undefined && status !== undefined) {
        await ended($, id, status)
      }
    }
    return next(e)
  })

  // The turn's end carries the engine's list of background work still in
  // flight: a job of ours missing from it ended without a notification we
  // saw. Absence proves that only when nothing is in flight, or when the list
  // names a job of ours (so its ids are the ones the launches answered);
  // otherwise the job is left to its notification, never closed on a guess.
  // Agents are always left to their notifications; their task id need not be
  // the agent id the launch answered.
  on('classic.Stop', async ($, e, next) => {
    const ran = await next(e)
    if (e.agent_id === undefined && e.background_tasks !== undefined) {
      const live = new Set(e.background_tasks.map(task => task.id))
      const ours = (await read($, mine)).filter(job => job.kind !== 'agent')
      const isProven = live.size === 0 || ours.some(job => live.has(job.id))
      const gone = isProven ? ours.filter(job => !live.has(job.id)) : []
      for (const job of gone) {
        await update($, mine, list => list.filter(one => one.id !== job.id))
        await record($, { kind: job.kind, id: job.id, status: 'stopped' })
      }
    }
    return ran
  })

  on('classic.StopFailure', async ($, e, next) => {
    const ran = await next(e)
    if (e.agent_id !== undefined) {
      return ran
    }
    if (e.error === 'rate_limit') {
      await limitStall($)
    } else if (AUTH_ERRORS.has(e.error)) {
      const now = await $.clock.now()
      await update($, stall, (): LazarusStall => ({ kind: 'auth', at: now, error: e.error }))
    }
    return ran
  })

  on('command.run', { command: 'park' }, async $ => {
    return { text: await park($) }
  })

  on('ui.render', { component: 'AbovePrompt' }, async ($, e, next) => {
    const jobs = await read($, dead)
    const stalled = await read($, stall)
    const theirs = await next(e)
    if (e.props.hasSurvey || (jobs.length === 0 && stalled === null)) {
      return theirs
    }
    const { Box, Text, Button } = $.ui.resolve(e)
    if (jobs.length > 0) {
      const at = await read($, diedAt)
      return (
        <Box flexDirection="column">
          <Box flexDirection="row" columnGap={1}>
            <Text>⟲ {summary(jobs)} died {clock(at)}</Text>
            <Button key="lazarus-resume" label="resume all" hotkey="1" variant="primary" onPress={() => { void resumeAll($) }} />
            <Button key="lazarus-list" label="list" hotkey="2" onPress={() => { void $.ui.open({ id: PANE, title: 'lazarus · what died', closeOnEscape: true }) }} />
            <Button key="lazarus-dismiss" label="dismiss" hotkey="3" onPress={() => { void dismiss($) }} />
          </Box>
          {theirs}
        </Box>
      )
    }
    if (stalled === null) {
      return theirs
    }
    const text = stalled.kind === 'auth'
      ? `⚠ Claude auth failed (${stalled.error}) — switch the account in a terminal: switcheroo cs <account>`
      : stalled.until === null
        ? '⏸ usage limit hit'
        : `⏸ usage limit — continues by itself at ${clock(stalled.until)}`
    const go = stalled.kind === 'auth' ? 'retry' : stalled.until === null ? 'continue' : 'continue now'
    return (
      <Box flexDirection="column">
        <Box flexDirection="row" columnGap={1}>
          <Text>{text}</Text>
          <Button key="lazarus-continue" label={go} hotkey="1" variant="primary" onPress={() => { void continueNow($) }} />
          <Button key="lazarus-cancel" label={stalled.kind === 'limit' && stalled.until !== null ? 'cancel' : 'dismiss'} hotkey="3" onPress={() => { void clearStall($, 'stopped') }} />
        </Box>
        {theirs}
      </Box>
    )
  })

  on('ui.render', { component: 'Pane', requestId: PANE }, async ($, e) => {
    const { Box, Text, Button } = $.ui.resolve(e)
    const jobs = await read($, dead)
    return (
      <Box flexDirection="column">
        {jobs.length === 0 && <Text dimColor>Nothing died.</Text>}
        {jobs.map(job => <Text wrap="truncate-end">{describeJob(job)}</Text>)}
        <Button key="lazarus-close" label="close" role="dismiss" onPress={() => { void $.ui.close({ id: PANE }) }} />
      </Box>
    )
  })
}

// scan reads a session's ledger: an open job this process did not launch
// died with the process before it; an open usage-limit wait is re-armed.
async function scan($: EngineInterface, session: string): Promise<void> {
  const ledger = await showLedger($, session)
  if (ledger === null) {
    return
  }
  const known = new Set((await read($, mine)).map(job => job.id))
  const now = await $.clock.now()
  const lost = ledger.jobs.filter(job => job.status === 'started' && isFollowed(job.kind) && !known.has(job.id)).map(fromLedger)
  if (lost.length > 0) {
    await update($, dead, list => [...list.filter(one => !lost.some(job => job.id === one.id)), ...lost])
    await update($, diedAt, () => now)
  }
  const wait = ledger.jobs.find(job => job.kind === 'limit-wait' && job.status === 'started')
  if (wait === undefined) {
    return
  }
  const until = wait.until === undefined ? null : Date.parse(wait.until)
  const at = Date.parse(wait.startedAt)
  if (until !== null && until > now) {
    await update($, stall, (): LazarusStall => ({ kind: 'limit', at, until }))
    armLimit($, until - now)
  } else {
    // The reset passed while nothing ran: the person is back, the key decides.
    await update($, stall, (): LazarusStall => ({ kind: 'limit', at, until: null }))
  }
}

async function launched($: EngineInterface, job: LazarusJob, command?: string): Promise<void> {
  await update($, mine, list => [...list.filter(one => one.id !== job.id), job])
  await record($, {
    kind: job.kind,
    id: job.id,
    status: 'started',
    runId: job.runId,
    scriptPath: job.scriptPath,
    description: job.description,
    command,
  })
}

// ended closes a job of ours on its notification. A job killed or stopped
// that the model did not stop itself died while the session ran.
async function ended($: EngineInterface, id: string, word: string): Promise<void> {
  const job = (await read($, mine)).find(one => one.id === id)
  const status = toStatus(word)
  if (job === undefined || status === null) {
    return
  }
  await update($, mine, list => list.filter(one => one.id !== id))
  await record($, { kind: job.kind, id, status })
  if ((status === 'killed' || status === 'stopped') && !(await read($, stoppedByModel)).includes(id)) {
    await update($, dead, list => [...list.filter(one => one.id !== id), job])
    const now = await $.clock.now()
    await update($, diedAt, () => now)
  }
}

// resumeAll hands what died to the model as facts: workflows with the exact
// call that resumes them, the rest for it to judge. The prompt is framed as
// lazarus's; nothing is re-run behind the model's back.
async function resumeAll($: EngineInterface): Promise<void> {
  const jobs = await read($, dead)
  if (jobs.length === 0) {
    return
  }
  await update($, dead, () => [])
  if (!(await submit($, resumePrompt(jobs)))) {
    // Nothing reached the model: the jobs stay offered.
    await update($, dead, list => [...jobs, ...list.filter(one => !jobs.some(job => job.id === one.id))])
    return
  }
  for (const job of jobs) {
    await record($, { kind: job.kind, id: job.id, status: 'stopped' })
  }
}

// submit starts a turn framed as lazarus's; a refusal is logged and shown,
// never lost in a press handler or a timer.
async function submit($: EngineInterface, text: string): Promise<boolean> {
  try {
    await $.prompt.submit({ text })
    return true
  } catch (err) {
    $.ui.log(`could not start a turn: ${String(err)}`, { to: 'debug' })
    $.ui.toast('could not start a turn; see the debug log')
    return false
  }
}

async function dismiss($: EngineInterface): Promise<void> {
  const jobs = await read($, dead)
  await update($, dead, () => [])
  for (const job of jobs) {
    await record($, { kind: job.kind, id: job.id, status: 'stopped' })
  }
  await $.ui.close({ id: PANE })
}

async function limitStall($: EngineInterface): Promise<void> {
  const now = await $.clock.now()
  const usage = await $.session.usage()
  const resets = usage.rateLimits
    .filter(limit => limit.resetsAt !== undefined && limit.percentUsed >= 100)
    .map(limit => Date.parse(limit.resetsAt ?? ''))
    .filter(at => Number.isFinite(at) && at > now)
  if (resets.length === 0) {
    await update($, stall, (): LazarusStall => ({ kind: 'limit', at: now, until: null }))
    await record($, { kind: 'limit-wait', id: LIMIT_ID, status: 'started' })
    return
  }
  // Spread the sessions one limit stalled over a minute after its reset.
  const until = Math.max(...resets) + jitter(await $.session.id())
  await update($, stall, (): LazarusStall => ({ kind: 'limit', at: now, until }))
  await record($, { kind: 'limit-wait', id: LIMIT_ID, status: 'started', until: new Date(until).toISOString() })
  armLimit($, until - now)
}

function armLimit($: EngineInterface, delay: number): void {
  limitTimer?.cancel()
  limitTimer = $.clock.after(Math.max(0, delay), () => {
    limitTimer = null
    void limitReset($)
  })
}

// limitReset is the wake at the reset: it continues the session only when
// no turn started since the stall (the engine's own auto-continue, or the
// person, would have cleared the stall already).
async function limitReset($: EngineInterface): Promise<void> {
  const stalled = await read($, stall)
  if (stalled?.kind !== 'limit') {
    return
  }
  const started = (await read($, lastTurnAt)) > stalled.at
  await clearStall($, 'completed')
  if (!started) {
    await submit($, 'continue — the usage limit has reset')
  }
}

async function continueNow($: EngineInterface): Promise<void> {
  const stalled = await read($, stall)
  if (stalled === null) {
    return
  }
  await clearStall($, 'completed')
  await submit($, stalled.kind === 'limit' ? 'continue — the usage limit has reset' : 'continue')
}

async function clearStall($: EngineInterface, status: JobStatus): Promise<void> {
  const stalled = await read($, stall)
  limitTimer?.cancel()
  limitTimer = null
  await update($, stall, () => null)
  if (stalled?.kind === 'limit') {
    await record($, { kind: 'limit-wait', id: LIMIT_ID, status })
  }
}

async function park($: EngineInterface): Promise<string> {
  const session = await $.session.id()
  const ledger = await showLedger($, session)
  const known = await read($, mine)
  const running = ledger === null
    ? known
    : ledger.jobs.filter(job => job.status === 'started' && isFollowed(job.kind) && known.some(one => one.id === job.id)).map(fromLedger)
  const lines = running.map(job => `- ${describeJob(job)}`)
  const verdict = running.length === 0
    ? 'Safe to restart: no background job of this session is running.'
    : `Not safe to restart: ${running.length === 1 ? '1 job' : `${running.length} jobs`} would die (lazarus offers them for resume afterwards).`
  const parts = [verdict, ...lines]
  if (ledger === null) {
    parts.push('(The ledger was unreadable; this lists only what this process launched.)')
  }
  await record($, { kind: 'park', id: `park-${await $.clock.now()}`, status: 'completed', description: verdict })
  return parts.join('\n')
}

async function showLedger($: EngineInterface, session: string): Promise<Ledger | null> {
  const out = await vybava($, ['fleet', 'ledger', 'show', '--session', session, '--json'])
  if (out === null) {
    return null
  }
  try {
    const envelope = JSON.parse(out) as Envelope<Ledger>
    if (envelope.ok && envelope.data !== undefined) {
      return envelope.data
    }
    $.ui.log(`fleet ledger show answered not ok: ${envelope.diagnostics.map(d => d.code).join(', ')}`, { to: 'debug' })
  } catch (err) {
    $.ui.log(`fleet ledger show printed no envelope: ${String(err)}`, { to: 'debug' })
  }
  return null
}

async function record($: EngineInterface, event: LedgerEvent): Promise<void> {
  const session = await $.session.id()
  const out = await vybava($, ['fleet', 'ledger', 'record', '--session', session], JSON.stringify(event))
  if (out === null && !ledgerWarned) {
    ledgerWarned = true
    $.ui.toast('the job ledger cannot be written; crash recovery is off for this session (see the debug log)')
  }
}

// vybava runs one fleet verb; a failure is logged to the debug log and
// answered as null, never thrown into the tool call it rides on.
async function vybava($: EngineInterface, argv: readonly string[], stdin?: string): Promise<string | null> {
  try {
    const ran = await $.process.run(['vybava', ...argv], stdin === undefined ? { timeoutMs: RUN_TIMEOUT_MS } : { stdin, timeoutMs: RUN_TIMEOUT_MS })
    if (ran.exitCode === 0) {
      return ran.stdout
    }
    $.ui.log(`vybava ${argv.slice(0, 3).join(' ')} exited ${ran.exitCode}: ${firstLine(ran.stderr === '' ? ran.stdout : ran.stderr)}`, { to: 'debug' })
  } catch (err) {
    $.ui.log(`vybava ${argv.slice(0, 3).join(' ')} failed: ${String(err)}`, { to: 'debug' })
  }
  return null
}

function resumePrompt(jobs: readonly LazarusJob[]): string {
  const lines = jobs.map(job => {
    if (job.kind === 'workflow' && job.runId !== undefined && job.scriptPath !== undefined) {
      return `- workflow "${job.description ?? job.id}": resume it with Workflow({ scriptPath: ${JSON.stringify(job.scriptPath)}, resumeFromRunId: ${JSON.stringify(job.runId)} })`
    }
    return `- ${describeJob(job)}: died; start it again only if it is still needed`
  })
  return [
    `${summary(jobs)} this session started died with it (a crash, a restart or a kill). The person pressed resume.`,
    ...lines,
    'Resume the workflows; decide on the rest from where the work stands.',
  ].join('\n')
}

function describeJob(job: LazarusJob): string {
  const what = `${WORDS[job.kind][0]} "${job.description ?? job.id}"`
  return job.runId === undefined ? `${what} (${job.id})` : `${what} (run ${job.runId})`
}

function summary(jobs: readonly LazarusJob[]): string {
  return FOLLOWED.map(kind => {
    const count = jobs.filter(job => job.kind === kind).length
    return count === 0 ? null : `${count} ${WORDS[kind][count === 1 ? 0 : 1]}`
  })
    .filter((part): part is string => part !== null)
    .join(' + ')
}

function fromLedger(job: LedgerJob): LazarusJob {
  const started = Date.parse(job.startedAt)
  return {
    kind: job.kind as LazarusKind,
    id: job.id,
    runId: job.runId,
    scriptPath: job.scriptPath,
    description: job.description,
    startedAt: Number.isFinite(started) ? started : 0,
  }
}

function isFollowed(kind: string): boolean {
  return (FOLLOWED as readonly string[]).includes(kind)
}

function toStatus(word: string): JobStatus | null {
  switch (word.trim()) {
    case 'completed':
      return 'completed'
    case 'failed':
      return 'failed'
    case 'killed':
      return 'killed'
    case 'stopped':
      return 'stopped'
    default:
      return null
  }
}

// jitter spreads the sessions one limit stalled: a stable 0–60 s per session.
function jitter(session: string): number {
  let hash = 0
  for (const char of session) {
    hash = (hash * 31 + char.charCodeAt(0)) >>> 0
  }
  return hash % JITTER_MAX_MS
}

function clock(ms: number): string {
  const at = new Date(ms)
  return `${String(at.getHours()).padStart(2, '0')}:${String(at.getMinutes()).padStart(2, '0')}`
}

function firstLine(text: string): string {
  return (text.trim().split('\n')[0] ?? '').slice(0, 200)
}
