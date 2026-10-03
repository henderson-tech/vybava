import type { On, RenderPropsOf } from 'claude-code'
import { expect, mock, test } from 'claude-code/testing'

import type { Envelope, FleetSession, FleetSnapshot, FleetSummary } from './types/fleet.gen'

const SURFACES = ['terminal', 'desktop'] as const
const NOW = Date.parse('2026-10-02T18:00:00Z')
const SELF = '51fc46df-788c-46e8-a3c9-3efeb3358873'
const W1 = 'da1953bd-ec3e-4129-8eda-b5277e45dbef'
const W2 = '6fd7979b-0000-4000-8000-000000000002'
const D1 = '9cdd4446-0000-4000-8000-000000000003'
const PANE: RenderPropsOf['Pane'] = {
  title: 'Fleet',
  isFocused: true,
  bodyColumns: 100,
  placement: 'dock',
  scroll: { offset: 0, bodyRows: 40 },
  view: {},
}

function session(over: Partial<FleetSession> & Pick<FleetSession, 'sessionId' | 'state'>): FleetSession {
  return {
    pid: 4242,
    project: 'FixIt',
    cwd: '/repo',
    status: over.state,
    statusSince: '2026-10-02T17:00:00Z',
    ageSeconds: 60,
    liveness: over.state === 'dead' ? 'gone' : 'alive',
    openJobs: 0,
    resume: `cd /repo && claude --resume ${over.sessionId}`,
    ...over,
  }
}

// Mirrors `vybava fleet --json`: sessions waiting first, as the applet orders them.
const SNAPSHOT: FleetSnapshot = {
  generatedAt: '2026-10-02T18:00:00Z',
  registry: '/Users/l/.claude/sessions',
  counts: { total: 7, waiting: 2, busy: 2, idle: 1, shell: 1, dead: 1, ended: 0, unknown: 0 },
  projects: [],
  sessions: [
    session({ sessionId: W2, state: 'waiting', name: 'fixit-5b', waitingFor: 'input needed', ageSeconds: 2100 }),
    session({ sessionId: W1, state: 'waiting', name: 'reservine-c4', project: 'reservine', waitingFor: 'input needed', ageSeconds: 30706 }),
    session({ sessionId: D1, state: 'dead', name: 'pwf-ui-9c', project: 'pwf-ui', openJobs: 2 }),
    session({ sessionId: SELF, state: 'busy', name: 'vybava-51', project: 'vybava' }),
    session({ sessionId: 'b1', state: 'busy', name: 'fixit-a5' }),
    session({ sessionId: 'i1', state: 'idle', name: 'voke-c0' }),
    session({ sessionId: 's1', state: 'shell', name: 'voke-cd' }),
  ],
  codex: [{ pid: 777, threadId: 'th-1', name: 'sidekick verify', project: 'FixIt', cwd: '/repo', calls: 14, lastCallAt: '2026-10-02T17:51:00Z' }],
}
const ENVELOPE: Envelope<FleetSnapshot> = { v: 3, ok: true, verb: 'fleet', data: SNAPSHOT, diagnostics: [], next: [] }

// The person typing /fleet at the prompt of a fullscreen terminal.
const FLEET_COMMAND = { command: 'fleet', args: '', origin: { kind: 'composer' }, presentation: { isFullscreen: true, columns: 180 } } as const

type World = {
  runs: string[][]
  statuses: Array<string | undefined>
  sent: Array<{ to: string; text: string }>
  copies: string[]
  closed: string[]
}

// world answers every noun the mod calls, beneath it, as the engine would.
function world(on: On, summary: FleetSummary | null, fleet: { exitCode: number; stdout: string } = { exitCode: 0, stdout: JSON.stringify(ENVELOPE) }): World {
  const w: World = { runs: [], statuses: [], sent: [], copies: [], closed: [] }
  mock.env(on, { HOME: '/Users/l' })
  on('session.start', (_$, e) => ({ cwd: e.cwd }))
  on('session.id', () => ({ value: SELF }))
  on('command.register', (_$, e) => ({ value: { command: e.name } }))
  on('ui.panes', () => ({ value: [] }))
  on('ui.open', () => ({ value: { isPlaced: true } }))
  on('ui.close', (_$, e) => {
    w.closed.push(e.id)
    return { value: undefined }
  })
  on('ui.log', () => ({ value: undefined }))
  on('ui.toast', () => ({ value: undefined }))
  on('ui.status', (_$, e) => {
    w.statuses.push(e.text)
    return { value: undefined }
  })
  on('process.run', (_$, e) => {
    w.runs.push([...e.argv])
    return { value: { ...fleet, stderr: '', isStdoutTruncated: false, isStderrTruncated: false } }
  })
  on('fs.exists', () => ({ value: summary !== null }))
  on('fs.read', () => ({ value: JSON.stringify(summary) }))
  on('session.send', (_$, e) => {
    w.sent.push({ to: e.to, text: e.text })
    return { isDelivered: true }
  })
  on('ui.copy', (_$, e) => {
    w.copies.push(e.text)
    return { value: { isCopied: true } }
  })
  return w
}

const fleetRuns = (w: World) => w.runs.filter(argv => argv[0] === 'vybava' && argv[1] === 'fleet').length

test('the pane draws the fleet in its groups, waiting on you oldest first', async ($, on) => {
  const clock = mock.clock(on, { now: NOW })
  world(on, null)
  await $.session.start({ cwd: '/repo', surface: 'terminal', isInteractive: true })
  await $.command.run(FLEET_COMMAND)
  await clock.settle()

  for (const surface of SURFACES) {
    const ui = await $.ui.mount({ plugin: 'fleet-pane', surface, component: 'Pane', requestId: 'fleet', props: PANE })
    const lines = (await ui.findAll({ type: 'Text' })).map(one => one.text)
    const at = (pattern: RegExp) => lines.findIndex(line => pattern.test(line))
    expect(at(/Fleet · 7 sessions · 2 waiting on you/)).toBe(0)
    const order = [
      /waiting on you · 2/,
      /reservine-c4 .*input needed .*8h31m/,
      /fixit-5b/,
      /died while busy · 1/,
      /✕ .*pwf-ui-9c .*died · 2 jobs/,
      /working · 2/,
      /vybava-51 \(you\)/,
      /idle · 2 · 1 at a shell/,
      /codex \(read-only\) · 1/,
      /sidekick verify · 14 calls last call 9m ago/,
    ].map(at)
    expect(order.every(index => index >= 0)).toBe(true)
    expect([...order].sort((a, b) => a - b)).toEqual(order)
    // The oldest waiting session answers y and c; this session and the dead one get no reply.
    expect((await ui.find({ key: `reply:${W1}` }))?.props.hotkey).toBe('y')
    expect((await ui.find({ key: `copy:${W1}` }))?.props.hotkey).toBe('c')
    expect(await ui.find({ key: `reply:${SELF}` })).toBeUndefined()
    expect(await ui.find({ key: `reply:${D1}` })).toBeUndefined()
    expect(await ui.find({ key: `copy:${D1}` })).toBeDefined()
  }
})

test('reply sends what was typed; copy puts the resume line on the clipboard', async ($, on) => {
  const clock = mock.clock(on, { now: NOW })
  const w = world(on, null)
  await $.session.start({ cwd: '/repo', surface: 'terminal', isInteractive: true })
  await $.command.run(FLEET_COMMAND)
  await clock.settle()

  for (const surface of SURFACES) {
    w.sent.length = 0
    w.copies.length = 0
    const ui = await $.ui.mount({ plugin: 'fleet-pane', surface, component: 'Pane', requestId: 'fleet', props: PANE })
    await ui.press({ key: `reply:${W1}` })
    await ui.input({ key: 'reply-text', text: '  merge it  ' })
    expect(w.sent).toHaveLength(1)
    expect(w.sent[0]?.text).toBe('merge it')
    expect(w.sent[0]?.to).toContain(W1)
    expect(await ui.find({ type: 'Text', text: 'sent to reservine-c4' })).toBeDefined()
    expect(await ui.find({ key: 'reply-text' })).toBeUndefined()

    await ui.press({ key: `copy:${D1}` })
    expect(w.copies).toEqual([`cd /repo && claude --resume ${D1}`])
    expect(await ui.find({ type: 'Text', text: /^copied: / })).toBeDefined()
  }
})

test('the status line counts the published summary, says when it is stale, and is absent without it', async ($, on) => {
  const fresh: FleetSummary = {
    generatedAt: new Date(NOW - 30_000).toISOString(),
    counts: SNAPSHOT.counts,
    waiting: [
      { sessionId: W1, project: 'reservine', ageSeconds: 30706 },
      { sessionId: SELF, project: 'vybava', ageSeconds: 10 },
    ],
  }
  const clock = mock.clock(on, { now: NOW })
  const w = world(on, fresh)
  await $.session.start({ cwd: '/repo', surface: 'terminal', isInteractive: true })
  await clock.settle()
  expect(w.statuses.at(-1)).toBe('1 waiting on you · /fleet')

  // Nobody refreshes the file for three minutes: old counts are not shown.
  await clock.advance(3 * 60_000)
  expect(w.statuses.at(-1)).toBe('fleet summary stale (is vybava.watchd running?)')
  expect(fleetRuns(w)).toBe(0)
})

test('no summary file, no status line', async ($, on) => {
  const clock = mock.clock(on, { now: NOW })
  const w = world(on, null)
  await $.session.start({ cwd: '/repo', surface: 'terminal', isInteractive: true })
  await clock.settle()
  expect(w.statuses).toEqual([undefined])
})

test('the pane refreshes only while it is open', async ($, on) => {
  const clock = mock.clock(on, { now: NOW })
  const w = world(on, null)
  await $.session.start({ cwd: '/repo', surface: 'terminal', isInteractive: true })
  expect(fleetRuns(w)).toBe(0)

  await $.command.run(FLEET_COMMAND)
  await clock.settle()
  expect(w.runs).toEqual([['vybava', 'fleet', '--json']])
  await clock.advance(5_000)
  expect(w.runs.at(-1)).toEqual(['vybava', 'fleet', '--json', '--no-codex'])
  await clock.advance(55_000)
  expect(w.runs.filter(argv => !argv.includes('--no-codex'))).toHaveLength(2)

  const ui = await $.ui.mount({ plugin: 'fleet-pane', surface: 'terminal', component: 'Pane', requestId: 'fleet', props: PANE })
  expect((await ui.find({ key: 'close' }))?.props.hotkey).toBe('x')
  await ui.press({ key: 'close' })
  expect(w.closed).toEqual(['fleet'])
  const runs = fleetRuns(w)
  await clock.advance(5 * 60_000)
  expect(fleetRuns(w)).toBe(runs)
})

test('a failed refresh is drawn in the pane, never swallowed', async ($, on) => {
  const clock = mock.clock(on, { now: NOW })
  const broken: Envelope<FleetSnapshot> = {
    v: 3, ok: false, verb: 'fleet', diagnostics: [{ code: 'FLEET_REGISTRY_SHAPE', severity: 'error', detail: 'unknown session registry shape' }], next: [],
  }
  world(on, null, { exitCode: 2, stdout: JSON.stringify(broken) })
  await $.session.start({ cwd: '/repo', surface: 'terminal', isInteractive: true })
  await $.command.run(FLEET_COMMAND)
  await clock.settle()
  for (const surface of SURFACES) {
    const ui = await $.ui.mount({ plugin: 'fleet-pane', surface, component: 'Pane', requestId: 'fleet', props: PANE })
    const line = await ui.find({ type: 'Text', text: /failed \(exit 2\): FLEET_REGISTRY_SHAPE: unknown session registry shape/ })
    expect(line?.props.color).toBe('red')
  }
})
