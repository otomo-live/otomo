import { mount } from '@vue/test-utils'
import { flushPromises } from '@vue/test-utils'
import { h } from 'vue'
import { describe, expect, it, vi } from 'vitest'

import { CopyableId, DataTable, ErrorPanel, Grid, StatTile, StatusDot } from '@/components/ui'
import type { DataTableColumn } from '@/components/ui'

interface Row {
  id: number
  name: string
  count: number
}

const columns: DataTableColumn[] = [
  { key: 'name', label: 'Name' },
  { key: 'count', label: 'Count', align: 'end' },
]

const rows: Row[] = [
  { id: 1, name: 'Alpha', count: 3 },
  { id: 2, name: 'Beta', count: 5 },
]

function rowKey(row: unknown): number {
  return (row as Row).id
}

describe('DataTable', () => {
  it('renders the columns and the rows', () => {
    const wrapper = mount(DataTable, { props: { columns, rows, rowKey } })
    const headers = wrapper.findAll('thead th')
    expect(headers.map((header) => header.text())).toEqual(['Name', 'Count'])
    const bodyRows = wrapper.findAll('tbody tr')
    expect(bodyRows).toHaveLength(2)
    expect(wrapper.text()).toContain('Alpha')
    expect(wrapper.text()).toContain('Beta')
  })

  it('renders a per-column cell slot and falls back for the rest', () => {
    const wrapper = mount(DataTable, {
      props: { columns, rows, rowKey },
      slots: {
        'cell-name': (params: { row: unknown; value: unknown }) =>
          h('strong', { class: 'cell-name' }, `slot:${(params.row as Row).name}`),
      },
    })
    expect(wrapper.findAll('.cell-name')).toHaveLength(2)
    expect(wrapper.text()).toContain('slot:Alpha')
    // The column without a slot still renders its default value.
    expect(wrapper.text()).toContain('3')
  })

  it('renders the empty slot when there are no rows', () => {
    const wrapper = mount(DataTable, {
      props: { columns, rows: [], rowKey },
      slots: { empty: '<p class="empty-slot">Nothing here</p>' },
    })
    expect(wrapper.find('.empty-slot').exists()).toBe(true)
    expect(wrapper.findAll('tbody tr')).toHaveLength(1)
  })

  it('emits row-click with the row', async () => {
    const wrapper = mount(DataTable, { props: { columns, rows, rowKey } })
    await wrapper.findAll('tbody tr')[1].trigger('click')
    expect(wrapper.emitted('row-click')?.[0]).toEqual([rows[1]])
  })
})

describe('StatusDot', () => {
  it('uses the label as its accessible name and renders it', () => {
    const wrapper = mount(StatusDot, { props: { status: 'warn', label: 'Degraded' } })
    expect(wrapper.attributes('aria-label')).toBe('Degraded')
    expect(wrapper.text()).toContain('Degraded')
  })

  it('falls back to the status name and hides the text when compact', () => {
    const wrapper = mount(StatusDot, { props: { status: 'danger', compact: true } })
    expect(wrapper.attributes('aria-label')).toBe('danger')
    expect(wrapper.text()).toBe('')
  })
})

describe('StatTile', () => {
  it('renders the value, unit and label', () => {
    const wrapper = mount(StatTile, { props: { label: 'CPU', value: 42, unit: '%' } })
    expect(wrapper.text()).toContain('CPU')
    expect(wrapper.text()).toContain('42')
    expect(wrapper.text()).toContain('%')
  })
})

describe('CopyableId', () => {
  it('copies the value and shows Copied', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.defineProperty(navigator, 'clipboard', {
      value: { writeText },
      configurable: true,
    })
    const wrapper = mount(CopyableId, { props: { value: 'req-123' } })

    await wrapper.get('button').trigger('click')
    await flushPromises()

    expect(writeText).toHaveBeenCalledWith('req-123')
    expect(wrapper.text()).toContain('Copied')
  })

  it('degrades without throwing when the clipboard is unavailable', async () => {
    Object.defineProperty(navigator, 'clipboard', { value: undefined, configurable: true })
    const wrapper = mount(CopyableId, { props: { value: 'req-456' } })
    await wrapper.get('button').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('Copy')
  })
})

describe('ErrorPanel', () => {
  it('shows the COM-5 request id and emits retry', async () => {
    const wrapper = mount(ErrorPanel, {
      props: {
        error: { code: 'expired', message: 'Your session has ended.', request_id: 'req-789' },
        retryLabel: 'Try again',
      },
    })
    expect(wrapper.text()).toContain('Your session has ended.')
    expect(wrapper.text()).toContain('req-789')
    expect(wrapper.text()).toContain('expired')

    await wrapper.get('.error-panel__retry').trigger('click')
    expect(wrapper.emitted('retry')).toHaveLength(1)
  })

  it('accepts an ApiError-like object with a camelCase requestId', () => {
    const wrapper = mount(ErrorPanel, {
      props: { message: 'It broke.', detail: 'Server detail.', requestId: 'req-abc' },
    })
    expect(wrapper.text()).toContain('It broke.')
    expect(wrapper.text()).toContain('Server detail.')
    expect(wrapper.text()).toContain('req-abc')
  })
})

describe('Grid', () => {
  it('sets an auto-fill track from min and a token gap', () => {
    const wrapper = mount(Grid, { props: { min: 320, gap: 5 } })
    const style = wrapper.attributes('style') ?? ''
    expect(style).toContain('320px')
    expect(style).toContain('auto-fill')
    expect(style).toContain('var(--ds-space-5)')
  })
})
