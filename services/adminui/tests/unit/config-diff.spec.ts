import { describe, expect, it } from 'vitest'

import { describeValue, diffDocuments, formatPointer } from '@/config/diff'

/**
 * The diff the editor uses for its unsaved-changes list and the conflict dialog.
 *
 * It exists because "what changed between the version and the draft" is a
 * question about the store that Config answers, and "what changed between the
 * draft I loaded and the draft in this editor" is a question only the SPA can
 * answer, because the editor's copy was never saved. The output is deliberately
 * Config's own shape so one component renders both.
 */

describe('diffDocuments', () => {
  it('has nothing to say about a document that did not move', () => {
    expect(diffDocuments({ a: 1, b: { c: 2 } }, { a: 1, b: { c: 2 } })).toEqual([])
  })

  it('reports a changed leaf with both values', () => {
    expect(diffDocuments({ damage: 40 }, { damage: 45 })).toEqual([
      { pointer: '/damage', kind: 'changed', before: 40, after: 45 },
    ])
  })

  it('walks into a nested object and points at the leaf', () => {
    expect(diffDocuments({ a: { b: 1 } }, { a: { b: 2 } })).toEqual([
      { pointer: '/a/b', kind: 'changed', before: 1, after: 2 },
    ])
  })

  it('calls a key present on one side only added or removed, not changed against undefined', () => {
    // The distinction is what lets a reader say "the `ammo` column is new"
    // rather than "`ammo` went from nothing to 6".
    expect(diffDocuments({ a: 1 }, { a: 1, b: 2 })).toEqual([
      { pointer: '/b', kind: 'added', after: 2 },
    ])
    expect(diffDocuments({ a: 1, b: 2 }, { a: 1 })).toEqual([
      { pointer: '/b', kind: 'removed', before: 2 },
    ])
  })

  it('walks arrays by index, so a shifted list reads as changes rather than as one blob', () => {
    expect(diffDocuments([1, 2, 3], [1, 9])).toEqual([
      { pointer: '/1', kind: 'changed', before: 2, after: 9 },
      { pointer: '/2', kind: 'removed', before: 3 },
    ])
  })

  it('treats a container on one side and a scalar on the other as a change, not a walk', () => {
    // Walking a scalar's keys would report nothing at all for this, which is the
    // bug the container check at the top of the walker exists to prevent: the
    // scalar has no keys, so the walk would come back empty and the change would
    // be invisible.
    expect(diffDocuments({ a: { b: 1 } }, { a: 5 })).toEqual([
      { pointer: '/a', kind: 'changed', before: { b: 1 }, after: 5 },
    ])
    expect(diffDocuments({ a: 5 }, { a: [1, 2] })).toEqual([
      { pointer: '/a', kind: 'changed', before: 5, after: [1, 2] },
    ])
  })

  it('walks an array against an object by key, because both are containers', () => {
    // The alternative, calling an array that met an object a change, would make
    // `[1,2]` and `{"0":1,"1":2}` different documents, and they are the same
    // document: that is the reason one walker serves both shapes rather than two
    // walkers that could drift apart.
    expect(diffDocuments({ a: [1, 2] }, { a: { b: 3 } })).toEqual([
      { pointer: '/a/0', kind: 'removed', before: 1 },
      { pointer: '/a/1', kind: 'removed', before: 2 },
      { pointer: '/a/b', kind: 'added', after: 3 },
    ])
  })

  it('treats null as a value, not as a container', () => {
    expect(diffDocuments(null, {})).toEqual([
      { pointer: '', kind: 'changed', before: null, after: {} },
    ])
    expect(diffDocuments({ a: null }, { a: {} })).toEqual([
      { pointer: '/a', kind: 'changed', before: null, after: {} },
    ])
  })

  it('tells a number from the string that looks like it', () => {
    expect(diffDocuments({ a: '0' }, { a: 0 })).toEqual([
      { pointer: '/a', kind: 'changed', before: '0', after: 0 },
    ])
  })

  it('compares with Object.is, so the same value twice is not a change', () => {
    // `Object.is` rather than `===` for the one case it differs on that a
    // document can reach: NaN compared with itself is the same value, and a
    // walker using `===` would report a change that is not there.
    expect(diffDocuments({ a: Number.NaN }, { a: Number.NaN })).toEqual([])
  })

  it('reports every change, not the first', () => {
    const changes = diffDocuments({ a: 1, b: 2, c: 3 }, { a: 9, b: 2, c: 8 })
    expect(changes.map((change) => change.pointer).sort()).toEqual(['/a', '/c'])
  })

  it('takes a starting pointer, so a subtree can be diffed in place', () => {
    expect(diffDocuments({ b: 1 }, { b: 2 }, '/a')).toEqual([
      { pointer: '/a/b', kind: 'changed', before: 1, after: 2 },
    ])
  })
})

describe('formatPointer', () => {
  it('reads as a path a person can follow', () => {
    expect(formatPointer('/weapons/0/damage')).toBe('weapons[0].damage')
    expect(formatPointer('/range')).toBe('range')
    expect(formatPointer('/a/b/c')).toBe('a.b.c')
  })

  it('names the document itself for the empty pointer, which is a real pointer', () => {
    expect(formatPointer('')).toBe('(document)')
  })

  it('undoes the escapes for a slash and a tilde in a key', () => {
    // A key containing a slash rendered as a path separator would be a
    // confusing bug to chase, which is why JSON Pointer defines the escapes.
    expect(formatPointer('/a~1b/c')).toBe('a/b.c')
    expect(formatPointer('/a~0b')).toBe('a~b')
  })
})

describe('describeValue', () => {
  it('shows a value as one line of JSON', () => {
    expect(describeValue(40)).toBe('40')
    expect(describeValue('Arc Rifle')).toBe('"Arc Rifle"')
    expect(describeValue({ a: 1 })).toBe('{"a":1}')
  })

  it('names an absent value rather than printing nothing', () => {
    expect(describeValue(undefined)).toBe('not set')
  })

  it('shortens a value too long for a row', () => {
    const long = 'x'.repeat(200)
    const described = describeValue(long)
    expect(described).toHaveLength(80)
    expect(described.endsWith('...')).toBe(true)
  })
})
