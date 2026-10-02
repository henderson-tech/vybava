// peek's state contract: the session values its hooks keep in $.state.

/** A tool call in flight, as the spinner names it. */
export type PeekCall = {
  id: string
  tool: string
  /** The call's own description, else its tool and a head of its input. */
  label: string
  startedAt: number
  /** The subagent's loop; null on the main loop. */
  agentId: string | null
}

/** One tool's share of the running main-loop turn. */
export type PeekToolTime = { tool: string; ms: number; calls: number }

declare module 'claude-code' {
  interface PluginState {
    peek: {
      calls: PeekCall[]
      /** Background task ids still running (Bash run_in_background). */
      background: string[]
      turn: PeekToolTime[]
      /** Bumped every 30 s so the spinner's ages redraw. */
      tick: number
    }
  }
}
