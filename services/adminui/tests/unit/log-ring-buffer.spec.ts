import { describe, expect, it } from 'vitest'

import { RingBuffer } from '@/logs/ringBuffer'

/**
 * The tail's buffer. The contract is small and specific: keep the newest N,
 * wherever they are put, and never copy the whole list to add one line, because
 * that is the difference between a tail that survives an afternoon and one that
 * makes the page janky within a minute.
 */

describe('RingBuffer', () => {
  it('keeps the newest items and drops the oldest once it is full', () => {
    const buffer = new RingBuffer<number>(5)
    buffer.prependAll([1, 2, 3, 4, 5, 6, 7])

    expect(buffer.length).toBe(5)
    // Newest first: 7 arrived last, 3 is the oldest kept (1 and 2 fell off).
    expect(buffer.toArray()).toEqual([7, 6, 5, 4, 3])
    expect(buffer.newest).toBe(7)
    expect(buffer.oldest).toBe(3)
    expect(buffer.get(0)).toBe(7)
    expect(buffer.get(4)).toBe(3)
    expect(buffer.get(5)).toBeUndefined()
  })

  it('is empty until something arrives, and reads nothing rather than undefined-ish', () => {
    const buffer = new RingBuffer<string>(3)
    expect(buffer.length).toBe(0)
    expect(buffer.toArray()).toEqual([])
    expect(buffer.newest).toBeUndefined()
    expect(buffer.oldest).toBeUndefined()
  })

  it('prepends a batch as one ordered splice, newest of the batch at the front', () => {
    const buffer = new RingBuffer<number>(10)
    buffer.prepend(100)
    buffer.prependAll([1, 2, 3])

    expect(buffer.toArray()).toEqual([3, 2, 1, 100])
  })

  it('wraps consistently across the capacity boundary', () => {
    const buffer = new RingBuffer<number>(3)
    for (const value of [1, 2, 3, 4, 5]) buffer.prepend(value)
    expect(buffer.toArray()).toEqual([5, 4, 3])
    buffer.prepend(6)
    buffer.prepend(7)
    expect(buffer.toArray()).toEqual([7, 6, 5])
  })

  it('inserts a hundred thousand items in well under two hundred milliseconds', () => {
    const buffer = new RingBuffer<number>(5000)
    const startedAt = performance.now()
    for (let index = 0; index < 100_000; index += 1) buffer.prepend(index)
    const elapsed = performance.now() - startedAt

    expect(buffer.length).toBe(5000)
    expect(buffer.newest).toBe(99_999)
    // The generous budget: the point is that it is linear in the inserts and
    // independent of the capacity, not that it hits a particular number.
    expect(elapsed).toBeLessThan(200)
  })

  it('rejects a capacity that is not a positive integer', () => {
    expect(() => new RingBuffer<number>(0)).toThrow(RangeError)
    expect(() => new RingBuffer<number>(1.5)).toThrow(RangeError)
  })

  it('clears back to empty', () => {
    const buffer = new RingBuffer<number>(3)
    buffer.prependAll([1, 2, 3])
    buffer.clear()

    expect(buffer.length).toBe(0)
    expect(buffer.toArray()).toEqual([])
  })
})
