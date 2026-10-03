import type { On, RenderPropsOf } from 'claude-code'
import { expect, mock, test } from 'claude-code/testing'

import type { JobKind, JobStatus, LedgerEvent, LedgerJob } from './types/fleet.gen'

const SURFACES = ['terminal', 'desktop'] as const
const BAND: RenderPropsOf['AbovePrompt'] = {
  hasSurvey: false,
  isWorking: false,
  maxRows: 6,
  bodyColumns: 120,
  scroll: { offset: 0, bodyRows: 6 },
  view: {},
}
const SESSION = 'b2a0c1d4-0000-4000-8000-000000000001'
const T0 = Date.UTC(2026, 9, 2, 12, 0, 0)
const MINUTE = 60_000

type Recorded = { session: string; event: LedgerEvent }

// fleet stands for `vybava fleet ledger`: show answers the session's jobs,
// record upserts by kind and id, as internal/fleet does.
function fleet(on: On, jobs: LedgerJob[]): Recorded[] {
  const recorded: Recorded[] = []
  const ok = (stdout: string) => ({ value: { exitCode: 0, stdout, stderr: '', isStdoutTruncated: false, isStderrTruncated: false } })
  on('process.run', (_$, e) => {
    const [bin, applet, noun, verb] = e.argv
    const session = e.argv[e.argv.indexOf('--session') + 1] ?? ''
    if (bin !== 'vybava' || applet !== 'fleet' || noun !== 'ledger') {
      return { value: { exitCode: 127, stdout: '', stderr: 'unexpected', isStdoutTruncated: false, isStderrTruncated: false } }
    }
    if (verb === 'show') {
      const data = { version: 1, sessionId: session, updatedAt: new Date(T0).toISOString(), jobs }
      return ok(JSON.stringify({ v: 3, ok: true, verb: 'fleet ledger show', data, diagnostics: [], next: [] }))
    }
    const event = JSON.parse(e.init?.stdin ?? '{}') as LedgerEvent
    recorded.push({ session, event })
    const found = jobs.find(job => job.kind === event.kind && job.id === event.id)
    if (found === undefined) {
      jobs.push(job(event.kind, event.id, event.status, event))
    } else {
      found.status = event.status
    }
    return ok('')
  })
  return recorded
}

function job(kind: JobKind, id: string, status: JobStatus, more: Partial<LedgerJob> = {}): LedgerJob {
  const at = new Date(T0 - 10 * MINUTE).toISOString()
  return { kind, id, status, startedAt: at, updatedAt: at, ...more }
}

function session(on: On): string[] {
  const submitted: string[] = []
  on('session.start', (_$, e) => ({ cwd: e.cwd }))
  on('session.id', () => ({ value: SESSION }))
  on('command.register', (_$, e) => ({ value: { command: e.name } }))
  on('prompt.submit', (_$, e) => {
    // An engine before 2.1.287 leaves origin unset on a plugin's own submit; in
    // this test only the mod submits without an explicit origin.
    if ((e.origin?.kind ?? 'plugin') === 'plugin') {
      submitted.push(e.text)
    }
    return { text: e.text }
  })
  on('ui.render', { component: 'AbovePrompt' }, () => ({ type: 'engine', ref: 0 }))
  return submitted
}

test('jobs that died with the previous process get one key that hands them back', async ($, on) => {
  mock.clock(on, { now: T0 })
  const recorded = fleet(on, [
    job('workflow', 'task-wf1', 'started', { runId: 'wf_abc', scriptPath: '/s/mods-wave.js', description: 'mods-wave' }),
    job('shell', 'b7k2', 'started', { description: 'devbox run verify' }),
    job('shell', 'old', 'completed'),
  ])
  const submitted = session(on)

  await $.session.start({ cwd: '/repo', surface: 'terminal', isInteractive: true })
  for (const surface of SURFACES) {
    const ui = await $.ui.mount({ plugin: 'lazarus', surface, component: 'AbovePrompt', props: BAND })
    expect(await ui.find({ type: 'Text', text: /1 workflow \+ 1 shell died/ })).toBeDefined()
    expect(await ui.find({ key: 'lazarus-resume' })).toBeDefined()
    await ui.unmount()
  }

  const ui = await $.ui.mount({ plugin: 'lazarus', surface: 'terminal', component: 'AbovePrompt', props: BAND })
  await ui.press({ key: 'lazarus-resume' })
  expect(submitted.length).toBe(1)
  expect(submitted[0]).toContain('Workflow({ scriptPath: "/s/mods-wave.js", resumeFromRunId: "wf_abc" })')
  expect(submitted[0]).toContain('shell "devbox run verify" (b7k2): died; start it again only if it is still needed')
  expect(recorded.map(one => `${one.event.id}:${one.event.status}`)).toEqual(['task-wf1:stopped', 'b7k2:stopped'])
  expect(await ui.find({ key: 'lazarus-resume' })).toBeUndefined()
})

test('a usage-limit stall continues by itself after the reset, never before, and not after a turn', async ($, on) => {
  const clock = mock.clock(on, { now: T0 })
  const recorded = fleet(on, [])
  const submitted = session(on)
  on('session.usage', () => ({
    value: {
      startedAt: T0,
      context: { window: 1_000_000 },
      rateLimits: [{ kind: 'five_hour', percentUsed: 100, resetsAt: new Date(clock.now() + 30 * MINUTE).toISOString() }],
    },
  }))
  on('turn.start', (_$, e) => ({ turnId: e.turnId }))
  on('classic.StopFailure', () => ({}))

  await $.session.start({ cwd: '/repo', surface: 'terminal', isInteractive: true })
  await $.classic.StopFailure({ error: 'rate_limit' })
  expect(recorded.at(-1)?.event).toMatchObject({ kind: 'limit-wait', status: 'started' })
  await clock.advance(30 * MINUTE - 1000)
  expect(submitted).toEqual([])
  await clock.advance(62_000)
  expect(submitted).toEqual(['continue — the usage limit has reset'])
  expect(recorded.at(-1)?.event).toMatchObject({ kind: 'limit-wait', status: 'completed' })

  // A turn started before the reset (the engine's own auto-continue, or the
  // person): the wake stands down.
  await $.classic.StopFailure({ error: 'rate_limit' })
  await $.turn.start({ text: 'go on', turnId: 't2' })
  await clock.advance(32 * MINUTE)
  expect(submitted.length).toBe(1)
})

test('a turn end closes a job only when the in-flight list proves it ended', async ($, on) => {
  mock.clock(on, { now: T0 })
  const recorded = fleet(on, [])
  session(on)
  on('tool.call', { tool: 'Bash' }, () => ({ result: { stdout: '', stderr: '', interrupted: false, backgroundTaskId: 'b9' } }))
  on('classic.Stop', () => ({}))
  const stops = () => recorded.filter(one => one.event.id === 'b9' && one.event.status === 'stopped').length

  await $.session.start({ cwd: '/repo', surface: 'terminal', isInteractive: true })
  await $.tool.call({ tool: 'Bash', command: 'bun test', run_in_background: true, description: 'Run the suite' })
  // Ids that name none of ours prove nothing about b9: it stays open.
  await $.classic.Stop({ stop_hook_active: false, background_tasks: [{ id: 'other-space-1', type: 'shell', status: 'running', description: 'bun test' }] })
  expect(stops()).toBe(0)
  // Nothing in flight at all: b9 has ended.
  await $.classic.Stop({ stop_hook_active: false, background_tasks: [] })
  expect(stops()).toBe(1)
})

test('a background shell without a description never puts its command in the ledger text', async ($, on) => {
  mock.clock(on, { now: T0 })
  const recorded = fleet(on, [])
  session(on)
  on('tool.call', { tool: 'Bash' }, () => ({ result: { stdout: '', stderr: '', interrupted: false, backgroundTaskId: 'b7' } }))

  await $.session.start({ cwd: '/repo', surface: 'terminal', isInteractive: true })
  await $.tool.call({ tool: 'Bash', command: 'TOKEN=not-for-the-ledger curl https://example.test', run_in_background: true })
  const started = recorded.find(one => one.event.id === 'b7' && one.event.status === 'started')
  expect(started?.event.description).toBe('background shell')
})

test('/park says whether a restart is safe from the jobs still running', async ($, on) => {
  mock.clock(on, { now: T0 })
  fleet(on, [job('workflow', 'from-before', 'started', { description: 'old run' })])
  session(on)
  on('tool.call', { tool: 'Bash' }, () => ({ result: { stdout: '', stderr: '', interrupted: false, backgroundTaskId: 'b9' } }))
  const park = () =>
    $.command.run({ command: 'park', args: '', origin: { kind: 'composer' }, presentation: { isFullscreen: false, columns: 120 } })

  await $.session.start({ cwd: '/repo', surface: 'terminal', isInteractive: true })
  await $.tool.call({ tool: 'Bash', command: 'bun test', run_in_background: true, description: 'Run the suite' })
  const busy = await park()
  expect(busy.text).toContain('Not safe to restart: 1 job would die')
  expect(busy.text).toContain('shell "Run the suite" (b9)')
  expect(busy.text).not.toContain('old run')

  await $.prompt.submit({
    text: '<task-notification><task-id>b9</task-id><status>completed</status></task-notification>',
    wait: false,
    origin: { kind: 'task-notification' },
  })
  const idle = await park()
  expect(idle.text).toContain('Safe to restart')
})
