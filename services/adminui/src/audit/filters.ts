/**
 * The audit page's filters, kept as URL state rather than component state.
 *
 * The same shape the log explorer uses, and for the same reason: the link is the
 * query, so a filtered trail can be pasted into a ticket and a reload lands on
 * exactly what was on screen.
 */

/** The sources the merged feed knows. Anything else is not offered. */
export const AUDIT_SOURCES = ['config', 'admin-auth', 'session'] as const

export interface AuditFilters {
  /** Empty means every source. */
  source: string
  /** Matched by id or by name, the service's rule. */
  actor: string
  /** A `datetime-local` value, matching the input that writes it. */
  from: string
  to: string
}

function readOne(value: unknown): string {
  return typeof value === 'string' ? value : ''
}

export function parseAuditFilters(query: Record<string, unknown>): AuditFilters {
  return {
    source: readOne(query.source),
    actor: readOne(query.actor),
    from: readOne(query.from),
    to: readOne(query.to),
  }
}

/** Only the set filters go in the URL; an empty one is the absence of a filter. */
export function auditFiltersQuery(filters: AuditFilters): Record<string, string> {
  const query: Record<string, string> = {}
  if (filters.source !== '') query.source = filters.source
  if (filters.actor !== '') query.actor = filters.actor
  if (filters.from !== '') query.from = filters.from
  if (filters.to !== '') query.to = filters.to
  return query
}
