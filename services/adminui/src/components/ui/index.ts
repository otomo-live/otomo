/**
 * The admin UI design system.
 *
 * Every view composes these primitives rather than reaching for a colour or a
 * layout of its own. `tokens.css` is the single source of the values they read.
 */

export { default as Breadcrumbs } from '@/components/ui/Breadcrumbs.vue'
export { default as CopyableId } from '@/components/ui/CopyableId.vue'
export { default as DataTable } from '@/components/ui/DataTable.vue'
export { default as EmptyState } from '@/components/ui/EmptyState.vue'
export { default as ErrorPanel } from '@/components/ui/ErrorPanel.vue'
export { default as Grid } from '@/components/ui/Grid.vue'
export { default as PageHeader } from '@/components/ui/PageHeader.vue'
export { default as Panel } from '@/components/ui/Panel.vue'
export { default as StatTile } from '@/components/ui/StatTile.vue'
export { default as StatusDot } from '@/components/ui/StatusDot.vue'
export { default as Toolbar } from '@/components/ui/Toolbar.vue'

export type { DataTableColumn, StatusKind } from '@/components/ui/types'
