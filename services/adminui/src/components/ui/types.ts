/**
 * Types shared by the design-system primitives.
 *
 * They live in a plain module rather than in one of the SFCs so that a component
 * (and a consumer importing from `@/components/ui`) can name them without
 * pulling a second component's compiled script in.
 */

/** The status roles the design system paints with. */
export type StatusKind = 'ok' | 'warn' | 'danger' | 'info' | 'neutral'

/** One column of a `DataTable`. */
export interface DataTableColumn {
  /** The row property to render by default, and the stem of the cell slot name. */
  key: string
  label: string
  align?: 'start' | 'center' | 'end'
  /** Any CSS width, e.g. `12rem` or `80px`. */
  width?: string
  /** Render the default cell in monospace: ids, codes, pointers. */
  mono?: boolean
}
