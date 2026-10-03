import type { On, PromptSubmitInput, RenderPropsOf } from 'claude-code'
import { expect, mock, test } from 'claude-code/testing'
import type { MockClock } from 'claude-code/testing'

const SURFACES = ['terminal', 'desktop'] as const
const HOME = '/home/me'
const SOCKET = `${HOME}/.local/state/vybava/watch/watchd.sock`
const PR = 'pr:henderson-tech/vybava#155'
const BAND: RenderPropsOf['AbovePrompt'] = {
  hasSurvey: false,
  isWorking: false,
  maxRows: 10,
  bodyColumns: 100,
  scroll: { offset: 0, bodyRows: 9 },
  view: {},
}

type Sent = { method: string; path: string; body: Record<string, unknown>; socketPath: string | undefined }
type Wire = { seq: number; subscription: string; target: string; until: string; kind: string; summary?: string; changed?: string[]; error?: string }

/** The watch daemon beneath the plugin: subscribe answers at once, an empty
 * events poll is held until the test pushes an event or 25 s pass. */
function daemon(on: On, clock: MockClock) {
  const sent: Sent[] = []
  let queue: Wire[] = []
  let release: (() => void) | null = null
  const reply = (status: number, body: unknown) => ({ value: { status, ok: status < 300, headers: {}, text: JSON.stringify(body) } })
  on('http.fetch', async (_$, e) => {
    const url = new URL(e.url)
    const method = e.init?.method ?? 'GET'
    const parsed: unknown = e.init?.body === undefined ? {} : JSON.parse(e.init.body)
    const body = typeof parsed === 'object' && parsed !== null ? (parsed as Record<string, unknown>) : {}
    sent.push({ method, path: url.pathname + url.search, body, socketPath: e.init?.socketPath })
    if (url.pathname === '/v1/subscriptions' && method === 'POST') {
      return reply(201, { subscription: { id: 'w1', session: body['session'], target: PR, until: body['until'] }, events: [] })
    }
    if (url.pathname === '/v1/subscriptions') {
      return reply(200, { subscriptions: [], targets: [] })
    }
    if (url.pathname === '/v1/events') {
      const after = Number(url.searchParams.get('after'))
      queue = queue.filter(ev => ev.seq > after)
      if (queue.length === 0 && url.searchParams.get('timeout') !== '0s') {
        await Promise.race([new Promise<void>(resolve => (release = resolve)), clock.sleep(25_000)])
      }
      return reply(200, { events: queue })
    }
    return reply(404, { error: 'no route' })
  })
  return {
    sent,
    polls: () => sent.filter(one => one.path.startsWith('/v1/events')),
    push(ev: Wire) {
      queue.push(ev)
      release?.()
      release = null
    },
  }
}

/** The engine around the plugin: a session, its turns and what it shows. */
function engine(on: On) {
  const seen = { submitted: [] as PromptSubmitInput[], commands: [] as string[], toasts: [] as string[], status: undefined as string | undefined, tools: [] as string[] }
  mock.env(on, { HOME })
  on('session.start', (_$, e) => ({ cwd: e.cwd }))
  on('session.id', () => ({ value: 'S1' }))
  on('session.cwd', () => ({ value: '/repo' }))
  on('tool.register', (_$, e) => {
    seen.tools.push(e.name)
    return { value: { tool: `mcp__wake__${e.name}` } }
  })
  // The engine's own band: what the mod's row stacks under.
  on('ui.render', { component: 'AbovePrompt' }, () => ({ type: 'engine', ref: 0 }))
  on('turn.start', (_$, e) => ({ turnId: e.turnId }))
  on('turn.complete', (_$, e) => ({ text: e.answer }))
  on('prompt.submit', (_$, e) => {
    seen.submitted.push(e)
    return { text: e.text }
  })
  on('command.run', (_$, e) => {
    seen.commands.push(`/${e.command} ${e.args}`)
    return { text: '' }
  })
  on('ui.toast', (_$, e) => {
    seen.toasts.push(e.text)
    return { value: undefined }
  })
  on('ui.status', (_$, e) => {
    seen.status = e.text
    return { value: undefined }
  })
  return seen
}

const met: Wire = { seq: 1, subscription: 'w1', target: PR, until: 'merged', kind: 'met', summary: 'merged', changed: ['state'] }

for (const surface of SURFACES) {
  test(`wake_when subscribes over the daemon's socket and answers at once (${surface})`, async ($, on) => {
    const clock = mock.clock(on)
    const fake = daemon(on, clock)
    const seen = engine(on)
    await $.session.start({ cwd: '/repo', surface, isInteractive: true })
    expect(seen.tools).toEqual(['when'])

    const ran = await $.tool.call({ tool: 'mcp__wake__when', target: 'pr:155', until: 'merged', note: 'tear down' })
    expect(ran.deny).toBeUndefined()
    expect(String(ran.result)).toContain('Subscribed w1')
    expect(String(ran.result)).toContain('End your turn')
    const post = fake.sent.find(one => one.method === 'POST')
    expect(post).toEqual({
      method: 'POST',
      path: '/v1/subscriptions',
      body: { session: 'S1', target: 'pr:155', until: 'merged', dir: '/repo' },
      socketPath: SOCKET,
    })
    expect(seen.status).toBe('watching PR #155')
  })

  test(`an event while idle starts the next turn with the facts (${surface})`, async ($, on) => {
    const clock = mock.clock(on)
    const fake = daemon(on, clock)
    const seen = engine(on)
    await $.session.start({ cwd: '/repo', surface, isInteractive: true })
    await $.tool.call({ tool: 'mcp__wake__when', target: 'pr:155', until: 'merged', note: 'tear down' })
    await clock.settle()
    expect(fake.polls().find(one => one.path.endsWith('timeout=25s'))?.path).toBe('/v1/events?session=S1&after=0&timeout=25s')

    fake.push(met)
    await clock.settle()
    expect(seen.submitted).toHaveLength(1)
    const woken = seen.submitted[0]
    // An engine before 2.1.287 sets no origin on a plugin's own submit; where
    // it does, the wake is framed as the plugin, never as the person.
    if (woken?.origin !== undefined) {
      expect(woken.origin).toMatchObject({ kind: 'plugin', name: 'wake' })
    }
    expect(woken?.text).toContain(`${PR} · until merged · met · merged · changed: state · your note: tear down`)
    expect(seen.toasts).toContain('◉ PR #155 · merged')
    expect(seen.status).toBeUndefined()
  })

  test(`an event during a turn waits for the turn to end (${surface})`, async ($, on) => {
    const clock = mock.clock(on)
    const fake = daemon(on, clock)
    const seen = engine(on)
    await $.session.start({ cwd: '/repo', surface, isInteractive: true })
    await $.tool.call({ tool: 'mcp__wake__when', target: 'pr:155', until: 'merged' })
    await $.turn.start({ text: 'keep going', turnId: 't1' })
    await clock.settle()
    fake.push(met)
    await clock.settle()
    expect(seen.submitted).toHaveLength(0)

    await $.turn.complete({ answer: 'done', durationMs: 1000, isAborted: false, turnId: 't1', reason: 'answer' })
    await clock.settle()
    expect(seen.submitted).toHaveLength(1)
    expect(seen.submitted[0]?.text).toContain(`${PR} · until merged · met`)
  })

  test(`a PR transition draws one band row whose 4 runs /prm (${surface})`, async ($, on) => {
    const clock = mock.clock(on)
    const fake = daemon(on, clock)
    const seen = engine(on)
    await $.session.start({ cwd: '/repo', surface, isInteractive: true })
    await $.tool.call({ tool: 'mcp__wake__when', target: 'pr:155', until: 'ready' })
    await clock.settle()
    fake.push({ seq: 1, subscription: 'w1', target: PR, until: 'ready', kind: 'change', summary: 'checks green · Eve done', changed: ['ci', 'bots'] })
    await clock.settle()
    expect(seen.toasts).toContain('◉ PR #155 · checks green · Eve done')
    expect(seen.submitted).toHaveLength(0) // a change is not the condition: no turn

    const band = await $.ui.mount({ plugin: 'wake', surface, component: 'AbovePrompt', props: BAND })
    expect((await band.find({ type: 'Text', text: /PR #155 · checks green · Eve done/ }))?.text).toBeDefined()
    expect((await band.find({ key: 'wake-prm' }))?.props['hotkey']).toBe('4')
    await band.press({ key: 'wake-prm' })
    expect(seen.commands).toEqual(['/prm 155 in henderson-tech/vybava'])
    expect(await band.find({ key: 'wake-prm' })).toBeUndefined()
    await band.unmount()
  })

  test(`polling stops once no watch is left (${surface})`, async ($, on) => {
    const clock = mock.clock(on)
    const fake = daemon(on, clock)
    engine(on)
    await $.session.start({ cwd: '/repo', surface, isInteractive: true })
    // Nothing watched: one look for a queued wake-up, never a long-poll.
    expect(fake.polls().map(one => one.path)).toEqual(['/v1/events?session=S1&after=0&timeout=0s'])

    await $.tool.call({ tool: 'mcp__wake__when', target: 'pr:155', until: 'merged' })
    await clock.settle()
    fake.push(met)
    await clock.settle()
    const polls = fake.polls().map(one => one.path)
    expect(polls.at(-1)).toBe('/v1/events?session=S1&after=1&timeout=0s') // acknowledges, then stops

    await clock.advance(10 * 60_000)
    expect(fake.polls()).toHaveLength(polls.length)
  })

  test(`a wake-up queued while the mod was unloaded is taken at the next start (${surface})`, async ($, on) => {
    const clock = mock.clock(on)
    const fake = daemon(on, clock)
    const seen = engine(on)
    fake.push(met) // met at the daemon: its subscription is already gone
    await $.session.start({ cwd: '/repo', surface, isInteractive: true })
    await clock.settle()
    expect(seen.submitted).toHaveLength(1)
    expect(seen.submitted[0]?.text).toContain('until merged · met')
    expect(fake.polls().at(-1)?.path).toBe('/v1/events?session=S1&after=1&timeout=0s')
  })

  test(`an error ends the watch at the daemon too (${surface})`, async ($, on) => {
    const clock = mock.clock(on)
    const fake = daemon(on, clock)
    engine(on)
    await $.session.start({ cwd: '/repo', surface, isInteractive: true })
    await $.tool.call({ tool: 'mcp__wake__when', target: 'pr:155', until: 'merged' })
    await clock.settle()
    fake.push({ ...met, kind: 'error', summary: '', changed: [], error: 'gh: rate limited' })
    await clock.settle()
    expect(fake.sent.some(one => one.method === 'DELETE' && one.path === '/v1/subscriptions/w1')).toBe(true)
  })
}
