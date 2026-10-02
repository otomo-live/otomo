import { stream } from '@/api/client'
import { ApiError } from '@/api/errors'
import { readLogLine, type LogLine } from '@/api/dashboard'

/**
 * The log tail, read as an event stream.
 *
 * `EventSource` is the obvious tool and is not usable here: it cannot send an
 * Authorization header, so it cannot carry the staff bearer token, and a stream
 * that cannot authenticate is a stream that 401s. It also retries on its own
 * schedule with no way to stop it. So the tail is a `fetch` whose body is read
 * chunk by chunk, which is what `stream()` in api/client.ts exists for.
 *
 * The reader is deliberately separate from the view. A log view has to do four
 * things a component should not be trusted with: keep a buffer across chunk
 * boundaries (a frame can arrive split in two), reconnect when the server closes
 * the stream, back off rather than hammer, and stop cleanly when the view is
 * left. All of that is here, and the view supplies three callbacks.
 */

/** How long to wait before the first reconnect, and the ceiling the doubling stops at. */
const RECONNECT_BASE_MS = 500
const RECONNECT_MAX_MS = 10_000

/**
 * The COM-5 code the tail endpoint answers with (429) once a staff member already
 * holds the two tails they are allowed, or the host has ten across everyone.
 * Retrying is pointless until someone closes one, so the loop stops and reports.
 */
export const TOO_MANY_TAILS = 'too_many_tails'

/** Whether a failure is the tail limit rather than a transport problem worth retrying. */
export function isTooManyTails(error: unknown): boolean {
  if (error instanceof ApiError) return error.code === TOO_MANY_TAILS || error.status === 429
  return false
}

export interface LogTailOptions {
  /** All three are server-side filters; empty means every service and every level. */
  service?: string
  level?: string
  contains?: string
  onLine: (line: LogLine) => void
  /** A failure that stopped a connection. Not called for an abort. */
  onError?: (error: unknown) => void
  /** Every reconnect, counting from 1. What a view shows as "reconnecting". */
  onReconnect?: (attempt: number) => void
}

export interface LogTail {
  /** Idempotent. Aborts the connection in flight and ends the loop. */
  stop: () => void
  /** Resolves once the loop has ended, after `stop()` or a permanent failure. */
  done: Promise<void>
}

/**
 * Splits whatever has arrived so far into complete events and the remainder.
 *
 * An SSE event ends at a blank line, `data:` lines within one event join with
 * newlines, and every other field is ignored. The remainder is returned rather
 * than kept in a module variable because a chunk boundary can fall anywhere: a
 * `data: {"at":"2026-` and its other half arrive as two reads, and a parser that
 * lost the first would drop the line rather than the frame.
 */
export function parseEventStream(buffer: string): { events: string[]; rest: string } {
  const events: string[] = []
  // Normalise CRLF first: the separator is a blank LINE, and a server that
  // writes \r\n would otherwise never match the \n\n below.
  const normalised = buffer.replaceAll('\r\n', '\n')
  const parts = normalised.split('\n\n')
  // The last part has no terminator yet, so it is not an event: it is the start
  // of the next one.
  const rest = parts.pop() ?? ''
  for (const part of parts) {
    const data = part
      .split('\n')
      .filter((line) => line.startsWith('data:'))
      .map((line) => line.slice('data:'.length).trimStart())
    // A comment-only or empty block is a keep-alive, not an event.
    if (data.length > 0) events.push(data.join('\n'))
  }
  return { events, rest }
}

function tailUrl(options: LogTailOptions): string {
  const search = new URLSearchParams()
  if (options.service !== undefined && options.service !== '')
    search.set('service', options.service)
  if (options.level !== undefined && options.level !== '') search.set('level', options.level)
  if (options.contains !== undefined && options.contains !== '')
    search.set('contains', options.contains)
  const query = search.toString()
  return `/api/admin/dashboard/logs/tail${query === '' ? '' : `?${query}`}`
}

function wait(milliseconds: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    const timer = setTimeout(resolve, milliseconds)
    signal.addEventListener(
      'abort',
      () => {
        clearTimeout(timer)
        resolve()
      },
      { once: true },
    )
  })
}

export function tailLogs(options: LogTailOptions): LogTail {
  const controller = new AbortController()
  const { signal } = controller
  const url = tailUrl(options)

  const done = (async () => {
    let attempt = 0

    while (!signal.aborted) {
      try {
        const response = await stream(url, { signal })
        const reader = response.body?.getReader()
        if (reader === undefined) throw new Error('the log tail answered with no body')

        // A connection that opened is what "back to normal" means, so the backoff
        // resets here rather than on the first line: an empty stream is still a
        // working stream.
        attempt = 0
        const decoder = new TextDecoder()
        let buffer = ''

        for (;;) {
          const { value, done: finished } = await reader.read()
          if (finished) break
          buffer += decoder.decode(value, { stream: true })
          const parsed = parseEventStream(buffer)
          buffer = parsed.rest
          for (const event of parsed.events) {
            options.onLine(readLogLine(safeParse(event)))
          }
        }
      } catch (error) {
        // An abort is not a failure: it is this function doing what stop() asked.
        if (signal.aborted) break
        options.onError?.(error)
        // The tail limit is a state, not a transient fault: reconnecting on the
        // backoff would just collect more 429s and hide the one message the
        // reader needs. Stop here, and let the view say why.
        if (isTooManyTails(error)) break
      }

      if (signal.aborted) break
      attempt += 1
      options.onReconnect?.(attempt)
      // Doubling, capped. A dashboard left open overnight should not be making a
      // hundred attempts a minute against a service that is down.
      await wait(Math.min(RECONNECT_BASE_MS * 2 ** (attempt - 1), RECONNECT_MAX_MS), signal)
    }
  })()

  return {
    stop: () => {
      controller.abort()
    },
    done,
  }
}

/**
 * A frame that is not JSON is still worth showing: the alternative is a log view
 * that silently drops whatever it cannot parse, which is the worst possible
 * behaviour for a log view.
 */
function safeParse(event: string): unknown {
  try {
    return JSON.parse(event)
  } catch {
    return { message: event }
  }
}
