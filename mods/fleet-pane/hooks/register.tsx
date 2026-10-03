import { atom, read, update } from 'claude-code'
import type { EngineInterface, Register, RenderSurface, Timer } from 'claude-code'

import type { FleetPaneCodex, FleetPaneSession, FleetView } from '../types'
import type { CodexRow, Envelope, FleetSession, FleetSnapshot, FleetSummary } from '../types/fleet.gen'

const view = atom({ plugin: 'fleet-pane', key: 'view' } as const, null as FleetView | null)
const codex = atom({ plugin: 'fleet-pane', key: 'codex' } as const, [] as FleetPaneCodex[])
const problem = atom({ plugin: 'fleet-pane', key: 'problem' } as const, null as string | null)
const replyTo = atom({ plugin: 'fleet-pane', key: 'replyTo' } as const, null as string | null)
const notice = atom({ plugin: 'fleet-pane', key: 'notice' } as const, null as string | null)
const self = atom({ plugin: 'fleet-pane', key: 'self' } as const, null as string | null)

const PANE = 'fleet'
const FAST_MS = 5_000
const CODEX_MS = 60_000
const STATUS_MS = 30_000
const STALE_MS = 2 * 60_000
const RUN_TIMEOUT_MS = 20_000
// The row's action buttons, drawn plain: "y: reply  c: copy".
const ACTIONS_WIDTH = 18
// "r: refresh  x: close".
const HEADER_ACTIONS_WIDTH = 22
const PROJECT_WIDTH = 12
const DETAIL_WIDTH = 14
const AGE_WIDTH = 6

// The pane's refresh timers run only while it is open; a reload drops them
// with the module, and session.start restarts them for a pane still open.
let fast: Timer | null = null
let slow: Timer | null = null
const inFlight = new Set<'fast' | 'codex'>()

// fleet draws what `vybava fleet --json` says and never decides it: grouping
// and liveness are the Go applet's. The status line reads the summary file
// the watch daemon publishes, so no session spawns a process on a timer.
export const register: Register = on => {
  on('session.start', async ($, e, next) => {
    const started = await next(e)
    await $.command.register({
      name: 'fleet',
      description: 'Every Claude session on this Mac, waiting on you first; reply or copy a resume line',
      immediate: true,
    })
    const id = await $.session.id()
    await update($, self, () => id)
    $.clock.every(STATUS_MS, () => {
      void refreshStatus($)
    })
    void refreshStatus($)
    const panes = await $.ui.panes()
    if (panes.some(pane => pane.id === PANE)) {
      startRefresh($)
    }
    return started
  })

  // /clear and /resume give the session a new id without a session.start.
  on('classic.SessionStart', async ($, e, next) => {
    const ran = await next(e)
    await update($, self, () => e.session_id)
    return ran
  })

  on('command.run', { command: 'fleet' }, async $ => {
    const opened = await $.ui.open({ id: PANE, title: 'Fleet', focus: true, closeOnEscape: true })
    if (!opened.isPlaced) {
      $.ui.toast(`the pane is waiting for room (${opened.reason})`)
    }
    startRefresh($)
    return {}
  })

  on('ui.close', { id: PANE }, async ($, e, next) => {
    stopRefresh()
    await update($, replyTo, () => null)
    return next(e)
  })

  on('ui.render', { component: 'Pane', requestId: PANE }, async ($, e) => {
    const table = $.ui.resolve(e)
    const { Box, Text, Button } = table
    const width = Math.max(40, e.props.bodyColumns)
    const snapshot = await read($, view)
    const codexRows = await read($, codex)
    const failure = await read($, problem)
    const replying = await read($, replyTo)
    const outcome = await read($, notice)
    const me = await read($, self)

    if (snapshot === null) {
      return (
        <Box flexDirection="column">
          <Text color={failure === null ? undefined : 'red'} wrap="truncate-end">
            {failure ?? 'Reading the fleet…'}
          </Text>
        </Box>
      )
    }

    const groups = groupSessions(snapshot.sessions)
    const waitingOnYou = groups.waiting.filter(one => one.sessionId !== me).length
    const first = groups.waiting.find(one => one.sessionId !== me)
    const textWidth = width - ACTIONS_WIDTH

    const row = (session: FleetPaneSession) => {
      const isSelf = session.sessionId === me
      const isDead = session.state === 'dead'
      const isFirst = first !== undefined && session.sessionId === first.sessionId
      const target = session
      return (
        <Box key={`row:${session.sessionId}`} flexDirection="column">
          <Box flexDirection="row">
            <Box width={textWidth}>
              <Text wrap="truncate-end" dimColor={isSelf}>
                {sessionLine(session, isSelf, textWidth)}
              </Text>
            </Box>
            <Box flexDirection="row" gap={2}>
              {!isSelf && !isDead && (
                <Button
                  key={`reply:${session.sessionId}`}
                  label="reply"
                  plain
                  dimColor={!isFirst}
                  {...(isFirst ? { hotkey: 'y' } : {})}
                  onPress={() => {
                    void openReply($, target.sessionId)
                  }}
                />
              )}
              <Button
                key={`copy:${session.sessionId}`}
                label="copy"
                plain
                dimColor={!isFirst}
                {...(isFirst ? { hotkey: 'c' } : {})}
                onPress={press => {
                  void copyResume($, target, press.surface)
                }}
              />
            </Box>
          </Box>
          {replying === session.sessionId && 'Input' in table && (
            <Box flexDirection="column" paddingLeft={2}>
              <table.Input
                key="reply-text"
                label={`reply to ${displayName(session)}: `}
                placeholder="type, Enter sends · Esc returns"
                submitLabel="send"
                autoFocus
                onSubmit={value => {
                  void sendReply($, target, value)
                }}
              />
              <Text dimColor wrap="truncate-end">
                {session.state === 'busy'
                  ? 'it is busy: the reply lands inside its running turn'
                  : 'the reply arrives as a message from the fleet mod'}
              </Text>
              <Button
                key="reply-cancel"
                label="cancel"
                plain
                dimColor
                onPress={() => {
                  void update($, replyTo, () => null)
                }}
              />
            </Box>
          )}
          {replying === session.sessionId && !('Input' in table) && (
            <Text dimColor wrap="truncate-end">
              this surface has no text field: reply from the terminal or desktop
            </Text>
          )}
        </Box>
      )
    }

    const idleParts = [`idle · ${groups.idle.length}`]
    if (snapshot.counts.shell > 0) {
      idleParts.push(`${snapshot.counts.shell} at a shell`)
    }
    if (snapshot.counts.ended + snapshot.counts.unknown > 0) {
      idleParts.push(`${snapshot.counts.ended + snapshot.counts.unknown} ended or unknown`)
    }

    return (
      <Box flexDirection="column">
        <Box flexDirection="row">
          <Box width={width - HEADER_ACTIONS_WIDTH}>
            <Text bold wrap="truncate-end">
              {`Fleet · ${snapshot.counts.total} sessions · ${waitingOnYou} waiting on you`}
            </Text>
          </Box>
          <Box flexDirection="row" gap={2}>
            <Button
              key="refresh"
              label="refresh"
              plain
              hotkey="r"
              onPress={() => {
                void refresh($, true)
              }}
            />
            <Button
              key="close"
              label="close"
              plain
              role="dismiss"
              hotkey="x"
              onPress={() => {
                void closePane($)
              }}
            />
          </Box>
        </Box>
        {failure !== null && (
          <Text color="red" wrap="truncate-end">
            {failure}
          </Text>
        )}
        {outcome !== null && (
          <Text dimColor wrap="truncate-end">
            {outcome}
          </Text>
        )}
        <Text dimColor>{rule(`waiting on you · ${groups.waiting.length}`, width)}</Text>
        {groups.waiting.length === 0 && <Text dimColor>  nobody is waiting on you</Text>}
        {groups.waiting.map(row)}
        {groups.dead.length > 0 && <Text dimColor>{rule(`died while busy · ${groups.dead.length}`, width)}</Text>}
        {groups.dead.map(row)}
        <Text dimColor>{rule(`working · ${groups.working.length}`, width)}</Text>
        {groups.working.map(row)}
        <Text dimColor>{rule(idleParts.join(' · '), width)}</Text>
        {codexRows.length > 0 && <Text dimColor>{rule(`codex (read-only) · ${codexRows.length}`, width)}</Text>}
        {codexRows.map(one => (
          <Text dimColor wrap="truncate-end">
            {codexLine(one, snapshot.generatedAt, width)}
          </Text>
        ))}
      </Box>
    )
  })
}

function startRefresh($: EngineInterface): void {
  if (fast !== null) {
    return
  }
  void refresh($, true)
  fast = $.clock.every(FAST_MS, () => {
    void refresh($, false)
  })
  slow = $.clock.every(CODEX_MS, () => {
    void refresh($, true)
  })
}

// closePane stops the timers itself: the ui.close hook covers the person's
// close and Escape, and this covers our own button whatever the chain does.
async function closePane($: EngineInterface): Promise<void> {
  stopRefresh()
  await update($, replyTo, () => null)
  await $.ui.close({ id: PANE })
}

function stopRefresh(): void {
  fast?.cancel()
  slow?.cancel()
  fast = null
  slow = null
}

// refresh runs `vybava fleet --json`; the Codex rows (lsof per process) only
// on the slow beat. A failure is logged and drawn, never swallowed.
async function refresh($: EngineInterface, withCodex: boolean): Promise<void> {
  const kind = withCodex ? 'codex' : 'fast'
  if (inFlight.has(kind)) {
    return
  }
  inFlight.add(kind)
  try {
    const argv = withCodex ? ['vybava', 'fleet', '--json'] : ['vybava', 'fleet', '--json', '--no-codex']
    let ran
    try {
      ran = await $.process.run(argv, { timeoutMs: RUN_TIMEOUT_MS })
    } catch (error) {
      await fail($, `could not run \`vybava fleet\`: ${messageOf(error)}`)
      return
    }
    const envelope = parseEnvelope(ran.stdout)
    if (envelope === null || !envelope.ok || envelope.data === undefined) {
      await fail($, `\`vybava fleet\` failed (exit ${ran.exitCode}): ${reasonOf(envelope, ran.stderr)}`)
      return
    }
    const data = envelope.data
    await update($, view, () => ({ generatedAt: data.generatedAt, counts: data.counts, sessions: data.sessions.map(paneSession) }))
    if (withCodex) {
      await update($, codex, () => data.codex.map(paneCodex))
    }
    await update($, problem, () => null)
  } finally {
    inFlight.delete(kind)
  }
}

async function fail($: EngineInterface, text: string): Promise<void> {
  $.ui.log(text, { to: 'debug' })
  await update($, problem, () => text)
}

// refreshStatus reads the published summary: no file, no line (the daemon
// is not installed); an old file says so instead of showing old counts.
async function refreshStatus($: EngineInterface): Promise<void> {
  const home = await $.env.get('HOME')
  if (home === undefined || home === '') {
    $.ui.log('HOME is unset; no summary to read', { to: 'debug' })
    return
  }
  const path = `${home}/.local/state/vybava/fleet/summary.json`
  if (!(await $.fs.exists(path))) {
    $.ui.status(undefined)
    return
  }
  let summary: FleetSummary
  try {
    summary = parseSummary(await $.fs.read(path))
  } catch (error) {
    $.ui.log(`${path} is unreadable: ${messageOf(error)}`, { to: 'debug' })
    $.ui.status('fleet summary unreadable · /fleet')
    return
  }
  const age = (await $.clock.now()) - Date.parse(summary.generatedAt)
  if (!(age <= STALE_MS)) {
    $.ui.status('fleet summary stale (is vybava.watchd running?)')
    return
  }
  const me = await read($, self)
  const waiting = summary.waiting.filter(one => one.sessionId !== me).length
  $.ui.status(waiting > 0 ? `${waiting} waiting on you · /fleet` : undefined)
}

async function openReply($: EngineInterface, sessionId: string): Promise<void> {
  await update($, notice, () => null)
  await update($, replyTo, () => sessionId)
}

// sendReply delivers what the person typed and pressed, nothing else.
async function sendReply($: EngineInterface, session: FleetPaneSession, value: string): Promise<void> {
  const text = value.trim()
  if (text === '') {
    await update($, notice, () => 'nothing sent: the reply was empty')
    return
  }
  const name = displayName(session)
  let outcome: string
  try {
    const sent = await $.session.send({ to: { sessionId: session.sessionId }, text })
    outcome = sent.isDelivered ? `sent to ${name}` : `not delivered to ${name}: ${sent.reason}`
  } catch (error) {
    outcome = `not delivered to ${name}: ${messageOf(error)}`
    $.ui.log(outcome, { to: 'debug' })
  }
  await update($, replyTo, () => null)
  await update($, notice, () => outcome)
}

async function copyResume($: EngineInterface, session: FleetPaneSession, surface: RenderSurface): Promise<void> {
  const copied = await $.ui.copy({ text: session.resume, surface })
  const outcome = copied.isCopied
    ? `copied: ${session.resume}`
    : `copy failed (${copied.reason}): ${session.resume}`
  await update($, notice, () => outcome)
}

type Groups = { waiting: FleetPaneSession[]; dead: FleetPaneSession[]; working: FleetPaneSession[]; idle: FleetPaneSession[] }

function groupSessions(sessions: readonly FleetPaneSession[]): Groups {
  const groups: Groups = { waiting: [], dead: [], working: [], idle: [] }
  for (const session of sessions) {
    if (session.state === 'waiting') {
      groups.waiting.push(session)
    } else if (session.state === 'dead') {
      groups.dead.push(session)
    } else if (session.state === 'busy') {
      groups.working.push(session)
    } else {
      groups.idle.push(session)
    }
  }
  groups.waiting.sort((a, b) => b.ageSeconds - a.ageSeconds)
  return groups
}

function sessionLine(session: FleetPaneSession, isSelf: boolean, width: number): string {
  const mark = session.state === 'waiting' ? '▸' : session.state === 'dead' ? '✕' : ' '
  const name = `${displayName(session)}${isSelf ? ' (you)' : ''}`
  const detail = detailOf(session)
  const age = ageText(session.ageSeconds).padStart(AGE_WIDTH)
  const fixed = 2 + PROJECT_WIDTH + 1 + 1 + DETAIL_WIDTH + 1 + AGE_WIDTH
  const nameWidth = width - fixed
  if (nameWidth < 8) {
    return fit(`${mark} ${session.project} ${name} ${age.trim()}`, width)
  }
  return `${mark} ${fit(session.project, PROJECT_WIDTH)} ${fit(name, nameWidth)} ${fit(detail, DETAIL_WIDTH)} ${age}`
}

function detailOf(session: FleetPaneSession): string {
  if (session.state === 'waiting') {
    return session.waitingFor ?? 'waiting'
  }
  if (session.state === 'dead') {
    return session.openJobs > 0 ? `died · ${session.openJobs} jobs` : 'died while busy'
  }
  return session.openJobs > 0 ? `${session.state} · ${session.openJobs} bg` : session.state
}

function codexLine(row: FleetPaneCodex, generatedAt: string, width: number): string {
  const name = row.name ?? row.branch ?? row.threadId.slice(0, 8)
  const last = row.lastCallAt === undefined ? '' : `last call ${ageText(Math.max(0, (Date.parse(generatedAt) - Date.parse(row.lastCallAt)) / 1000))} ago`
  return fit(`  ${fit(row.project, PROJECT_WIDTH)} ${name} · ${row.calls} calls ${last}`.trimEnd(), width)
}

// paneSession and paneCodex keep what the pane draws; their parameter types
// are the generated Go contract, so a drift there fails tsc here.
function paneSession(session: FleetSession): FleetPaneSession {
  const { sessionId, name, project, state, waitingFor, ageSeconds, openJobs, resume } = session
  return { sessionId, name, project, state, waitingFor, ageSeconds, openJobs, resume }
}

function paneCodex(row: CodexRow): FleetPaneCodex {
  const { threadId, name, branch, project, calls, lastCallAt } = row
  return { threadId, name, branch, project, calls, lastCallAt }
}

function displayName(session: FleetPaneSession): string {
  return session.name ?? session.sessionId.slice(0, 8)
}

function rule(title: string, width: number): string {
  const head = `── ${title} `
  return head.length >= width ? fit(head, width) : head + '─'.repeat(width - head.length)
}

function fit(text: string, width: number): string {
  if (text.length <= width) {
    return text.padEnd(width)
  }
  return width <= 1 ? text.slice(0, width) : `${text.slice(0, width - 1)}…`
}

function ageText(seconds: number): string {
  const minutes = Math.floor(seconds / 60)
  if (minutes < 1) {
    return `${Math.floor(seconds)}s`
  }
  if (minutes < 60) {
    return `${minutes}m`
  }
  const hours = Math.floor(minutes / 60)
  if (hours < 48) {
    const rest = minutes % 60
    return rest === 0 ? `${hours}h` : `${hours}h${rest}m`
  }
  return `${Math.floor(hours / 24)}d`
}

function parseEnvelope(stdout: string): Envelope<FleetSnapshot> | null {
  let parsed: unknown
  try {
    parsed = JSON.parse(stdout)
  } catch {
    return null
  }
  if (typeof parsed !== 'object' || parsed === null || !('ok' in parsed) || typeof parsed.ok !== 'boolean' || !('diagnostics' in parsed) || !Array.isArray(parsed.diagnostics)) {
    return null
  }
  // The shape past these keys is the Go contract, held by fleet.gen.d.ts's drift test.
  return parsed as Envelope<FleetSnapshot>
}

function parseSummary(text: string): FleetSummary {
  const parsed: unknown = JSON.parse(text)
  if (typeof parsed !== 'object' || parsed === null || !('generatedAt' in parsed) || typeof parsed.generatedAt !== 'string' || !('waiting' in parsed) || !Array.isArray(parsed.waiting)) {
    throw new Error('not a fleet summary')
  }
  return parsed as FleetSummary
}

function reasonOf(envelope: Envelope<FleetSnapshot> | null, stderr: string): string {
  const diagnostic = envelope?.diagnostics.find(one => one.severity === 'error') ?? envelope?.diagnostics[0]
  if (diagnostic !== undefined) {
    return `${diagnostic.code}: ${diagnostic.detail}`
  }
  const line = stderr.trim().split('\n')[0]
  return line === undefined || line === '' ? 'no JSON on stdout' : line
}

function messageOf(error: unknown): string {
  return error instanceof Error ? error.message : String(error)
}
