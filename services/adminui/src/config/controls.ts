import { isRecord } from '@/api/shape'

/**
 * Which control a schema fragment gets.
 *
 * This is the one place the SPA decides what a JSON Schema type MEANS to a
 * person, and it is deliberately small. Everything it does not understand falls
 * to the raw JSON editor, which can express any document at all: a schema the
 * form cannot lay out is then still editable, which is the property that keeps
 * this from being a limitation of the product rather than of one control.
 *
 * So the fallback is not `string`, and it is not `object`. A `type` this does
 * not recognise, an untyped property, a union, an array of objects and a nested
 * object all get the raw editor, because guessing a control for any of them
 * would write a value of the wrong shape into the document.
 */

export type ControlKind =
  | 'enum'
  | 'boolean'
  | 'integer'
  | 'number'
  | 'date'
  | 'string'
  /** An array whose items are all scalars: a small repeater of one control. */
  | 'scalars'
  /** Anything else: the raw JSON editor for this field alone. */
  | 'json'

const SCALAR_TYPES = new Set(['string', 'number', 'integer', 'boolean'])

export function controlFor(schema: Record<string, unknown>): ControlKind {
  // A closed set of choices is a better control than any text field, whatever
  // the underlying type, so it is checked before `type` is even read.
  if (Array.isArray(schema.enum)) return 'enum'

  switch (schema.type) {
    case 'boolean':
      return 'boolean'
    case 'integer':
      return 'integer'
    case 'number':
      return 'number'
    case 'string':
      // Only `date`. A `date-time` is an RFC 3339 string with an offset and a
      // native datetime-local input cannot produce one, so a control that
      // looked like the right one would quietly write a string the schema's
      // own `format` rejects.
      return schema.format === 'date' ? 'date' : 'string'
    case 'array': {
      const items = schema.items
      if (!isRecord(items)) return 'json'
      if (Array.isArray(items.enum)) return 'scalars'
      return typeof items.type === 'string' && SCALAR_TYPES.has(items.type) ? 'scalars' : 'json'
    }
    default:
      return 'json'
  }
}

export interface EnumOption {
  /** The member as JSON text: what the control carries, and what is parsed back. */
  token: string
  label: string
}

/**
 * The `enum` as options.
 *
 * The control carries the member's JSON TEXT rather than its index, so a
 * reordered or renamed option cannot silently change what a saved value means.
 * `enumNames` is JSON Schema's own vocabulary for the label and is used when it
 * is there and the same length; otherwise the member's own text is the label,
 * and `null` is spelled out because "null" as a label reads like the string.
 */
export function enumOptions(schema: Record<string, unknown>): EnumOption[] {
  const members = Array.isArray(schema.enum) ? schema.enum : []
  const names = Array.isArray(schema.enumNames) ? schema.enumNames : []
  return members.map((member, index) => {
    const named = names.length === members.length ? names[index] : undefined
    return {
      token: JSON.stringify(member ?? null) ?? 'null',
      label: typeof named === 'string' ? named : readableMember(member),
    }
  })
}

function readableMember(member: unknown): string {
  if (member === null) return 'null'
  if (typeof member === 'string') return member
  return JSON.stringify(member) ?? String(member)
}

/** The member a token stands for, or `undefined` if the token is not JSON. */
export function memberForToken(token: string): unknown {
  try {
    return JSON.parse(token)
  } catch {
    return undefined
  }
}

/** The token for a value in the document, so the control can select it. */
export function tokenForValue(value: unknown): string | undefined {
  if (value === undefined) return undefined
  return JSON.stringify(value ?? null) ?? undefined
}

/** What a scalar control shows for a value, without inventing one. */
export function textFor(value: unknown): string {
  if (value === undefined || value === null) return ''
  if (typeof value === 'string') return value
  if (typeof value === 'number' || typeof value === 'boolean') return String(value)
  return JSON.stringify(value) ?? ''
}

export type NumberOutcome =
  /** A value the document can hold. */
  | { kind: 'value'; value: number }
  /** The field was emptied, which removes the key. */
  | { kind: 'empty' }
  /**
   * Typed so far but not yet a number, or text JSON has no way to hold: `-`,
   * `1e`, `0x10`, `Infinity`. Not an error, yet, and deliberately not `empty`
   * either: the field keeps what was typed and writes nothing.
   */
  | { kind: 'incomplete' }

/**
 * A number field's text as a value.
 *
 * `incomplete` is the reason this returns three things instead of one. A person
 * typing `-12` passes through `-`, which is not a number, and a control that
 * treated that as "remove the field" would delete the value the moment they
 * typed a minus sign.
 */
export function numberFrom(text: string, integer: boolean): NumberOutcome {
  const trimmed = text.trim()
  if (trimmed === '') return { kind: 'empty' }

  // Number() accepts what JSON does not: `0x10`, `Infinity`, and whitespace it
  // has already trimmed. The pattern is the document's rule, not the form's.
  if (!/^[+-]?(\d+\.?\d*|\.\d+)([eE][+-]?\d+)?$/.test(trimmed)) return { kind: 'incomplete' }

  const value = Number(trimmed)
  if (!Number.isFinite(value)) return { kind: 'incomplete' }
  return { kind: 'value', value: integer ? Math.trunc(value) : value }
}
