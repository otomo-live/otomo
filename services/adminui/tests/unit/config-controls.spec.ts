import { describe, expect, it } from 'vitest'

import {
  controlFor,
  enumOptions,
  memberForToken,
  numberFrom,
  textFor,
  tokenForValue,
} from '@/config/controls'

/**
 * The two decisions this module makes: which control a schema fragment gets, and
 * what a control carries as a value. Both are tested directly, because the
 * failure they would cause in the browser is a field that quietly writes a value
 * of the wrong shape into a document someone then saves.
 */

describe('controlFor', () => {
  it('reads the scalar types', () => {
    expect(controlFor({ type: 'boolean' })).toBe('boolean')
    expect(controlFor({ type: 'integer' })).toBe('integer')
    expect(controlFor({ type: 'number' })).toBe('number')
    expect(controlFor({ type: 'string' })).toBe('string')
  })

  it('prefers a closed set of choices whatever the type says', () => {
    expect(controlFor({ type: 'string', enum: ['a', 'b'] })).toBe('enum')
    expect(controlFor({ type: 'integer', enum: [1, 2] })).toBe('enum')
    // Untyped with an enum is still a closed set, which is all a select needs.
    expect(controlFor({ enum: ['a', 'b'] })).toBe('enum')
  })

  it('treats only `date` as a date', () => {
    expect(controlFor({ type: 'string', format: 'date' })).toBe('date')
    // Not the right control for a `date-time`: an RFC 3339 string carries an
    // offset a native datetime-local input cannot produce, so a control that
    // looked correct would write a string the schema's own format rejects.
    expect(controlFor({ type: 'string', format: 'date-time' })).toBe('string')
    expect(controlFor({ type: 'string', format: 'email' })).toBe('string')
  })

  it('repeats an array of scalars', () => {
    expect(controlFor({ type: 'array', items: { type: 'string' } })).toBe('scalars')
    expect(controlFor({ type: 'array', items: { type: 'integer' } })).toBe('scalars')
    expect(controlFor({ type: 'array', items: { enum: ['a', 'b'] } })).toBe('scalars')
  })

  it('falls back to the raw editor for anything it cannot lay out', () => {
    // Each of these is a case where guessing a control would write the wrong
    // shape: an object has no single field, a union has no single control, and an
    // untyped property could hold anything.
    expect(controlFor({ type: 'array', items: { type: 'object' } })).toBe('json')
    expect(controlFor({ type: 'array', items: { type: 'array' } })).toBe('json')
    expect(controlFor({ type: 'array' })).toBe('json')
    expect(controlFor({ type: 'array', items: 'string' })).toBe('json')
    expect(controlFor({ type: 'object', properties: { a: { type: 'string' } } })).toBe('json')
    expect(controlFor({ type: ['string', 'null'] })).toBe('json')
    expect(controlFor({})).toBe('json')
    expect(controlFor({ type: 'null' })).toBe('json')
  })
})

describe('enumOptions', () => {
  it('carries each member as its JSON text, so reordering cannot change what a value means', () => {
    const options = enumOptions({ enum: ['low', 'high'] })
    expect(options.map((option) => option.token)).toEqual(['"low"', '"high"'])
  })

  it('uses the members as labels when there is nothing else', () => {
    expect(enumOptions({ enum: ['low', 2, null] }).map((option) => option.label)).toEqual([
      'low',
      '2',
      'null',
    ])
  })

  it('uses enumNames for labels when it is the same length as the enum', () => {
    const options = enumOptions({ enum: ['low', 'high'], enumNames: ['Low', 'High'] })
    expect(options.map((option) => option.label)).toEqual(['Low', 'High'])
    expect(options.map((option) => option.token)).toEqual(['"low"', '"high"'])
  })

  it('ignores enumNames when it is a different length, because the pairing is unknown', () => {
    const options = enumOptions({ enum: ['low', 'high'], enumNames: ['Low'] })
    expect(options.map((option) => option.label)).toEqual(['low', 'high'])
  })

  it('is empty for a schema with no enum, rather than throwing', () => {
    expect(enumOptions({ type: 'string' })).toEqual([])
  })
})

describe('tokens and values', () => {
  it('round trips every JSON member, including null and a number', () => {
    for (const member of ['low', 2, true, null]) {
      const token = enumOptions({ enum: [member] })[0].token
      expect(memberForToken(token)).toEqual(member)
    }
  })

  it('tells a string member from a number that looks like one', () => {
    // The reason the token is JSON text and not the member's display text.
    expect(memberForToken(tokenForValue('2') ?? '')).toBe('2')
    expect(memberForToken(tokenForValue(2) ?? '')).toBe(2)
  })

  it('has no token for a field the document does not carry', () => {
    expect(tokenForValue(undefined)).toBeUndefined()
    expect(memberForToken('not json')).toBeUndefined()
  })
})

describe('textFor', () => {
  it('shows a value without inventing one', () => {
    expect(textFor('Arc Rifle')).toBe('Arc Rifle')
    expect(textFor(0)).toBe('0')
    expect(textFor(false)).toBe('false')
    expect(textFor({ a: 1 })).toBe('{"a":1}')
  })

  it('shows nothing for absent and for null, which are the same to a text field', () => {
    expect(textFor(undefined)).toBe('')
    expect(textFor(null)).toBe('')
  })
})

describe('numberFrom', () => {
  it('reads what has been typed as a value', () => {
    expect(numberFrom('12', false)).toEqual({ kind: 'value', value: 12 })
    expect(numberFrom(' 12 ', false)).toEqual({ kind: 'value', value: 12 })
    expect(numberFrom('+5', false)).toEqual({ kind: 'value', value: 5 })
    expect(numberFrom('-5', false)).toEqual({ kind: 'value', value: -5 })
    expect(numberFrom('1.5', false)).toEqual({ kind: 'value', value: 1.5 })
    expect(numberFrom('.5', false)).toEqual({ kind: 'value', value: 0.5 })
    expect(numberFrom('1e3', false)).toEqual({ kind: 'value', value: 1000 })
  })

  it('reads an emptied field as the absence of the key', () => {
    expect(numberFrom('', false)).toEqual({ kind: 'empty' })
    expect(numberFrom('   ', false)).toEqual({ kind: 'empty' })
  })

  it('leaves a half-typed number alone rather than clearing the field', () => {
    // A person typing `-12` passes through `-`. Treating that as a removal would
    // delete the value the moment they typed a minus sign.
    for (const text of ['-', '1e', '1e-', '.']) {
      expect(numberFrom(text, false)).toEqual({ kind: 'incomplete' })
    }
  })

  it('refuses the text Number would take and JSON would not', () => {
    // The document's rule, not the form's: `0x10` and `Infinity` are numbers to
    // Number() and are not JSON at all.
    for (const text of ['0x10', 'Infinity', '-Infinity', 'NaN', '1_000', 'abc']) {
      expect(numberFrom(text, false)).toEqual({ kind: 'incomplete' })
    }
  })

  it('truncates for an integer field, because the schema says an integer', () => {
    expect(numberFrom('12.7', true)).toEqual({ kind: 'value', value: 12 })
    expect(numberFrom('-12.7', true)).toEqual({ kind: 'value', value: -12 })
    expect(numberFrom('12.7', false)).toEqual({ kind: 'value', value: 12.7 })
  })

  it('reads a trailing dot as the number it already is', () => {
    // `1.` is complete as far as the pattern goes, so the value is 1 and typing
    // on to `1.5` refines it. Nothing is lost either way.
    expect(numberFrom('1.', false)).toEqual({ kind: 'value', value: 1 })
  })
})
