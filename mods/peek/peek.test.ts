import type { RenderPropsOf } from 'claude-code'
import { expect, mock, test } from 'claude-code/testing'

const SURFACES = ['terminal', 'desktop'] as const
const SPINNER: RenderPropsOf['Spinner'] = { word: 'Sauteing', message: null, suffix: '…', mode: 'tool-use' }
const MINUTE = 60_000

test('a held Bash call names itself in the spinner, its age once it ran a tick', async ($, on) => {
  const clock = mock.clock(on)
  const held: Array<() => void> = []
  on('session.start', (_$, e) => ({ cwd: e.cwd }))
  on('tool.call', { tool: 'Bash' }, (_$, e) =>
    e.run_in_background === true
      ? { result: { stdout: '', stderr: '', interrupted: false, backgroundTaskId: 'b7k2' } }
      : new Promise(resolve => {
          held.push(() => resolve({ result: { stdout: '', stderr: '', interrupted: false } }))
        }),
  )
  on('prompt.submit', (_$, e) => ({ text: e.text }))
  let suffix = ''
  on('ui.render', { component: 'Spinner' }, (_$, e) => {
    suffix = e.props.suffix
    return { type: 'engine', ref: 0 }
  })
  const spinner = async (surface: (typeof SURFACES)[number]) => {
    await $.ui.render({ surface, component: 'Spinner', requestId: 'main', props: SPINNER })
    return suffix
  }

  await $.session.start({ cwd: '/repo', surface: 'terminal', isInteractive: true })
  void $.tool.call({ tool: 'Bash', command: 'devbox run verify', description: 'Run devbox verify' })
  await clock.settle()
  for (const surface of SURFACES) {
    expect(await spinner(surface)).toBe('… Run devbox verify')
  }

  await clock.advance(12 * MINUTE)
  await $.tool.call({ tool: 'Bash', command: 'bun test', run_in_background: true })
  for (const surface of SURFACES) {
    expect(await spinner(surface)).toBe('… Run devbox verify · 12m · 1 bg')
  }

  await $.prompt.submit({
    text: '<task-notification><task-id>b7k2</task-id><status>completed</status></task-notification>',
    wait: false,
    origin: { kind: 'task-notification' },
  })
  held.forEach(release => release())
  await clock.settle()
  for (const surface of SURFACES) {
    expect(await spinner(surface)).toBe('…')
  }
})

test('a main-loop turn past 30 minutes ends with its time and token breakdown', async ($, on) => {
  const clock = mock.clock(on)
  on('turn.start', (_$, e) => ({ turnId: e.turnId }))
  on('tool.call', { tool: 'Bash' }, async () => {
    await clock.sleep(20 * MINUTE)
    return { result: { stdout: '', stderr: '', interrupted: false } }
  })
  on('turn.complete', (_$, e) => ({ text: e.answer }))
  const usage = {
    model: 'claude-opus-5-5',
    input_tokens: 40_000,
    cache_read_input_tokens: 1_100_000,
    cache_creation_input_tokens: 60_000,
    output_tokens: 38_000,
  }

  await $.turn.start({ text: 'ship it', turnId: 't1' })
  const call = $.tool.call({ tool: 'Bash', command: 'gh pr checks --watch', description: 'Wait on CI' })
  await clock.advance(20 * MINUTE)
  await call
  const long = await $.turn.complete({
    answer: 'done', durationMs: 47 * MINUTE, isAborted: false, turnId: 't1', reason: 'answer', usage,
  })
  expect(long.text).toBe('Turn 47m — tool time: Bash 20m ×1 — tokens: 1.2M in (1.1M cached) / 38k out')

  await $.turn.start({ text: 'again', turnId: 't2' })
  const short = await $.turn.complete({
    answer: 'done', durationMs: 5 * MINUTE, isAborted: false, turnId: 't2', reason: 'answer', usage,
  })
  expect(short.text).toBe('done')
})
