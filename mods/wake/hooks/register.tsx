import { atom, read, update } from 'claude-code'
import type { EngineInterface, Register, Timer } from 'claude-code'

import type { WakeAlert, WakeEvent, WakeEventKind, WakeUp, WakeWatch } from '../types'

const watches = atom({ plugin: 'wake', key: 'watches' } as const, [] as WakeWatch[])
const after = atom({ plugin: 'wake', key: 'after' } as const, 0)
const handled = atom({ plugin: 'wake', key: 'handled' } as const, [] as number[])
const held = atom({ plugin: 'wake', key: 'held' } as const, [] as WakeUp[])
const alerts = atom({ plugin: 'wake', key: 'alerts' } as const, [] as WakeAlert[])
const busy = atom({ plugin: 'wake', key: 'busy' } as const, false)
const isDown = atom({ plugin: 'wake', key: 'isDown' } as const, false)
const hinted = atom({ plugin: 'wake', key: 'hinted' } as const, false)

/** The daemon's own long-poll slice: it answers within it, events or not. */
const SLICE = '25s'
/** A slice the daemon never answered is abandoned after this. */
const GUARD_MS = 40_000
/** Wait before reaching for a daemon that did not answer. */
const RETRY_MS = 30_000
const MAX_ALERTS = 5
const ENDING: ReadonlySet<WakeEventKind> = new Set(['met', 'error', 'expired'])
const KINDS: ReadonlySet<string> = new Set(['change', 'met', 'error', 'expired'])
const PR = /^pr:(.+)#(\d+)$/

const DESCRIPTION = [
  'Wake this session when a PR, CI run, Eve review, devbox, vitrinka task or deployik deploy reaches a state,',
  'instead of a sleep/until loop or a Monitor armed only to wait for it. Subscribe, then END YOUR TURN:',
  'one shared poller (vybava watch) watches it, and a prompt from the wake plugin starts your next turn when',
  'the condition holds, or the watch errors or expires.',
  'Targets: pr:<n> (this repo), pr:<owner>/<name>#<n>, devbox:<box>, devbox-run:<workspace>,',
  'vitrinka:<workspace>/<id>, deployik:<slug>[/<env>].',
  'Conditions: pr: merged, closed, checks-settled (green OR red - use it to wait for CI), checks-green,',
  'checks-red, eve-approved, ready; devbox: up, down; devbox-run: idle, running; vitrinka: done, closed;',
  'deployik: live, failed, building, settled; any target: changed, <field>=<value>.',
  'Not a replacement for /prm\'s `vybava gitkit pr-events` Monitor, which also carries review comments; keep it.',
  'It wakes the main session only - a subagent must not call it.',
].join(' ')

const SCHEMA = {
  type: 'object',
  properties: {
    target: { type: 'string', description: 'What to watch, e.g. pr:155, devbox-run:my-ws, vitrinka:fixit/4759' },
    until: { type: 'string', description: 'The condition that wakes you, e.g. checks-settled, merged, idle, done' },
    note: { type: 'string', description: 'Optional: what you will do when woken; echoed back in the wake prompt' },
  },
  required: ['target', 'until'],
  additionalProperties: false,
}

// The poll loop's handles live with the module: a reload drops its timers,
// and session.start arms a fresh loop from the daemon's own list.
let timer: Timer | null = null
let isPolling = false

export const register: Register = on => {
  on('session.start', async ($, e, next) => {
    timer = null
    isPolling = false
    await $.tool.register({ name: 'when', description: DESCRIPTION, inputSchema: SCHEMA })
    await sync($)
    return next(e)
  })

  // /clear and /resume start a new conversation without session.start; the
  // watches of the old one stay with the daemon until their TTL.
  on('classic.SessionStart', async ($, e, next) => {
    const ran = await next(e)
    if (e.source === 'clear' || e.source === 'resume') {
      await sync($)
    }
    return ran
  })

  on('tool.call', { tool: 'mcp__wake__when' }, async ($, e) => {
    const target = e['target']
    const until = e['until']
    const note = e['note']
    if (typeof target !== 'string' || target.trim() === '' || typeof until !== 'string' || until.trim() === '') {
      return { deny: 'wake: `target` and `until` are required strings (e.g. target "pr:155", until "checks-settled").' }
    }
    const want = { target: target.trim(), until: until.trim(), note: typeof note === 'string' && note.trim() !== '' ? note.trim() : null }
    const session = await $.session.id()
    const dir = await $.session.cwd()
    const reply = await call($, 'POST', '/v1/subscriptions', { session, target: want.target, until: want.until, dir })
    if (reply.kind === 'down') {
      await markDown($, reply.reason)
      if (!(await read($, hinted))) {
        await update($, hinted, () => true)
        $.ui.status('the watch daemon is not running — `vybava watch agent install`')
      }
      return {
        deny:
          `wake: the watch daemon is not running (${reply.reason}). Wait with a Monitor instead: ` +
          `\`vybava watch until ${want.target} ${want.until} --timeout 2h\` (it probes directly). ` +
          'The human can install the daemon with `vybava watch agent install`.',
      }
    }
    await markUp($)
    if (reply.status !== 201) {
      return { deny: `wake: ${errorOf(reply.json) ?? `the daemon answered ${reply.status}`}` }
    }
    const added = parseAdded(reply.json)
    if (added === null) {
      $.ui.log(`unexpected subscribe answer: ${reply.text.slice(0, 200)}`, { to: 'debug' })
      $.ui.log('the watch daemon answered a subscribe it could not read; see the debug log')
      return { deny: 'wake: the watch daemon answered in a shape wake cannot read (mod and daemon versions differ?).' }
    }
    const seqs = added.events.map(ev => ev.seq)
    if (seqs.length > 0) {
      await update($, handled, list => [...list, ...seqs])
    }
    const met = added.events.find(ev => ev.kind === 'met')
    if (met !== undefined) {
      $.ui.toast(`◉ ${label(met.target)} · ${met.summary || `already ${met.until}`}`)
      return { result: `Already ${want.until}: ${label(met.target)} — ${met.summary || 'condition holds now'}. Nothing to wait for; carry on.` }
    }
    const watch: WakeWatch = { id: added.id, target: added.target, until: want.until, note: want.note }
    await update($, watches, list => [...list.filter(one => one.id !== watch.id), watch])
    await refreshStatus($)
    arm($, 0)
    return {
      result:
        `Subscribed ${watch.id}: this session will be woken when ${label(watch.target)} is ${watch.until}. ` +
        'End your turn now; do not poll, sleep or arm a Monitor for it.',
    }
  })

  on('turn.start', async ($, e, next) => {
    await update($, busy, () => true)
    return next(e)
  })

  on('turn.complete', async ($, e, next) => {
    const ran = await next(e)
    if (e.agentId !== undefined) {
      return ran
    }
    await update($, busy, () => false)
    const waiting = await read($, held)
    if (waiting.length > 0) {
      await update($, held, () => [])
      submit($, waiting)
    }
    return ran
  })

  on('ui.render', { component: 'AbovePrompt' }, async ($, e, next) => {
    const list = await read($, alerts)
    const top = list.at(-1)
    if (e.props.hasSurvey || top === undefined) {
      return next(e)
    }
    const theirs = await next(e)
    const { Box, Text, Button } = $.ui.resolve(e)
    const more = list.length > 1 ? ` (+${list.length - 1} more)` : ''
    return (
      <Box flexDirection="column">
        {theirs}
        <Box flexDirection="row" gap={1}>
          <Text wrap="truncate-end">
            ◉ {top.line}
            {more}
          </Text>
          <Button key="wake-prm" label="/prm" hotkey="4" onPress={() => runPrm($, top)} />
          <Button key="wake-dismiss" label="dismiss" hotkey="5" onPress={() => update($, alerts, () => [])} />
        </Box>
      </Box>
    )
  })
}

/** Re-reads this session's watches from the daemon and (re)arms the poll. */
async function sync($: EngineInterface): Promise<void> {
  const session = await $.session.id()
  const reply = await call($, 'GET', `/v1/subscriptions?session=${encodeURIComponent(session)}`)
  if (reply.kind === 'down') {
    await markDown($, reply.reason)
    if ((await read($, watches)).length > 0) {
      arm($, RETRY_MS)
    }
    return
  }
  await markUp($)
  const listed = reply.status === 200 ? parseListed(reply.json) : null
  if (listed === null) {
    $.ui.log(`could not read this session's watches (${reply.status}): ${reply.text.slice(0, 200)}`, { to: 'debug' })
    return
  }
  const known = await read($, watches)
  await update($, watches, () =>
    listed.map(one => ({ ...one, note: known.find(old => old.id === one.id)?.note ?? null })),
  )
  await refreshStatus($)
  if (listed.length > 0) {
    arm($, 0)
  }
}

/** Schedules the next long-poll slice unless one is due or running. */
function arm($: EngineInterface, delayMs: number): void {
  if (timer !== null || isPolling) {
    return
  }
  timer = $.clock.after(delayMs, () => {
    timer = null
    void poll($)
  })
}

/** Runs one slice, then re-arms while watches remain, else stops. */
async function poll($: EngineInterface): Promise<void> {
  if (isPolling) {
    return
  }
  isPolling = true
  let retry: number
  try {
    retry = await slice($)
  } catch (err) {
    $.ui.log(`poll failed: ${messageOf(err)}`, { to: 'debug' })
    retry = RETRY_MS
  } finally {
    isPolling = false
  }
  if ((await read($, watches)).length > 0) {
    arm($, retry)
    return
  }
  // Nothing left to watch: acknowledge the last batch so a later watch
  // never sees it again, and stop.
  const session = await $.session.id()
  const seq = await read($, after)
  if (seq > 0) {
    await call($, 'GET', `/v1/events?session=${encodeURIComponent(session)}&after=${seq}&timeout=0s`)
  }
}

/** One long-poll: acknowledge what was handled, wait up to SLICE for more.
 * Answers the delay before the next slice. */
async function slice($: EngineInterface): Promise<number> {
  if ((await read($, watches)).length === 0) {
    return 0
  }
  const session = await $.session.id()
  const seq = await read($, after)
  const reply = await call($, 'GET', `/v1/events?session=${encodeURIComponent(session)}&after=${seq}&timeout=${SLICE}`)
  if (reply.kind === 'down') {
    await markDown($, reply.reason)
    return RETRY_MS
  }
  await markUp($)
  const events = reply.status === 200 ? parseEvents(reply.json) : null
  if (events === null) {
    $.ui.log(`unreadable events answer (${reply.status}): ${reply.text.slice(0, 200)}`, { to: 'debug' })
    return RETRY_MS
  }
  await deliver($, events)
  return 0
}

/** Takes one answered batch: advance the watermark, react to what is new. */
async function deliver($: EngineInterface, events: readonly WakeEvent[]): Promise<void> {
  if (events.length === 0) {
    return
  }
  const seen = await read($, after)
  const skip = await read($, handled)
  const fresh = events.filter(ev => ev.seq > seen && !skip.includes(ev.seq)).sort((a, b) => a.seq - b.seq)
  const top = Math.max(seen, ...events.map(ev => ev.seq))
  await update($, after, n => Math.max(n, top))
  await update($, handled, list => list.filter(seq => seq > top))
  if (fresh.length > 0) {
    await react($, fresh)
  }
}

/** Toasts every transition, ends finished watches, wakes the model on them. */
async function react($: EngineInterface, fresh: readonly WakeEvent[]): Promise<void> {
  const known = await read($, watches)
  const rows: WakeAlert[] = []
  for (const ev of fresh) {
    $.ui.toast(`◉ ${label(ev.target)} · ${ev.kind === 'error' ? `error: ${ev.error}` : ev.summary || ev.kind}`)
    const pr = PR.exec(ev.target)
    if (pr !== null && ev.kind !== 'expired') {
      rows.push({ seq: ev.seq, target: ev.target, prm: `${pr[2]} in ${pr[1]}`, line: `${label(ev.target)} · ${ev.summary || ev.kind}` })
    }
  }
  if (rows.length > 0) {
    await update($, alerts, list =>
      [...list.filter(one => !rows.some(row => row.target === one.target)), ...rows].slice(-MAX_ALERTS),
    )
  }
  const ending: WakeUp[] = fresh
    .filter(ev => ENDING.has(ev.kind))
    .map(ev => ({ ...ev, note: known.find(one => one.id === ev.subscription)?.note ?? null }))
  if (ending.length > 0) {
    await update($, watches, list => list.filter(one => !ending.some(ev => ev.subscription === one.id)))
    if (await read($, busy)) {
      await update($, held, list => [...list, ...ending])
    } else {
      submit($, ending)
    }
  }
  await refreshStatus($)
}

/** Starts the model's next turn with the facts, framed as this plugin. */
function submit($: EngineInterface, events: readonly WakeUp[]): void {
  const lines = events.map(ev => {
    const parts = [`- ${ev.target} · until ${ev.until} · ${ev.kind}`]
    if (ev.summary !== '') {
      parts.push(ev.summary)
    }
    if (ev.changed.length > 0) {
      parts.push(`changed: ${ev.changed.join(', ')}`)
    }
    if (ev.error !== '') {
      parts.push(`error: ${ev.error}`)
    }
    if (ev.note !== null) {
      parts.push(`your note: ${ev.note}`)
    }
    return parts.join(' · ')
  })
  const text = ['A watch you subscribed to with mcp__wake__when has settled:', ...lines].join('\n')
  $.prompt.submit({ text }).catch(err => {
    $.ui.log(`could not start the woken turn: ${messageOf(err)}`)
  })
}

async function runPrm($: EngineInterface, alert: WakeAlert): Promise<void> {
  await update($, alerts, list => list.filter(one => one.seq !== alert.seq))
  try {
    await $.command.run({ command: 'prm', args: alert.prm })
  } catch (err) {
    $.ui.log(`/prm did not run as a command (${messageOf(err)}); filling the prompt`, { to: 'debug' })
    const filled = await $.prompt.fill({ text: `/prm ${alert.prm}` })
    $.ui.toast(filled.isFilled ? '/prm is in the prompt — press Enter' : `run /prm ${alert.prm}`)
  }
}

async function refreshStatus($: EngineInterface): Promise<void> {
  const list = await read($, watches)
  const down = await read($, isDown)
  if (list.length > 0) {
    const names = list.map(one => label(one.target))
    $.ui.status(`watching ${names.join(', ')}${down ? ' — watch daemon unreachable, retrying' : ''}`)
    return
  }
  // Down with nothing watched: leave whatever is shown (the install hint a
  // refused subscribe put up), so a session that never subscribes stays quiet.
  if (!down) {
    $.ui.status(undefined)
  }
}

async function markDown($: EngineInterface, reason: string): Promise<void> {
  if (!(await read($, isDown))) {
    $.ui.log(`watch daemon unreachable: ${reason}`, { to: 'debug' })
  }
  await update($, isDown, () => true)
  await refreshStatus($)
}

async function markUp($: EngineInterface): Promise<void> {
  if (await read($, isDown)) {
    await update($, isDown, () => false)
    await refreshStatus($)
  }
}

type Reply = { kind: 'ok'; status: number; text: string; json: unknown } | { kind: 'down'; reason: string }

/** One request to the daemon over its socket, never waiting past GUARD_MS. */
async function call($: EngineInterface, method: string, path: string, body?: unknown): Promise<Reply> {
  const home = await $.env.get('HOME')
  if (home === undefined || home === '') {
    return { kind: 'down', reason: 'HOME is not set' }
  }
  const socketPath = `${home}/.local/state/vybava/watch/watchd.sock`
  const guard: { timer?: Timer } = {}
  const expired = new Promise<null>(resolve => {
    guard.timer = $.clock.after(GUARD_MS, () => resolve(null))
  })
  try {
    const init = body === undefined
      ? { method, socketPath }
      : { method, socketPath, headers: { 'content-type': 'application/json' }, body: JSON.stringify(body) }
    const res = await Promise.race([$.http.fetch(`http://watchd${path}`, init), expired])
    if (res === null) {
      return { kind: 'down', reason: `no answer in ${GUARD_MS / 1000}s` }
    }
    return { kind: 'ok', status: res.status, text: res.text, json: parseJson(res.text) }
  } catch (err) {
    return { kind: 'down', reason: messageOf(err) }
  } finally {
    guard.timer?.cancel()
  }
}

function parseJson(text: string): unknown {
  try {
    return JSON.parse(text)
  } catch {
    return undefined
  }
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function stringOf(record: Record<string, unknown>, key: string): string {
  const value = record[key]
  return typeof value === 'string' ? value : ''
}

function parseEvent(value: unknown): WakeEvent | null {
  if (!isRecord(value) || typeof value['seq'] !== 'number' || !KINDS.has(stringOf(value, 'kind'))) {
    return null
  }
  const changed = value['changed']
  return {
    seq: value['seq'],
    subscription: stringOf(value, 'subscription'),
    target: stringOf(value, 'target'),
    until: stringOf(value, 'until'),
    kind: stringOf(value, 'kind') as WakeEventKind,
    summary: stringOf(value, 'summary'),
    changed: Array.isArray(changed) ? changed.filter((one): one is string => typeof one === 'string') : [],
    error: stringOf(value, 'error'),
  }
}

/** `{events: [...]}`; null when the shape is not that (never a silent empty). */
function parseEvents(json: unknown): WakeEvent[] | null {
  if (!isRecord(json) || !Array.isArray(json['events'])) {
    return null
  }
  const events = json['events'].map(parseEvent)
  return events.every((one): one is WakeEvent => one !== null) ? events : null
}

function parseAdded(json: unknown): { id: string; target: string; events: WakeEvent[] } | null {
  if (!isRecord(json) || !isRecord(json['subscription'])) {
    return null
  }
  const id = stringOf(json['subscription'], 'id')
  const target = stringOf(json['subscription'], 'target')
  const events = parseEvents({ events: json['events'] ?? [] })
  return id === '' || target === '' || events === null ? null : { id, target, events }
}

function parseListed(json: unknown): Array<Omit<WakeWatch, 'note'>> | null {
  if (!isRecord(json) || !Array.isArray(json['subscriptions'])) {
    return null
  }
  const out: Array<Omit<WakeWatch, 'note'>> = []
  for (const one of json['subscriptions']) {
    if (!isRecord(one) || stringOf(one, 'id') === '') {
      return null
    }
    out.push({ id: stringOf(one, 'id'), target: stringOf(one, 'target'), until: stringOf(one, 'until') })
  }
  return out
}

function errorOf(json: unknown): string | null {
  return isRecord(json) && typeof json['error'] === 'string' ? json['error'] : null
}

/** `pr:owner/name#155` → `PR #155`; other targets as given. */
function label(target: string): string {
  const pr = PR.exec(target)
  return pr === null ? target : `PR #${pr[2]}`
}

function messageOf(err: unknown): string {
  return err instanceof Error ? err.message : String(err)
}
