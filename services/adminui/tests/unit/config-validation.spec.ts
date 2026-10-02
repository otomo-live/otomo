import { afterEach, describe, expect, it } from 'vitest'

import balanceWeapons from '@/mocks/fixtures/namespace-balance-weapons.json'
import { unclaimedIssues } from '@/config/document'
import { validateLocally, type LocalValidation } from '@/config/validation'

/**
 * The local check, run against the real fixture schema rather than a schema
 * written for the test: the thing that could go wrong here is a mismatch between
 * the dialect a project's schema declares and the one the local validator is
 * asked to use, and a schema invented in the test file would not have that
 * mismatch.
 *
 * These are the three answers the editor has to distinguish. `unavailable` is the
 * one worth having: an editor that says "no problems" because it never managed to
 * check is worse than one that admits it does not know.
 */

const schema = balanceWeapons.schema.body
const validDocument = { name: 'Arc Rifle', damage: 40, range: 18.5, ammo: 6 }

/** Each case needs its own cache key, since the key is what the compiled validator is held under. */
function key(name: string): string {
  return `balance.weapons@${name}`
}

function errorsOf(result: LocalValidation) {
  if (result.kind !== 'invalid') throw new Error(`expected invalid, got ${result.kind}`)
  return result.errors
}

describe('validateLocally', () => {
  it('accepts a document that satisfies the schema', () => {
    expect(validateLocally(schema, validDocument, key('valid'))).toEqual({ kind: 'valid' })
  })

  it('accepts a document that leaves an optional field out', () => {
    const { name, damage, range } = validDocument
    expect(validateLocally(schema, { name, damage, range }, key('optional'))).toEqual({
      kind: 'valid',
    })
  })

  it('reports a missing required field against the field, not against the document', () => {
    // The library reports `required` against the OBJECT that is missing the
    // property, so a missing top-level field would arrive at `/` and render as
    // a complaint with no control to fix. Pointing it at the absent property is
    // the one adjustment this module makes to a library error.
    const errors = errorsOf(validateLocally(schema, { damage: 1, range: 1 }, key('required')))
    expect(errors).toContainEqual({ pointer: '/name', message: 'is required' })
  })

  it('reports every problem, not the first', () => {
    // `allErrors`, because a form that reports one problem at a time makes the
    // reader fix them one reload at a time.
    const errors = errorsOf(
      validateLocally(schema, { name: '', damage: 900, range: -1 }, key('all')),
    )
    const pointers = errors.map((error) => error.pointer)
    expect(pointers).toContain('/name')
    expect(pointers).toContain('/damage')
    expect(pointers).toContain('/range')
  })

  it('reports a value of the wrong type at the field it is in', () => {
    const errors = errorsOf(
      validateLocally(schema, { ...validDocument, damage: 'forty' }, key('type')),
    )
    expect(errors.map((error) => error.pointer)).toEqual(['/damage'])
  })

  it('points a pointer at the property it names, so a field can show it', () => {
    const errors = errorsOf(validateLocally(schema, { ...validDocument, damage: 900 }, key('max')))
    expect(errors).toHaveLength(1)
    expect(errors[0].pointer).toBe('/damage')
  })

  it('reports a property the schema does not define, and it is one no field can show', () => {
    const errors = errorsOf(
      validateLocally(schema, { ...validDocument, rarity: 'rare' }, key('extra')),
    )
    // `additionalProperties: false` reports against the object rather than the
    // key, so this is exactly the case unclaimedIssues exists for: the message
    // has to be listed somewhere or the document saves with a complaint no one
    // can see.
    expect(unclaimedIssues(errors, ['/name', '/damage', '/range', '/ammo'])).toEqual(errors)
  })

  it('reports a document that is not an object at all', () => {
    expect(validateLocally(schema, [1, 2], key('array')).kind).toBe('invalid')
  })

  it('admits that it has no opinion when the schema cannot be compiled', () => {
    // The dialect is the real risk this guards: a schema declaring a dialect the
    // validator was not asked for does not resolve, and the answer has to be "no
    // local opinion" rather than "valid".
    const result = validateLocally(
      { $ref: 'https://example.invalid/does-not-exist' },
      validDocument,
      key('uncompilable'),
    )
    expect(result.kind).toBe('unavailable')
    if (result.kind !== 'unavailable') throw new Error('expected unavailable')
    expect(result.reason).not.toBe('')
  })

  it('reports plain JSON Pointers for keys that need escaping or percent-encoding', () => {
    // The library's locations are URI fragments; the editor's markers are plain
    // pointers, so `max hp` must come back as `/max hp`, not `/max%20hp`.
    const keyed = {
      type: 'object',
      properties: {
        'max hp': { type: 'number' },
        'a/b': { type: 'number' },
        é: { type: 'number' },
        '100%': { type: 'number' },
      },
      required: ['needs space'],
    }
    const errors = errorsOf(
      validateLocally(keyed, { 'max hp': 'x', 'a/b': 'x', é: 'x', '100%': 'x' }, key('escaped')),
    )
    expect(errors.map((error) => error.pointer).sort()).toEqual(
      ['/100%', '/a~1b', '/max hp', '/needs space', '/é'].sort(),
    )
  })

  it('holds a constructed validator per key, so two calls agree', () => {
    const first = validateLocally(schema, validDocument, key('cached'))
    const second = validateLocally(schema, validDocument, key('cached'))
    expect(second).toEqual(first)
  })

  it('honours a boolean schema, which is valid JSON Schema and holds no properties', () => {
    expect(validateLocally(true, 'anything', key('true'))).toEqual({ kind: 'valid' })
    expect(validateLocally(false, 'anything', key('false')).kind).toBe('invalid')
  })
})

/**
 * The whole reason for the library swap. The app ships `script-src 'self'`, so
 * an implementation that reached for `new Function` (what Ajv did to compile a
 * schema) would be blocked in the browser and the editor's hints would die
 * exactly where the CSP was doing its job. Removing the constructor proves the
 * check is an interpreter: the valid, invalid and required paths all run.
 */
describe('validateLocally without the Function constructor', () => {
  const originalFunction = globalThis.Function

  afterEach(() => {
    globalThis.Function = originalFunction
  })

  it('produces the same answers when eval is unavailable', () => {
    globalThis.Function = (() => {
      throw new Error('blocked by Content-Security-Policy')
    }) as unknown as FunctionConstructor

    expect(validateLocally(schema, validDocument, key('no-eval-valid'))).toEqual({ kind: 'valid' })

    const invalid = errorsOf(
      validateLocally(schema, { ...validDocument, damage: 'forty' }, key('no-eval-invalid')),
    )
    expect(invalid.map((error) => error.pointer)).toEqual(['/damage'])

    const missing = errorsOf(
      validateLocally(schema, { damage: 1, range: 1 }, key('no-eval-required')),
    )
    expect(missing).toContainEqual({ pointer: '/name', message: 'is required' })
  })
})
