import { mount } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'

import DiffView from '@/config/DiffView.vue'
import type { DiffChange } from '@/config/diff'

/**
 * The presentational diff: grouping by top-level key, the op badge, the path
 * rendering and the empty case. The documents are passed in already fetched, so
 * this suite needs no server and no router.
 */

const CHANGES: DiffChange[] = [
  { op: 'replace', path: '/damage', from: 40, to: 45 },
  { op: 'add', path: '/ammo', to: 6 },
  { op: 'remove', path: '/range', from: 18.5 },
  { op: 'replace', path: '/nested/deep', from: 1, to: 2 },
]

function mountDiff(changes: DiffChange[], fromDocument: unknown = {}, toDocument: unknown = {}) {
  return mount(DiffView, {
    props: { changes, fromRef: '11', toRef: 'draft', fromDocument, toDocument },
  })
}

describe('DiffView', () => {
  it('renders one row per change and groups by top-level key', () => {
    const wrapper = mountDiff(CHANGES)

    expect(wrapper.findAll('[data-testid="diff-row"]')).toHaveLength(4)
    const groups = wrapper.findAll('[data-testid="diff-group"]')
    expect(groups.map((group) => group.find('h3').text())).toEqual([
      'damage',
      'ammo',
      'range',
      'nested',
    ])
  })

  it('renders the op badge vocabulary', () => {
    const wrapper = mountDiff(CHANGES)
    expect(wrapper.findAll('[data-testid="diff-op"]').map((badge) => badge.text())).toEqual([
      'changed',
      'added',
      'removed',
      'changed',
    ])
  })

  it('renders a JSON pointer as a path a person can follow', () => {
    const wrapper = mountDiff(CHANGES)
    const paths = wrapper.findAll('.diff__path').map((path) => path.text())
    expect(paths).toContain('damage')
    expect(paths).toContain('nested › deep')
  })

  it('says so when there are no differences', () => {
    const wrapper = mountDiff([])
    expect(wrapper.get('[data-testid="diff-empty"]').text()).toContain('No differences')
    expect(wrapper.findAll('[data-testid="diff-row"]')).toHaveLength(0)
  })

  it('offers side-by-side only when both documents were supplied', () => {
    const without = mount(DiffView, {
      props: { changes: [], fromRef: '11', toRef: 'draft' },
    })
    expect(without.find('[data-testid="diff-side-by-side"]').exists()).toBe(false)

    const withDocs = mountDiff([{ op: 'replace', path: '/a', from: 1, to: 2 }], { a: 1 }, { a: 2 })
    expect(withDocs.find('[data-testid="diff-side-by-side"]').exists()).toBe(true)
  })
})
