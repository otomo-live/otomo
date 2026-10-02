import { describe, expect, it } from 'vitest'

import type { ValidationIssue } from '@/api/config'
import {
  clone,
  fieldOf,
  messagesFor,
  parseRaw,
  serialise,
  unclaimedIssues,
  withField,
  type ParseOutcome,
} from '@/config/document'

/**
 * The pure half of the reconciliation rule: the parsed document is the single
 * source of truth, so everything that reads or writes one has to agree about what
 * "the document" is. These functions are what the form, the raw tab and the
 * unsaved-changes list all go through, and they are tested here rather than
 * through the editor view because the interesting cases are the ones a person
 * cannot easily type into a browser.
 */

/** The two failure kinds carry a message and no document, which is all the tests need. */
function messageOf(outcome: ParseOutcome): string {
  if (outcome.kind === 'document') throw new Error('expected a failure, got a document')
  return outcome.message
}

function documentOf(outcome: ParseOutcome): Record<string, unknown> {
  if (outcome.kind !== 'document') throw new Error(`expected a document, got ${outcome.kind}`)
  return outcome.document
}

describe('parseRaw', () => {
  it('parses an object, which is the only root the form can lay out', () => {
    expect(documentOf(parseRaw('{"name":"Arc Rifle","ammo":6}'))).toEqual({
      name: 'Arc Rifle',
      ammo: 6,
    })
  })

  it('keeps properties the schema knows nothing about, because the form does not own them', () => {
    const document = documentOf(parseRaw('{"name":"x","extra":{"deep":[1,2]}}'))
    expect(document.extra).toEqual({ deep: [1, 2] })
  })

  it('reports a syntax error rather than an empty document', () => {
    const outcome = parseRaw('{"name": }')
    expect(outcome.kind).toBe('unparsable')
    expect(messageOf(outcome).length).toBeGreaterThan(0)
  })

  it('separates "not JSON" from "JSON the form cannot lay out"', () => {
    // The two need different sentences: one is a syntax error, the other is a
    // document that is perfectly valid and has no properties to render. A single
    // failure kind would have to say one of those things about the other case.
    for (const text of ['[1,2,3]', 'null', '"a string"', '5', 'true']) {
      const outcome = parseRaw(text)
      expect(outcome.kind).toBe('not-an-object')
      expect(messageOf(outcome)).toContain('object')
    }
  })

  it('treats an empty text as unparsable, so clearing the tab is not a save', () => {
    expect(parseRaw('').kind).toBe('unparsable')
  })
})

describe('serialise and clone', () => {
  it('writes two-space JSON, which is what a person reads and what Config stores', () => {
    expect(serialise({ a: 1 })).toBe('{\n  "a": 1\n}')
  })

  it('spells an absent document out rather than leaving the tab blank', () => {
    expect(serialise(undefined)).toBe('null')
  })

  it('clones a document that shares nothing with the original', () => {
    const original = { name: 'Arc Rifle', tags: ['a', 'b'] }
    const copy = clone(original) as { name: string; tags: string[] }

    expect(copy).toEqual(original)
    copy.tags.push('c')

    // The whole reason the editor keeps two: a diff between an object and itself
    // is empty however much has been typed into it.
    expect(original.tags).toEqual(['a', 'b'])
  })

  it('round trips a document through JSON, which is the depth of copy that is right for one', () => {
    const original = { nested: { list: [1, { deep: true }] }, nil: null }
    expect(clone(original)).toEqual(original)
  })
})

describe('fieldOf', () => {
  it('reads a property, and undefined for a property that is not there', () => {
    expect(fieldOf({ ammo: 6 }, 'ammo')).toBe(6)
    expect(fieldOf({ ammo: 6 }, 'range')).toBeUndefined()
  })

  it('keeps absent and null apart, because a schema default applies to the first', () => {
    expect(fieldOf({ ammo: null }, 'ammo')).toBeNull()
    expect(fieldOf({}, 'ammo')).toBeUndefined()
  })

  it('answers undefined for anything that is not a document', () => {
    expect(fieldOf(null, 'ammo')).toBeUndefined()
    expect(fieldOf([1, 2], 'ammo')).toBeUndefined()
    expect(fieldOf('text', 'ammo')).toBeUndefined()
  })
})

describe('withField', () => {
  it('sets a value in a copy and leaves the document it was given alone', () => {
    const before = { name: 'Arc Rifle', ammo: 6 }
    const after = withField(before, 'ammo', 7)

    expect(after).toEqual({ name: 'Arc Rifle', ammo: 7 })
    expect(before.ammo).toBe(6)
  })

  it('removes the key when the value is undefined, rather than storing one JSON drops', () => {
    const next = withField({ name: 'x', ammo: 6 }, 'ammo', undefined)
    expect('ammo' in next).toBe(false)
    expect(next).toEqual({ name: 'x' })
  })

  it('builds a document out of something that is not one', () => {
    expect(withField(null, 'name', 'x')).toEqual({ name: 'x' })
    expect(withField('not a document', 'name', 'x')).toEqual({ name: 'x' })
  })

  it('keeps a value the document already held when another field is written', () => {
    expect(withField({ a: 1, b: 2 }, 'b', 3)).toEqual({ a: 1, b: 3 })
  })
})

describe('messagesFor', () => {
  const issues: ValidationIssue[] = [
    { pointer: '', message: 'the document itself' },
    { pointer: '/name', message: 'name' },
    { pointer: '/names', message: 'a sibling whose pointer only shares a prefix' },
    { pointer: '/tags/0', message: 'first tag' },
    { pointer: '/tags/0/label', message: 'a field beneath the first tag' },
    { pointer: '/extra', message: 'a property no field renders' },
  ]

  it('gives a field its own issue', () => {
    expect(messagesFor(issues, '/name')).toEqual(['name'])
  })

  it('gives an array item the issues beneath it too', () => {
    expect(messagesFor(issues, '/tags/0')).toEqual(['first tag', 'a field beneath the first tag'])
  })

  it('does not leak across a shared prefix', () => {
    // `/names` starts with `/name` and must not be read as a child of it: the
    // boundary is the slash, which is what makes this worth a test.
    expect(messagesFor(issues, '/name')).not.toContain(
      'a sibling whose pointer only shares a prefix',
    )
  })

  it('gives nothing to a pointer no issue mentions', () => {
    expect(messagesFor(issues, '/range')).toEqual([])
  })

  it('never gives a document-level issue to a field', () => {
    // No field pointer is a prefix of `''`, so a complaint about the document as
    // a whole can only reach the reader through unclaimedIssues below.
    for (const pointer of ['/name', '/names', '/tags/0', '/extra']) {
      expect(messagesFor(issues, pointer)).not.toContain('the document itself')
    }
  })
})

describe('unclaimedIssues', () => {
  const issues: ValidationIssue[] = [
    { pointer: '', message: 'the document itself' },
    { pointer: '/name', message: 'name' },
    { pointer: '/tags/0', message: 'first tag' },
    { pointer: '/extra', message: 'a property no field renders' },
  ]

  it('keeps what no field would show', () => {
    const unclaimed = unclaimedIssues(issues, ['/name', '/tags'])
    expect(unclaimed.map((issue) => issue.pointer)).toEqual(['', '/extra'])
  })

  it('is everything when the form renders nothing', () => {
    expect(unclaimedIssues(issues, []).map((issue) => issue.pointer)).toEqual([
      '',
      '/name',
      '/tags/0',
      '/extra',
    ])
  })

  it('is nothing when the form renders every field an issue mentions', () => {
    expect(
      unclaimedIssues(issues, ['/name', '/tags', '/extra']).map((issue) => issue.pointer),
    ).toEqual([''])
  })
})
