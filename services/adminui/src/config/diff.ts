/**
 * The local document diff, in Config's own shape.
 *
 * Two diffs exist and they are not the same question:
 *
 *  - "what changed between version 11 and the draft" is a question about the
 *    STORE, and Config answers it at `GET /namespaces/{ns}/diff`. The SPA calls
 *    that and renders whatever it says.
 *  - "what changed between the draft I loaded and the draft in THIS editor" is
 *    not a question about the store at all, because the editor's copy was never
 *    saved. Only the SPA can answer it, and this is where.
 *
 * The output is deliberately the same shape Config returns, `{pointer, kind,
 * before, after}`, so one component renders both and a reader does not have to
 * learn two vocabularies for the same idea. That is also why the fixture API's
 * `/diff` endpoint imports this file rather than keeping a second implementation:
 * a fixture that disagreed with the app about the shape of a diff would hide
 * exactly the kind of disagreement the fixture exists to catch.
 *
 * jsondiffpatch was the plan's tool here. Its delta is a different structure
 * (nested, with move and text-diff markers) that would have to be flattened into
 * this one anyway, and the flattening has to reconstruct the before and after
 * values that a delta does not carry at the top level. A walker that produces the
 * wanted shape directly is smaller than the flattening, and it is what the reader
 * and the conflict dialog both consume.
 */

export type ChangeKind = 'added' | 'removed' | 'changed'

export interface DocumentChange {
  /** A JSON Pointer into the document; `''` is the document itself. */
  pointer: string
  kind: ChangeKind
  before?: unknown
  after?: unknown
}

/**
 * Containers are walked, everything else is compared.
 *
 * Arrays and objects are walked the same way, by key, and an array's keys are
 * its indices. The difference from `isRecord` in api/shape.ts is deliberate:
 * that one exists to tell the two apart, and here the distinction does not
 * matter. What DOES matter is that a container on one side and a scalar on the
 * other is a change rather than a walk of the scalar's keys.
 */
function isContainer(value: unknown): value is unknown[] | Record<string, unknown> {
  return typeof value === 'object' && value !== null
}

/**
 * An array seen as a record, so one walker serves both.
 *
 * `Object.keys` on an array already yields its indices as strings; this is only
 * telling the type system what the runtime does. Duplicating the walker for
 * arrays would be the alternative, and two walkers is how a diff ends up
 * treating `[1,2]` by a different rule than `{"0":1,"1":2}`.
 */
function asEntries(value: unknown[] | Record<string, unknown>): Record<string, unknown> {
  return Array.isArray(value) ? { ...value } : value
}

/**
 * Compares two documents and reports every leaf that differs.
 *
 * Keys are unioned rather than zipped, so a key present on one side only is an
 * `added` or a `removed` and not a `changed` against undefined. `Object.is` rather
 * than `===` for the leaf comparison, so that NaN compared with NaN is the same
 * value: it is, in JSON's absence of NaN and in a document that has been round
 * tripped through `JSON.parse`.
 */
export function diffDocuments(before: unknown, after: unknown, pointer = ''): DocumentChange[] {
  if (isContainer(before) && isContainer(after)) {
    const left = asEntries(before)
    const right = asEntries(after)
    const keys = new Set([...Object.keys(left), ...Object.keys(right)])
    const changes: DocumentChange[] = []
    for (const key of keys) {
      const child = `${pointer}/${key}`
      const had = Object.hasOwn(left, key)
      const has = Object.hasOwn(right, key)
      if (!had) changes.push({ pointer: child, kind: 'added', after: right[key] })
      else if (!has) changes.push({ pointer: child, kind: 'removed', before: left[key] })
      else changes.push(...diffDocuments(left[key], right[key], child))
    }
    return changes
  }

  return Object.is(before, after) ? [] : [{ pointer, kind: 'changed', before, after }]
}

/**
 * `/weapons/0/damage` as `weapons[0].damage`.
 *
 * A JSON Pointer is the right thing to send and a poor thing to read, and this
 * is the only place the conversion happens. `~1` and `~0` are the escapes JSON
 * Pointer defines for `/` and `~`; a key containing a slash is rare and a key
 * containing a slash that silently rendered as a path separator would be a
 * confusing bug to chase.
 */
export function formatPointer(pointer: string): string {
  if (pointer === '') return '(document)'
  return pointer
    .split('/')
    .filter((segment) => segment !== '')
    .map((segment) => segment.replaceAll('~1', '/').replaceAll('~0', '~'))
    .reduce((path, segment) => {
      if (/^\d+$/.test(segment)) return `${path}[${segment}]`
      return path === '' ? segment : `${path}.${segment}`
    }, '')
}

/** A value as one short line, for a row in a list of changes. */
export function describeValue(value: unknown): string {
  if (value === undefined) return 'not set'
  const text = JSON.stringify(value)
  if (text === undefined) return String(value)
  return text.length > 80 ? `${text.slice(0, 77)}...` : text
}

// ---------------------------------------------------------------------------
// Config's wire diff
//
// Config's `/diff` endpoint emits JSON-Patch-like changes (`{op, path, from,
// to}`) while this module's walker emits `{pointer, kind, before, after}`. The
// two describe the same fact in two vocabularies, and the SPA needs both: the
// local walker for "what am I about to save", the service's for "what changed
// between these versions". The conversion lives here so the mock, which builds
// its wire response from the walker, and the client, which reads either shape,
// cannot disagree about the vocabulary.
// ---------------------------------------------------------------------------

export type DiffOp = 'add' | 'remove' | 'replace'

export interface DiffChange {
  /** Config's op vocabulary. `replace` reads as "changed" in the UI. */
  op: DiffOp
  /** A JSON Pointer, in Config's wire form. */
  path: string
  from?: unknown
  to?: unknown
}

function kindToOp(kind: ChangeKind): DiffOp {
  if (kind === 'added') return 'add'
  if (kind === 'removed') return 'remove'
  return 'replace'
}

/** The walker's changes in Config's `{op, path, from, to}` wire vocabulary. */
export function documentDiff(from: unknown, to: unknown): DiffChange[] {
  return diffDocuments(from, to).map((change) => {
    const op = kindToOp(change.kind)
    if (op === 'add') return { op, path: change.pointer, to: change.after }
    if (op === 'remove') return { op, path: change.pointer, from: change.before }
    return { op, path: change.pointer, from: change.before, to: change.after }
  })
}

/**
 * A JSON Pointer split into its decoded segments.
 *
 * `~1` and `~0` are the escapes JSON Pointer defines for `/` and `~`; a key
 * containing a slash is rare and one that silently rendered as a path separator
 * would be a confusing bug to chase.
 */
export function pointerSegments(pointer: string): string[] {
  if (pointer === '') return []
  return pointer
    .split('/')
    .filter((segment) => segment !== '')
    .map((segment) => segment.replaceAll('~1', '/').replaceAll('~0', '~'))
}

/**
 * A pointer as a path a person can follow, with an explicit separator rather
 * than a bare slash or dot: `weapons › 0 › damage`. The empty pointer names the
 * document itself.
 */
export function formatPointerPath(pointer: string): string {
  const segments = pointerSegments(pointer)
  return segments.length === 0 ? '(document)' : segments.join(' › ')
}

/**
 * The first segment of a pointer, which is the key a change groups under in the
 * diff viewer. The document itself is its own group.
 */
export function topLevelKey(pointer: string): string {
  return pointerSegments(pointer)[0] ?? '(document)'
}

/** The human label for an op, which is the word a badge shows. */
export function opLabel(op: string): string {
  if (op === 'add') return 'added'
  if (op === 'remove') return 'removed'
  return 'changed'
}
