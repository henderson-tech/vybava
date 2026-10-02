import { atom, read, update } from 'claude-code'
import type { Register, ToolCallInput, TurnUsage } from 'claude-code'

import type { PeekCall, PeekToolTime } from '../types'

const calls = atom({ plugin: 'peek', key: 'calls' } as const, [] as PeekCall[])
const background = atom({ plugin: 'peek', key: 'background' } as const, [] as string[])
const turn = atom({ plugin: 'peek', key: 'turn' } as const, [] as PeekToolTime[])
const tick = atom({ plugin: 'peek', key: 'tick' } as const, 0)

const TICK_MS = 30_000
const LONG_TURN_MS = 30 * 60_000
const LABEL_MAX = 48
const TASK_ID = /<task-id>([^<]+)<\/task-id>/
const AGENT_TOOLS = new Set(['Agent', 'Task'])

// peek only watches: every hook passes its event on unchanged, and the one
// rewrite is the spinner's suffix. Nothing here spawns a process.
export const register: Register = on => {
  on('session.start', async ($, e, next) => {
    // A reload drops the hooks that would have closed the calls in flight.
    await update($, calls, () => [])
    $.clock.every(TICK_MS, () => {
      void update($, tick, n => n + 1)
    })
    return next(e)
  })

  on('turn.start', async ($, e, next) => {
    await update($, turn, () => [])
    return next(e)
  })

  on('tool.call', async ($, e, next) => {
    const call: PeekCall = {
      id: e.tool_use_id,
      tool: e.tool,
      label: labelOf(e),
      startedAt: await $.clock.now(),
      agentId: e.agentId ?? null,
    }
    await update($, calls, list => [...list, call])
    try {
      const ran = await next(e)
      const taskId = ran.deny === undefined ? backgroundTaskId(ran.result) : null
      if (taskId !== null) {
        await update($, background, ids => [...ids, taskId])
      }
      if (call.agentId === null) {
        const elapsed = (await $.clock.now()) - call.startedAt
        await update($, turn, times => addTime(times, call.tool, elapsed))
      }
      return ran
    } finally {
      await update($, calls, list => list.filter(one => one.id !== call.id))
    }
  })

  on('prompt.submit', async ($, e, next) => {
    const done = e.origin.kind === 'task-notification' ? TASK_ID.exec(e.text)?.[1] : undefined
    if (done !== undefined) {
      await update($, background, ids => ids.filter(id => id !== done))
    }
    return next(e)
  })

  // The turn's end carries the engine's own list of background work: the
  // authoritative count, so a task that ended without a notification (stopped,
  // or folded into a running turn) never lingers as "1 bg".
  on('classic.Stop', async ($, e, next) => {
    const ran = await next(e)
    if (e.background_tasks !== undefined) {
      const shells = e.background_tasks.filter(task => task.type === 'shell').map(task => task.id)
      await update($, background, () => shells)
    }
    return ran
  })

  on('ui.render', { component: 'Spinner' }, async ($, e, next) => {
    const list = await read($, calls)
    const running = (await read($, background)).length
    await read($, tick) // subscribes the drawing to the age tick
    const detail = describe(list, running, await $.clock.now())
    if (detail === null) {
      return next(e)
    }
    return next({ ...e, props: { ...e.props, suffix: `${e.props.suffix} ${detail}` } })
  })

  on('turn.complete', async ($, e, next) => {
    const ran = await next(e)
    if (e.agentId !== undefined) {
      return ran
    }
    // Nothing of the main loop runs past its turn; a reload may have left one.
    await update($, calls, list => list.filter(one => one.agentId !== null))
    if (e.durationMs < LONG_TURN_MS) {
      return ran
    }
    const line = breakdown(e.durationMs, await read($, turn), e.usage)
    // next() answers the answer's own text; any other text is a line drawn
    // beneath it. A line a plugin below already set stays, ours goes after it.
    return { ...ran, text: ran.text === e.answer ? line : `${ran.text}\n${line}` }
  })
}

// describe is what the spinner adds: the main loop's oldest call in flight
// (its age once it has run a tick), foreground agents, background tasks.
function describe(list: readonly PeekCall[], running: number, now: number): string | null {
  const main = list.filter(one => one.agentId === null)
  const agents = main.filter(one => AGENT_TOOLS.has(one.tool)).length
  const parts: string[] = []
  const oldest = main.reduce<PeekCall | null>((first, one) => (first === null || one.startedAt < first.startedAt ? one : first), null)
  if (oldest !== null) {
    const age = now - oldest.startedAt
    parts.push(age >= TICK_MS ? `${oldest.label} · ${duration(age)}` : oldest.label)
    if (main.length > 1) {
      parts.push(`+${main.length - 1} more`)
    }
  }
  if (agents > 0) {
    parts.push(agents === 1 ? '1 agent' : `${agents} agents`)
  }
  if (running > 0) {
    parts.push(`${running} bg`)
  }
  return parts.length === 0 ? null : parts.join(' · ')
}

function breakdown(durationMs: number, times: readonly PeekToolTime[], usage: TurnUsage | undefined): string {
  const tools = [...times]
    .sort((a, b) => b.ms - a.ms)
    .slice(0, 4)
    .map(one => `${one.tool} ${duration(one.ms)} ×${one.calls}`)
  const parts = [`Turn ${duration(durationMs)}`]
  if (tools.length > 0) {
    parts.push(`tool time: ${tools.join(', ')}`)
  }
  if (usage !== undefined) {
    const input = usage.input_tokens + usage.cache_read_input_tokens + usage.cache_creation_input_tokens
    parts.push(`tokens: ${count(input)} in (${count(usage.cache_read_input_tokens)} cached) / ${count(usage.output_tokens)} out`)
  }
  return parts.join(' — ')
}

function addTime(times: readonly PeekToolTime[], tool: string, ms: number): PeekToolTime[] {
  const found = times.find(one => one.tool === tool)
  if (found === undefined) {
    return [...times, { tool, ms, calls: 1 }]
  }
  return times.map(one => (one.tool === tool ? { tool, ms: one.ms + ms, calls: one.calls + 1 } : one))
}

function labelOf(e: ToolCallInput): string {
  if ('description' in e && typeof e.description === 'string' && e.description.trim() !== '') {
    return clip(e.description.trim())
  }
  if (e.tool === 'Bash') {
    return clip(e.command.trim().split('\n')[0] ?? '')
  }
  const tool = e.tool.startsWith('mcp__') ? (e.tool.split('__').at(-1) ?? e.tool) : e.tool
  if ('file_path' in e && typeof e.file_path === 'string') {
    return clip(`${tool} ${e.file_path.split('/').at(-1) ?? ''}`)
  }
  return tool
}

function backgroundTaskId(result: unknown): string | null {
  if (typeof result !== 'object' || result === null || !('backgroundTaskId' in result)) {
    return null
  }
  return typeof result.backgroundTaskId === 'string' ? result.backgroundTaskId : null
}

function clip(text: string): string {
  return text.length <= LABEL_MAX ? text : `${text.slice(0, LABEL_MAX - 1)}…`
}

function duration(ms: number): string {
  const minutes = Math.floor(ms / 60_000)
  if (minutes < 1) {
    return `${Math.floor(ms / 1000)}s`
  }
  if (minutes < 60) {
    return `${minutes}m`
  }
  const rest = minutes % 60
  return rest === 0 ? `${Math.floor(minutes / 60)}h` : `${Math.floor(minutes / 60)}h ${rest}m`
}

function count(tokens: number): string {
  if (tokens >= 1_000_000) {
    return `${(tokens / 1_000_000).toFixed(1)}M`
  }
  if (tokens >= 1000) {
    return `${Math.round(tokens / 1000)}k`
  }
  return String(tokens)
}
