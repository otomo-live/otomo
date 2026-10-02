import { HttpResponse, http } from 'msw'
import { describe, expect, it } from 'vitest'

import type { LogLine } from '@/api/dashboard'
import { isTooManyTails, parseEventStream, tailLogs } from '@/api/sse'
import { server } from '@/mocks/server'

/**
 * The log tail. `EventSource` cannot carry an Authorization header, so the tail is
 * a `fetch` whose body is read chunk by chunk, and the four things a log view
 * needs are then this module's job rather than a component's: keep a buffer
 * across chunk boundaries, reconnect when the server closes the stream, back off,
 * and stop cleanly when the view is left.
 *
 * The parse cases are fed as strings because a chunk boundary can fall anywhere,
 * and the loop cases run against the fixture's real streamed frames.
 */

const TAIL = '/api/admin/dashboard/logs/tail'

/** Polling, because the thing being waited for is a network read rather than a tick. */
async function until(condition: () => boolean, milliseconds = 5000): Promise<void> {
  const deadline = Date.now() + milliseconds
  while (!condition()) {
    if (Date.now() > deadline) throw new Error('timed out waiting for the log tail')
    await new Promise((resolve) => setTimeout(resolve, 10))
  }
}

function watcher() {
  const lines: LogLine[] = []
  const errors: unknown[] = []
  const reconnects: number[] = []
  return { lines, errors, reconnects }
}

describe('parseEventStream', () => {
  it('reads one complete event', () => {
    expect(parseEventStream('data: {"a":1}\n\n')).toEqual({ events: ['{"a":1}'], rest: '' })
  })

  it('keeps a frame that has not finished arriving as the remainder', () => {
    const first = parseEventStream('data: {"at":"2026-')
    expect(first.events).toEqual([])
    expect(first.rest).toBe('data: {"at":"2026-')
  })

  it('joins a frame split across two chunks rather than dropping the first half', () => {
    const first = parseEventStream('data: {"at":"2026-')
    const second = parseEventStream(`${first.rest}09-22T10:15:01Z"}\n\n`)

    expect(second.events).toEqual(['{"at":"2026-09-22T10:15:01Z"}'])
    expect(second.rest).toBe('')
  })

  it('reads two events that arrived in one chunk', () => {
    const parsed = parseEventStream('data: one\n\ndata: two\n\n')
    expect(parsed.events).toEqual(['one', 'two'])
    expect(parsed.rest).toBe('')
  })

  it('joins the data lines of one event with newlines', () => {
    expect(parseEventStream('data: one\ndata: two\n\n').events).toEqual(['one\ntwo'])
  })

  it('ignores every field that is not data, which is how an id or an event name is skipped', () => {
    expect(parseEventStream('event: ping\nid: 7\ndata: {"a":1}\n\n').events).toEqual(['{"a":1}'])
  })

  it('treats a comment-only block as a keep-alive and not as an event', () => {
    // A server that sends `:keep-alive` to hold a quiet connection open must not
    // make the view render a blank line every fifteen seconds.
    const parsed = parseEventStream(':keep-alive\n\ndata: {"a":1}\n\n')
    expect(parsed.events).toEqual(['{"a":1}'])
  })

  it('reads a data line with no space after the colon', () => {
    expect(parseEventStream('data:{"a":1}\n\n').events).toEqual(['{"a":1}'])
  })

  it('treats a blank line as the separator whatever line ending the server uses', () => {
    expect(parseEventStream('data: {"a":1}\r\n\r\n').events).toEqual(['{"a":1}'])
  })

  it('has nothing to say about an empty chunk', () => {
    expect(parseEventStream('')).toEqual({ events: [], rest: '' })
  })
})

describe('tailLogs', () => {
  it('delivers the lines as they arrive and stops when asked', async () => {
    const { lines, errors, reconnects } = watcher()
    const tail = tailLogs({
      service: 'config',
      onLine: (line) => lines.push(line),
      onError: (error) => errors.push(error),
      onReconnect: (attempt) => reconnects.push(attempt),
    })

    await until(() => lines.length >= 2)
    tail.stop()
    await tail.done

    expect(lines.map((line) => line.service)).toEqual(['config', 'config'])
    expect(lines.map((line) => line.message)).toContain(
      'draft read namespace=balance.weapons revision=12',
    )
    expect(lines[0].at).not.toBe('')
    expect(errors).toEqual([])
  })

  it('reconnects after the server closes the stream, and counts the attempts', async () => {
    // A stream that ends is not a failure: a log tail is expected to be cut and
    // resumed, and the fixture closes deliberately so this path is exercised.
    const { lines, errors, reconnects } = watcher()
    const tail = tailLogs({
      level: 'debug',
      onLine: (line) => lines.push(line),
      onError: (error) => errors.push(error),
      onReconnect: (attempt) => reconnects.push(attempt),
    })

    await until(() => reconnects.length >= 1)
    tail.stop()
    await tail.done

    expect(reconnects[0]).toBe(1)
    expect(errors).toEqual([])
    expect(lines.length).toBeGreaterThan(0)
  })

  it('stops cleanly: an abort is not a failure, and stopping twice is the same as once', async () => {
    const { errors } = watcher()
    const tail = tailLogs({ onLine: () => {}, onError: (error) => errors.push(error) })

    tail.stop()
    tail.stop()
    await tail.done

    expect(errors).toEqual([])
  })

  it('says nothing and reports no error when a filter matches no line', async () => {
    const { lines, errors, reconnects } = watcher()
    const tail = tailLogs({
      service: 'no-such-service',
      onLine: (line) => lines.push(line),
      onError: (error) => errors.push(error),
      onReconnect: (attempt) => reconnects.push(attempt),
    })

    await until(() => reconnects.length >= 1)
    tail.stop()
    await tail.done

    expect(lines).toEqual([])
    expect(errors).toEqual([])
  })

  it('shows a frame that is not JSON rather than dropping it, which is the worst a log view could do', async () => {
    const encoder = new TextEncoder()
    server.use(
      http.get(
        TAIL,
        () =>
          new HttpResponse(
            new ReadableStream<Uint8Array>({
              start(controller) {
                controller.enqueue(encoder.encode('data: plain text from a service\n\n'))
                controller.close()
              },
            }),
            { headers: { 'Content-Type': 'text/event-stream' } },
          ),
      ),
    )

    const { lines } = watcher()
    const tail = tailLogs({ onLine: (line) => lines.push(line) })

    await until(() => lines.length >= 1)
    tail.stop()
    await tail.done

    expect(lines[0]).toEqual({
      at: '',
      service: '',
      level: '',
      message: 'plain text from a service',
      fields: null,
    })
  })

  it('sends the contains filter and surfaces a 429 as a typed stop rather than retrying', async () => {
    server.use(
      http.get(TAIL, () =>
        HttpResponse.json(
          { error: { code: 'too_many_tails', message: 'limit reached', request_id: 'req_1' } },
          { status: 429 },
        ),
      ),
    )

    const { lines, errors, reconnects } = watcher()
    const tail = tailLogs({
      contains: 'needle',
      onLine: (line) => lines.push(line),
      onError: (error) => errors.push(error),
      onReconnect: (attempt) => reconnects.push(attempt),
    })

    await until(() => errors.length >= 1)
    await tail.done

    expect(lines).toEqual([])
    expect(reconnects).toEqual([])
    expect(isTooManyTails(errors[0])).toBe(true)
    expect((errors[0] as { code?: string }).code).toBe('too_many_tails')
  })
})
