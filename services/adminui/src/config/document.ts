import type { ValidationIssue } from '@/api/config'

/**
 * The document as an editable value.
 *
 * The reconciliation rule for the raw-JSON escape hatch is that THE PARSED
 * DOCUMENT IS THE SINGLE SOURCE OF TRUTH, always. The form writes into it, the
 * raw tab parses into it, and there is never a parallel form model that could
 * drift from what the server holds. This module is the small set of pure
 * functions that rule needs, kept out of the view so they can be tested without
 * mounting one.
 */

/** Two-space indentation, which is what a human reads and what Config stores. */
export function serialise(document: unknown): string {
  return JSON.stringify(document ?? null, null, 2) ?? 'null'
}

/**
 * A document that shares nothing with its original.
 *
 * The editor keeps two of them: what the server gave it, and what is on screen.
 * They have to be separate objects, because a diff between one object and itself
 * is empty however much has been typed into it. A round trip through JSON is the
 * right depth of copy for a document that IS JSON, and it is available in every
 * environment this runs in, which `structuredClone` is not.
 */
export function clone(document: unknown): unknown {
  return JSON.parse(serialise(document)) as unknown
}

export type ParseOutcome =
  | { kind: 'document'; document: Record<string, unknown> }
  | { kind: 'unparsable'; message: string }
  /**
   * Valid JSON whose root is not an object. A form needs named properties to
   * have fields at all, so this stays in the raw tab. It is a separate case from
   * `unparsable` because the two need different sentences: one is a syntax
   * error with a position, the other is a document the form cannot lay out.
   */
  | { kind: 'not-an-object'; message: string }

export function parseRaw(text: string): ParseOutcome {
  let parsed: unknown
  try {
    parsed = JSON.parse(text)
  } catch (error) {
    // JSON.parse's own message carries the position, which is the only useful
    // thing to say about a syntax error.
    return { kind: 'unparsable', message: error instanceof Error ? error.message : 'invalid JSON' }
  }

  if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
    return {
      kind: 'not-an-object',
      message: 'The document must be a JSON object: the form renders its properties.',
    }
  }
  return { kind: 'document', document: parsed as Record<string, unknown> }
}

/**
 * One field's value, or `undefined` when the document does not carry it.
 *
 * Absent and `null` are different states and stay different: an optional field
 * left alone is absent, which is what lets a schema's `default` apply, and only
 * what the editor actually writes enters the document.
 */
export function fieldOf(document: unknown, name: string): unknown {
  if (typeof document !== 'object' || document === null || Array.isArray(document)) return undefined
  return (document as Record<string, unknown>)[name]
}

/**
 * A copy of the document with one field set. `undefined` removes the key rather
 * than storing it, because `JSON.stringify` drops an undefined value anyway and
 * a key that survives in memory and vanishes on save is a lie about what will be
 * stored.
 *
 * A copy rather than a mutation, so a reader that compares the document with the
 * one it loaded (the unsaved-changes list, the conflict dialog) cannot be
 * looking at the same object twice.
 */
export function withField(
  document: unknown,
  name: string,
  value: unknown,
): Record<string, unknown> {
  const next: Record<string, unknown> = isPlainObject(document) ? { ...document } : {}
  if (value === undefined) delete next[name]
  else next[name] = value
  return next
}

function isPlainObject(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

/**
 * The messages that belong to one pointer.
 *
 * A field shows an issue whose pointer is the field itself (`/damage`) or
 * anything beneath it (`/damage/0`, `/tags/2`). The local validator and Config's
 * `POST /draft/validate` both report in exactly this form, so the two agree
 * without translation. The pointer is passed in rather than
 * derived from a name because an array item's field is `/tags/0`, not `/tags`.
 */
export function messagesFor(issues: ValidationIssue[], pointer: string): string[] {
  return issues.filter((issue) => belongsTo(issue, pointer)).map((issue) => issue.message)
}

/**
 * The issues no field will show.
 *
 * Not simply the ones at the root: an issue can point at a property the form
 * does not render at all, which happens when a document carries a key the schema
 * does not define. Those have to be listed somewhere or the document would save
 * with a complaint no one can see.
 */
export function unclaimedIssues(issues: ValidationIssue[], pointers: string[]): ValidationIssue[] {
  return issues.filter((issue) => pointers.every((pointer) => !belongsTo(issue, pointer)))
}

function belongsTo(issue: ValidationIssue, pointer: string): boolean {
  return issue.pointer === pointer || issue.pointer.startsWith(`${pointer}/`)
}
