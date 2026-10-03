// fleet's state contract: the session values its hooks keep in $.state.
// Self-contained by rule, so it restates the few fields the pane draws from
// fleet.gen.d.ts (the Go contract); register.tsx assigns the generated types
// to these, so tsc fails if the two ever stop fitting.

export type FleetPaneState = 'waiting' | 'busy' | 'idle' | 'shell' | 'dead' | 'ended' | 'unknown'

/** One Claude session as the pane draws it. */
export type FleetPaneSession = {
  sessionId: string
  name?: string
  project: string
  state: FleetPaneState
  waitingFor?: string
  ageSeconds: number
  openJobs: number
  /** `cd '<cwd>' && claude --resume <id>`, what [copy] puts on the clipboard. */
  resume: string
}

export type FleetPaneCounts = {
  total: number
  waiting: number
  busy: number
  idle: number
  shell: number
  dead: number
  ended: number
  unknown: number
}

/** One live Codex thread, read-only. */
export type FleetPaneCodex = {
  threadId: string
  name?: string
  branch?: string
  project: string
  calls: number
  lastCallAt?: string
}

/** What the pane draws from: the last `vybava fleet --json` that succeeded. */
export type FleetView = {
  generatedAt: string
  counts: FleetPaneCounts
  sessions: FleetPaneSession[]
}

declare module 'claude-code' {
  interface PluginState {
    'fleet-pane': {
      view: FleetView | null
      /** Codex rows from the last refresh that read them (every ~60 s). */
      codex: FleetPaneCodex[]
      /** Why the last refresh failed; null once one succeeds. */
      problem: string | null
      /** The session whose reply field is open. */
      replyTo: string | null
      /** The last action's outcome: sent, not delivered, copied. */
      notice: string | null
      /** This session's own id: its row is marked and left out of the count. */
      self: string | null
    }
  }
}
