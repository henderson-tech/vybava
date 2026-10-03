// wake's state contract: the session values its hooks keep in $.state.
// The event and subscription shapes mirror `vybava watch` (docs/watch.md).

/** One subscription this session holds at the watch daemon. */
export type WakeWatch = {
  /** The daemon's subscription id (`w…`). */
  id: string
  /** The canonical target, as the daemon answered it (`pr:owner/name#155`). */
  target: string
  /** The condition it waits for (`merged`, `checks-settled`, `idle`, …). */
  until: string
  /** Why the model subscribed, echoed back when it is woken. */
  note: string | null
}

/** What the daemon tells a subscriber. */
export type WakeEventKind = 'change' | 'met' | 'error' | 'expired'

/** One event of `GET /v1/events`, as wake reads it. */
export type WakeEvent = {
  seq: number
  subscription: string
  target: string
  until: string
  kind: WakeEventKind
  summary: string
  changed: string[]
  error: string
}

/** An event that ends a watch, with the note it was subscribed with. */
export type WakeUp = WakeEvent & { note: string | null }

/** The band row: the newest PR transition the person can act on. */
export type WakeAlert = {
  seq: number
  target: string
  /** `/prm`'s argument for the PR (`155 in owner/name`). */
  prm: string
  line: string
}

declare module 'claude-code' {
  interface PluginState {
    wake: {
      watches: WakeWatch[]
      /** The highest event seq handled; the next poll acknowledges up to it. */
      after: number
      /** Seqs handled outside a poll (an `add` answered met); skipped when they arrive. */
      handled: number[]
      /** Wake-ups that arrived while a turn ran, submitted when it ends. */
      held: WakeUp[]
      alerts: WakeAlert[]
      /** A main-loop turn is running. */
      busy: boolean
      /** The last call to the daemon failed to reach it. */
      isDown: boolean
      /** The install hint was shown this session. */
      hinted: boolean
    }
  }
}
