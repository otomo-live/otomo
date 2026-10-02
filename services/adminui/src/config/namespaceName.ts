/**
 * The namespace-name rule, mirroring Config's own.
 *
 * Config rejects a create whose name does not match
 * `^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$` (services/config/internal/api/namespaces.go),
 * and the browser is where a typo is cheapest to fix. The regexp is copied with
 * the same anchoring so the two cannot disagree about a name the server would
 * accept: a looser one here offers the Create button for a name that then 400s,
 * and a stricter one refuses a name Config would take.
 */

/** The slug rule: dot-separated segments of lowercase letters, digits, underscores. */
export const NAMESPACE_NAME_PATTERN = /^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$/

/** The one example the create form shows, kept next to the regexp it illustrates. */
export const NAMESPACE_NAME_EXAMPLE = 'e.g. balance.weapons'

/** True when Config's create endpoint would accept this name. */
export function isValidNamespaceName(name: string): boolean {
  return NAMESPACE_NAME_PATTERN.test(name)
}

/**
 * The reason a name cannot be submitted, or null when it can.
 *
 * The checks run in Config's order (empty, then length, then the pattern) so the
 * browser and the server name the same first problem for the same input.
 */
export function namespaceNameProblem(name: string): string | null {
  if (name === '') return 'Name is required.'
  if (name.length > 64) return 'Name must be at most 64 characters.'
  if (!isValidNamespaceName(name)) {
    return `Use lowercase letters, digits and underscores, separated by dots (${NAMESPACE_NAME_EXAMPLE}).`
  }
  return null
}
