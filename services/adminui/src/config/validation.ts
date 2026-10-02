import { Validator, escapePointer, type OutputUnit, type Schema } from '@cfworker/json-schema'

import type { ValidationIssue } from '@/api/config'

/**
 * Local validation, for feedback while someone types.
 *
 * Config's `POST /namespaces/{ns}/draft/validate` is the authority, and the
 * editor calls it on save (CFG-B4). This is the instant second opinion that
 * makes a form usable, and the two are allowed to disagree in exactly one
 * direction: the server may refuse something this accepted, and then the server
 * wins. That direction matters for the options below.
 *
 * - **`@cfworker/json-schema`, because the page ships a `script-src 'self'` CSP
 *   with no `'unsafe-eval'`.** Ajv, which this replaced, compiled a schema with
 *   `new Function`; that is what a CSP without `unsafe-eval` exists to block, so
 *   the editor's hints would fail exactly where the strict policy was doing its
 *   job. This library walks the schema instead, so it needs no code generation
 *   and no eval at all.
 * - **draft 2020-12, which is what design/02-config.md section 2 names.** The
 *   validator is constructed with the `'2020-12'` dialect, the one every Config
 *   schema declares, so `$schema` and the 2020 keyword set are understood.
 * - **`shortCircuit: false`,** because a form that reports one problem at a time
 *   makes the reader fix them one reload at a time.
 * - **formats built in,** for PARITY rather than courtesy. The library checks the
 *   standard formats (`date-time`, `email`, `uri`, `ipv4`, `uuid`, …) itself, so
 *   a `format` is not silently ignored here and then enforced by the server's
 *   `santhosh-tekuri/jsonschema` on save, producing an error the local check
 *   never mentioned. The format set still is not identical between the two, and
 *   it does not have to be, because of the direction above.
 */

/**
 * Constructed validators, keyed by schema version.
 *
 * Constructing one dereferences the whole schema and is the expensive half, and
 * the schema changes rarely, so this is worth keeping. The cache is per module
 * rather than per editor: two tabs on the same namespace share a validator, and
 * a version is immutable, so an entry can never go stale.
 */
const validators = new Map<string, Validator | null>()

function validatorFor(schema: unknown, cacheKey: string): Validator | null {
  const cached = validators.get(cacheKey)
  if (cached !== undefined) return cached

  let validator: Validator | null
  try {
    validator = new Validator(schema as Schema | boolean, '2020-12', false)
  } catch {
    // A schema this library cannot dereference is not a validation failure: it is
    // the absence of a local opinion. Recorded as null so the failure is not
    // retried on every keystroke, and reported as `unavailable` by the caller.
    validator = null
  }
  validators.set(cacheKey, validator)
  return validator
}

export type LocalValidation =
  /** The document satisfies the schema, as far as this can tell. */
  | { kind: 'valid' }
  | { kind: 'invalid'; errors: ValidationIssue[] }
  /** No local opinion. The server's answer is the only one there is. */
  | { kind: 'unavailable'; reason: string }

const UNAVAILABLE = 'the schema could not be prepared in the browser'

export function validateLocally(
  schema: unknown,
  document: unknown,
  cacheKey: string,
): LocalValidation {
  const validator = validatorFor(schema, cacheKey)
  if (validator === null) return { kind: 'unavailable', reason: UNAVAILABLE }

  let result: { valid: boolean; errors: OutputUnit[] }
  try {
    result = validator.validate(document)
  } catch {
    // An unresolved `$ref` is the common case: it is only discovered while the
    // document is being walked, not when the validator is constructed. That is
    // still "no local opinion", never "invalid".
    return { kind: 'unavailable', reason: UNAVAILABLE }
  }

  if (result.valid) return { kind: 'valid' }
  return { kind: 'invalid', errors: toIssues(result.errors) }
}

/**
 * The library's errors as Config's.
 *
 * Two shapes have to be reconciled. The server and the UI speak JSON Pointer
 * (`/damage`, `/tags/0`), while this library reports an `instanceLocation` as a
 * URI fragment (`#`, `#/damage`, `#/max%20hp`), so the `#` is stripped and the
 * percent-encoding undone.
 *
 * The second is structural. Ajv reported one error per failing keyword; this
 * library wraps a group of leaf errors in a "Property X does not match schema"
 * error at the parent. Those wrappers are dropped when a more specific child
 * error exists, and errors that land on the same pointer are collapsed to the
 * first, which is the more specific one. `additionalProperties` is the exception:
 * there the wrapper is the only report that names the object, so it is kept and
 * the boolean-schema error beneath it (`False boolean schema.`) is dropped
 * instead.
 *
 * The one adjustment is `required`. The library reports it against the OBJECT
 * that is missing the property, so a missing top-level field would arrive at `/`
 * and render as a document-level complaint with no control to fix. Pointing it at
 * the absent property puts the message under the field that needs filling in,
 * which is where the reader is looking.
 */
function toIssues(errors: OutputUnit[]): ValidationIssue[] {
  const issues: ValidationIssue[] = []
  const seen = new Set<string>()
  for (const error of dropRedundant(errors)) {
    const issue = toIssue(error)
    if (seen.has(issue.pointer)) continue
    seen.add(issue.pointer)
    issues.push(issue)
  }
  return issues
}

/** Group errors whose message repeats the leaf error they wrap. */
const WRAPPER_KEYWORDS = new Set([
  'properties',
  'patternProperties',
  'allOf',
  'anyOf',
  'oneOf',
  'if',
  'then',
  'else',
  'dependentSchemas',
  'dependencies',
  '$ref',
  '$recursiveRef',
  'contains',
  'unevaluatedItems',
  'prefixItems',
  'items',
])

/** Group errors that are the only report naming a failing property. */
const ADDITIONAL_KEYWORDS = new Set(['additionalProperties', 'unevaluatedProperties'])

function dropRedundant(errors: OutputUnit[]): OutputUnit[] {
  const dropped = new Set<OutputUnit>()
  const keptAdditional = new Set<OutputUnit>()

  for (const error of errors) {
    if (WRAPPER_KEYWORDS.has(error.keyword)) {
      if (errors.some((other) => other !== error && nestedWithin(error, other))) dropped.add(error)
    }
    if (ADDITIONAL_KEYWORDS.has(error.keyword)) {
      // The interpreter does not mark a defined property as evaluated when its
      // own schema fails, so it is reported here again. When a specific leaf
      // error sits beneath it, that leaf is the one to show.
      const repeatsLeaf = errors.some(
        (other) => other !== error && other.keyword !== 'false' && nestedWithin(error, other),
      )
      if (repeatsLeaf) dropped.add(error)
      else keptAdditional.add(error)
    }
  }

  // An additional-properties wrapper that survived is the report for an extra
  // key; the boolean-schema error under it is the same problem twice.
  for (const wrapper of keptAdditional) {
    for (const error of errors) {
      if (error.keyword === 'false' && nestedWithin(wrapper, error)) dropped.add(error)
    }
  }

  return errors.filter((error) => !dropped.has(error))
}

/** Whether `child` points at something beneath `parent`'s location. */
function nestedWithin(parent: OutputUnit, child: OutputUnit): boolean {
  return child.instanceLocation.startsWith(`${parent.instanceLocation}/`)
}

function toIssue(error: OutputUnit): ValidationIssue {
  const pointer = pointerOf(error.instanceLocation)
  if (error.keyword === 'required') {
    const missing = missingProperty(error.error)
    if (missing !== null) {
      return { pointer: `${pointer}/${escapePointer(missing)}`, message: 'is required' }
    }
  }
  return { pointer, message: error.error }
}

/**
 * `#` is the root, `#/damage` is `/damage`. The library also runs each segment
 * through `encodeURI` (it is a URI fragment), so a key such as `max hp` arrives as
 * `max%20hp`; `decodeURI` is that call's exact inverse and gives back the plain
 * JSON Pointer the editor's markers use. `~0`/`~1` escaping is untouched by both.
 */
function pointerOf(instanceLocation: string): string {
  const fragment = instanceLocation.startsWith('#') ? instanceLocation.slice(1) : instanceLocation
  try {
    return decodeURI(fragment)
  } catch {
    return fragment
  }
}

/** The library puts the absent key in the message, not in params. */
function missingProperty(message: string): string | null {
  const match = /required property "([\s\S]*)"\.$/.exec(message)
  return match?.[1] ?? null
}
