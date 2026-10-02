import { ApiError } from '@/api/errors'

/**
 * Reading a response that arrived, but not as the shape the endpoint documents.
 *
 * A 200 with the wrong body means something other than the service answered: a
 * captive portal, a proxy error page rendered as JSON, or a deployment of the
 * wrong container. Left alone it becomes `undefined` somewhere in a component,
 * which reports a Vue warning about a property that does not exist and says
 * nothing about which endpoint lied. Checked here instead, at the boundary,
 * where the path is still in hand.
 *
 * The helpers below are deliberately shallow. They answer "is there a row to
 * render" and "what can be read off it", not "is this valid Config data": Config
 * validates its own documents with a JSON Schema and this is not that. A row
 * missing a field renders blank rather than taking the page down, which is what
 * a partially deployed backend should look like.
 */

export function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

export function wrongShape(path: string, expected: string): ApiError {
  return new ApiError({
    status: 200,
    code: 'internal',
    message: `${path} did not answer with ${expected}.`,
    requestId: null,
  })
}

/** The response body itself must be an object. */
export function objectIn(value: unknown, path: string): Record<string, unknown> {
  if (!isRecord(value)) throw wrongShape(path, 'an object')
  return value
}

/** An array of objects at `key`, which is the shape every list endpoint uses. */
export function recordsIn(value: unknown, key: string, path: string): Record<string, unknown>[] {
  return arrayIn(value, key, path).filter(isRecord)
}

export function arrayIn(value: unknown, key: string, path: string): unknown[] {
  const container = objectIn(value, path)[key]
  if (!Array.isArray(container)) throw wrongShape(path, `a \`${key}\` array`)
  return container
}

export function str(value: unknown, fallback = ''): string {
  return typeof value === 'string' ? value : fallback
}

export function num(value: unknown, fallback = 0): number {
  return typeof value === 'number' && Number.isFinite(value) ? value : fallback
}

/**
 * A number that is allowed to be absent.
 *
 * The overview's figures can be `null` when the query behind them failed or had
 * no data (DSH-C5's `degraded` list names exactly those). `num` would turn that
 * absence into a zero, which reads as a real measurement, so the reader that
 * cannot tell the difference uses this one instead.
 */
export function nullableNum(value: unknown): number | null {
  return typeof value === 'number' && Number.isFinite(value) ? value : null
}

export function bool(value: unknown, fallback = false): boolean {
  return typeof value === 'boolean' ? value : fallback
}

/** `null` rather than `''` where "absent" and "empty" mean different things. */
export function nullableStr(value: unknown): string | null {
  return typeof value === 'string' ? value : null
}
