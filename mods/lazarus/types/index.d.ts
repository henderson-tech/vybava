// lazarus's state contract: the session values its hooks keep in $.state.

/** The background work lazarus follows; the ledger's job kinds less its own. */
export type LazarusKind = 'workflow' | 'shell' | 'monitor' | 'agent'

/** A background job as the ledger keys it (kind + id). */
export type LazarusJob = {
  kind: LazarusKind
  id: string
  /** A workflow's run, what resumeFromRunId takes. */
  runId?: string
  scriptPath?: string
  description?: string
  /** When it started, in $.clock.now() milliseconds. */
  startedAt: number
}

/**
 * What stopped the session: a usage limit, which continues by itself at
 * `until` when the reset is known, or an auth failure, which waits for a key.
 */
export type LazarusStall =
  | { kind: 'limit'; at: number; until: number | null }
  | { kind: 'auth'; at: number; error: string }

declare module 'claude-code' {
  interface PluginState {
    lazarus: {
      /** Jobs this process launched that have not ended. */
      mine: LazarusJob[]
      /** Task ids the model stopped itself (TaskStop): their end is no death. */
      stoppedByModel: string[]
      /** Jobs that died with a previous process, or were killed while this one ran. */
      dead: LazarusJob[]
      /** When lazarus found them dead, in milliseconds. */
      diedAt: number
      stall: LazarusStall | null
      /** The last main-loop turn's start; a limit wake that sees a newer one stands down. */
      lastTurnAt: number
    }
  }
}
