import { mount } from '@vue/test-utils'
import { defineComponent, h } from 'vue'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { usePolling, type PollingTarget } from '@/composables/usePolling'

/**
 * The poller's contract with a background tab, which is the whole reason it
 * exists: no timer and no request while hidden, one immediate request on the way
 * back if the picture is stale, and an abort when the page leaves.
 *
 * Time is faked so an hour of hiding costs no wall-clock time, and the document
 * is a fake so the test never touches the real `visibilityState`.
 */

function fakeTarget(initial: DocumentVisibilityState = 'visible') {
  const listeners = new Set<() => void>()
  const doc = {
    visibilityState: initial,
    addEventListener(_type: 'visibilitychange', listener: () => void): void {
      listeners.add(listener)
    },
    removeEventListener(_type: 'visibilitychange', listener: () => void): void {
      listeners.delete(listener)
    },
  }
  const set = (state: DocumentVisibilityState): void => {
    doc.visibilityState = state
    for (const listener of listeners) listener()
  }
  return {
    doc: doc as PollingTarget,
    hide: () => set('hidden'),
    show: () => set('visible'),
    listenerCount: () => listeners.size,
  }
}

beforeEach(() => {
  vi.useFakeTimers()
})

afterEach(() => {
  vi.useRealTimers()
})

describe('usePolling', () => {
  it('polls once immediately and then every interval', async () => {
    const { doc } = fakeTarget()
    const fn = vi.fn()
    const polling = usePolling({ fn, intervalMs: 15_000, target: doc })

    polling.start()
    expect(fn).toHaveBeenCalledTimes(1)

    await vi.advanceTimersByTimeAsync(15_000)
    expect(fn).toHaveBeenCalledTimes(2)

    await vi.advanceTimersByTimeAsync(15_000)
    expect(fn).toHaveBeenCalledTimes(3)

    polling.stop()
  })

  it('sends nothing while hidden, even across a minute of time, and resumes on show', async () => {
    const { doc, hide, show } = fakeTarget()
    const fn = vi.fn()
    const polling = usePolling({ fn, intervalMs: 15_000, target: doc })

    polling.start()
    expect(fn).toHaveBeenCalledTimes(1)

    hide()
    await vi.advanceTimersByTimeAsync(60_000)
    // Four intervals of fake time, zero requests.
    expect(fn).toHaveBeenCalledTimes(1)

    show()
    // Stale, so showing runs immediately rather than waiting an interval.
    expect(fn).toHaveBeenCalledTimes(2)

    await vi.advanceTimersByTimeAsync(15_000)
    expect(fn).toHaveBeenCalledTimes(3)

    polling.stop()
  })

  it('starts hidden without polling, then fetches on the first show', async () => {
    const { doc, show } = fakeTarget('hidden')
    const fn = vi.fn()
    const polling = usePolling({ fn, intervalMs: 15_000, target: doc })

    polling.start()
    await vi.advanceTimersByTimeAsync(60_000)
    expect(fn).not.toHaveBeenCalled()

    show()
    expect(fn).toHaveBeenCalledTimes(1)

    polling.stop()
  })

  it('does not re-fetch on show when the last answer is fresh', async () => {
    const { doc, hide, show } = fakeTarget()
    const fn = vi.fn()
    const polling = usePolling({ fn, intervalMs: 15_000, target: doc })

    polling.start()
    expect(fn).toHaveBeenCalledTimes(1)

    // Away for two seconds and back: the first answer is still recent.
    await vi.advanceTimersByTimeAsync(2_000)
    hide()
    show()
    expect(fn).toHaveBeenCalledTimes(1)

    await vi.advanceTimersByTimeAsync(13_000)
    expect(fn).toHaveBeenCalledTimes(2)

    polling.stop()
  })

  it('aborts the in-flight request when hidden', async () => {
    const { doc, hide } = fakeTarget()
    const signals: AbortSignal[] = []
    const fn = vi.fn((incoming: AbortSignal) => {
      signals.push(incoming)
    })
    const polling = usePolling({ fn, intervalMs: 15_000, target: doc })

    polling.start()
    expect(signals[0].aborted).toBe(false)

    hide()
    expect(signals[0].aborted).toBe(true)

    polling.stop()
  })

  it('clears its timer and aborts on unmount', async () => {
    const target = fakeTarget()
    const signals: AbortSignal[] = []
    const fn = vi.fn((incoming: AbortSignal) => {
      signals.push(incoming)
    })
    const Host = defineComponent({
      setup() {
        usePolling({ fn, intervalMs: 1_000, target: target.doc })
        return () => h('div')
      },
    })

    const wrapper = mount(Host)
    expect(fn).toHaveBeenCalledTimes(1)
    await vi.advanceTimersByTimeAsync(1_000)
    expect(fn).toHaveBeenCalledTimes(2)

    wrapper.unmount()
    expect(target.listenerCount()).toBe(0)
    expect(signals[1].aborted).toBe(true)

    await vi.advanceTimersByTimeAsync(5_000)
    expect(fn).toHaveBeenCalledTimes(2)
  })
})
