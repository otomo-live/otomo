import { getCurrentInstance, onBeforeUnmount, onMounted } from 'vue'

/**
 * A visibility-aware poller.
 *
 * The overview moves on the scale of a scrape interval, so the page refreshes on
 * a timer rather than holding a stream open — but a timer that keeps firing in a
 * background tab is pure waste and an aborted request every interval. This
 * schedules one timeout at a time, cancels it and the in-flight request the
 * moment the document is hidden, and on the way back runs immediately if the
 * last answer is older than the interval.
 *
 * The work is a callback rather than a function this module knows about, which
 * keeps it free of the API client and testable with a plain vi.fn.
 */

/** The piece of `document` this needs, so a test can supply a fake. */
export interface PollingTarget {
  readonly visibilityState: DocumentVisibilityState
  addEventListener(type: 'visibilitychange', listener: () => void): void
  removeEventListener(type: 'visibilitychange', listener: () => void): void
}

export interface UsePollingOptions {
  /** Run one poll. The signal aborts when the page hides or the component unmounts. */
  fn: (signal: AbortSignal) => void | Promise<void>
  /** Milliseconds between polls. Defaults to 15000. */
  intervalMs?: number
  /** Defaults to the real `document`; injectable so tests need not fake a global. */
  target?: PollingTarget
}

export interface Polling {
  start(): void
  stop(): void
}

const DEFAULT_INTERVAL_MS = 15_000

export function usePolling(options: UsePollingOptions): Polling {
  const interval = options.intervalMs ?? DEFAULT_INTERVAL_MS
  const target: PollingTarget = options.target ?? document

  let timer: ReturnType<typeof setTimeout> | null = null
  let controller: AbortController | null = null
  /** Zero until the first poll, which is what makes a hidden start skip it. */
  let lastRunAt = 0
  let active = false

  function clearTimer(): void {
    if (timer !== null) {
      clearTimeout(timer)
      timer = null
    }
  }

  /** Schedules the next poll relative to the last one, not to "now". */
  function schedule(): void {
    clearTimer()
    if (target.visibilityState === 'hidden') return

    const elapsed = lastRunAt === 0 ? interval : Date.now() - lastRunAt
    const delay = Math.max(interval - elapsed, 0)
    timer = setTimeout(() => {
      timer = null
      void run()
      schedule()
    }, delay)
  }

  async function run(): Promise<void> {
    if (target.visibilityState === 'hidden') return

    // One poll at a time: a manual refresh that overlapped the timer would
    // otherwise leave two requests writing the same refs in arrival order.
    controller?.abort()
    controller = new AbortController()
    lastRunAt = Date.now()
    try {
      await options.fn(controller.signal)
    } catch {
      // The callback owns its failure: it is the one that can describe it to a
      // person. A rejection must not become an unhandled one here.
    }
  }

  function onVisibilityChange(): void {
    if (target.visibilityState === 'hidden') {
      clearTimer()
      controller?.abort()
      return
    }
    // Back in view: only pay for a request if the picture is stale.
    if (Date.now() - lastRunAt >= interval) void run()
    schedule()
  }

  function start(): void {
    if (active) return
    active = true
    target.addEventListener('visibilitychange', onVisibilityChange)
    if (target.visibilityState !== 'hidden' && Date.now() - lastRunAt >= interval) {
      void run()
    }
    schedule()
  }

  function stop(): void {
    if (!active) return
    active = false
    clearTimer()
    target.removeEventListener('visibilitychange', onVisibilityChange)
    controller?.abort()
    controller = null
  }

  // Auto-wire inside a component; outside one (a unit test) the caller drives
  // start/stop itself.
  if (getCurrentInstance() !== null) {
    onMounted(start)
    onBeforeUnmount(stop)
  }

  return { start, stop }
}
