/**
 * URL hygiene for the two values that arrive from the address bar: the
 * `returnTo` a guard writes, and the `area` the denied screen echoes back.
 *
 * Both end up somewhere that makes an unchecked value dangerous. `returnTo` is
 * handed to the router, and `area` is rendered as text. Neither is exploitable
 * on its own, which is exactly why the check belongs in one place with a name
 * rather than inline at each call site.
 */

/** Long enough for any real route, short enough that a crafted one is noise. */
const MAX_PATH_LENGTH = 512

/**
 * The value if it is a path within this app, otherwise null.
 *
 * `//evil.example` is rejected along with anything not starting with a slash:
 * a browser reads a leading `//` as a protocol-relative URL, so an orchestrator
 * or a router that treats it as internal is one redirect away from sending the
 * user, and any token in the fragment, to another origin.
 */
export function sanitiseInternalPath(value: unknown): string | null {
  if (typeof value !== 'string') return null
  if (value.length === 0 || value.length > MAX_PATH_LENGTH) return null
  if (!value.startsWith('/')) return null
  if (value.startsWith('//')) return null
  // A backslash is normalised to a slash by some browsers, so `/\evil.example`
  // reaches the same place as `//evil.example`.
  if (value.startsWith('/\\')) return null
  return value
}
