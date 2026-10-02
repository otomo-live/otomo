/**
 * A fixed-capacity buffer that keeps the most recent items, newest first.
 *
 * The log tail is the reason it exists. A stream at a couple of hundred lines a
 * second, left open, would grow without bound and turn an evening's logging into
 * hundreds of megabytes of DOM. The tail keeps a window, and the oldest line
 * falls off the end as a new one arrives.
 *
 * The backing array is allocated once and never grows, and `prepend` writes one
 * slot and moves one index: O(1), with no array copy per line. A view that has
 * to hand the data to a virtualiser asks for `toArray()` once per animation
 * frame, which is a copy per frame rather than per line.
 */
export class RingBuffer<T> {
  private readonly slots: (T | undefined)[]
  /** Index of the newest item. */
  private head = 0
  private size = 0

  constructor(readonly capacity: number) {
    if (!Number.isInteger(capacity) || capacity <= 0) {
      throw new RangeError('RingBuffer capacity must be a positive integer')
    }
    this.slots = new Array<T | undefined>(capacity)
  }

  get length(): number {
    return this.size
  }

  get full(): boolean {
    return this.size === this.capacity
  }

  /** The most recently prepended item, or `undefined` when empty. */
  get newest(): T | undefined {
    return this.size === 0 ? undefined : this.slots[this.head]
  }

  /** The item closest to being dropped, or `undefined` when empty. */
  get oldest(): T | undefined {
    if (this.size === 0) return undefined
    return this.slots[(this.head + this.size - 1) % this.capacity]
  }

  /** Index 0 is the newest. */
  get(index: number): T | undefined {
    if (index < 0 || index >= this.size) return undefined
    return this.slots[(this.head + index) % this.capacity]
  }

  /**
   * Put one item at the front. When full the oldest is overwritten in place,
   * which is what keeps this O(1) and allocation-free.
   */
  prepend(value: T): void {
    this.head = (this.head - 1 + this.capacity) % this.capacity
    this.slots[this.head] = value
    if (this.size < this.capacity) this.size += 1
  }

  /**
   * Put a batch at the front. Within `values` the caller supplies them oldest
   * first (as a stream delivers them), so the last one ends up the newest.
   */
  prependAll(values: Iterable<T>): void {
    for (const value of values) this.prepend(value)
  }

  /** Newest first. The copy is what a reactive consumer assigns to. */
  toArray(): T[] {
    const out = new Array<T>(this.size)
    for (let index = 0; index < this.size; index += 1) {
      out[index] = this.slots[(this.head + index) % this.capacity] as T
    }
    return out
  }

  clear(): void {
    this.slots.fill(undefined)
    this.head = 0
    this.size = 0
  }
}
