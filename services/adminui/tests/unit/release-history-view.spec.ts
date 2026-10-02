import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter, type Router } from 'vue-router'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import {
  getChannelHead,
  getRelease,
  listReleaseHistory,
  promoteRelease,
  rollbackRelease,
  type ReleaseHistoryPage,
  type ReleaseHistoryRow,
} from '@/api/config'
import { ApiError } from '@/api/errors'
import StaleReleaseDialog from '@/config/StaleReleaseDialog.vue'
import { useSessionStore } from '@/stores/session'
import ReleaseHistoryView from '@/views/config/ReleaseHistoryView.vue'

/**
 * The release history page, with its reads and mutations stubbed.
 *
 * The states worth pinning down are the ones a mounted page cannot fake: the
 * head is a distinct id from the newest row after a rollback, paging appends,
 * the admin-only controls do not render for a live_ops/viewer session, and both
 * mutations branch on the two refusals the server documents.
 */

vi.mock('@/api/config', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/api/config')>()
  return {
    ...actual,
    listReleaseHistory: vi.fn(),
    getChannelHead: vi.fn(),
    getRelease: vi.fn(),
    rollbackRelease: vi.fn(),
    promoteRelease: vi.fn(),
  }
})

const mockHistory = vi.mocked(listReleaseHistory)
const mockHead = vi.mocked(getChannelHead)
const mockGetRelease = vi.mocked(getRelease)
const mockRollback = vi.mocked(rollbackRelease)
const mockPromote = vi.mocked(promoteRelease)

const AUDIO_SHA = '22'.repeat(32)

function release(id: number, channel: string, isHead = false): ReleaseHistoryRow {
  const manifest = {
    format: 1,
    channel,
    releaseId: id,
    createdAt: '2026-09-20T00:00:00Z',
    minClientVersion: id >= 3 ? '1.1.0' : '1.0.0',
    config: { 'balance.weapons': { version: id, sha256: '33'.repeat(32), size: 640 } },
    packs: [{ name: 'audio.pak', sha256: AUDIO_SHA, size: 4096 }],
  }
  return {
    releaseId: id,
    channel,
    manifestSha256: 'ff'.repeat(32),
    serverManifestSha256: null,
    minClientVersion: manifest.minClientVersion,
    message: `release ${id}`,
    createdBy: 'admin@example.com',
    createdAt: '2026-09-20T00:00:00Z',
    isHead,
    manifest,
    serverManifest: null,
  }
}

function page(
  headReleaseId: number,
  ids: number[],
  nextBefore: number | null = null,
): ReleaseHistoryPage {
  return {
    headReleaseId,
    releases: ids.map((id) => release(id, 'dev', id === headReleaseId)),
    nextBefore,
  }
}

const IONIC_STUBS = {
  IonPage: true,
  IonHeader: true,
  IonToolbar: true,
  IonMenuButton: true,
  IonContent: true,
  IonSpinner: true,
  IonButton: true,
  IonModal: true,
  IonTitle: true,
  IonButtons: true,
}

const routes = [
  { path: '/config/releases', name: 'config-releases', component: ReleaseHistoryView },
  {
    path: '/config/releases/:channel/compose',
    name: 'config-release-compose',
    component: { template: '<div />' },
  },
  {
    path: '/config/releases/:channel/:releaseId',
    name: 'config-release-diff',
    component: { template: '<div />' },
  },
]

async function mountView(
  query: string,
  roles: string[],
): Promise<{ wrapper: VueWrapper; router: Router }> {
  const pinia = createPinia()
  setActivePinia(pinia)
  const store = useSessionStore()
  store.user = { id: 'usr_test', name: 'Test Operator', roles }
  store.status = 'authenticated'

  const router = createRouter({ history: createMemoryHistory(), routes })
  await router.push(`/config/releases${query}`)
  await router.isReady()

  const wrapper = mount(ReleaseHistoryView, {
    global: {
      plugins: [router, pinia],
      stubs: IONIC_STUBS,
      renderStubDefaultSlot: true,
    },
  })
  await flushPromises()
  await flushPromises()
  return { wrapper, router }
}

function rollbackButtons(wrapper: VueWrapper) {
  return wrapper.findAll('[data-testid="rollback-release"]')
}

beforeEach(() => {
  mockHistory.mockReset()
  mockHead.mockReset()
  mockGetRelease.mockReset()
  mockRollback.mockReset()
  mockPromote.mockReset()

  mockHistory.mockImplementation(async (channel: string) => {
    if (channel === 'staging') {
      return { headReleaseId: 1, releases: [release(1, 'staging', true)], nextBefore: null }
    }
    if (channel === 'live') {
      return {
        headReleaseId: 2,
        releases: [release(2, 'live', true), release(1, 'live')],
        nextBefore: null,
      }
    }
    return page(3, [3, 2, 1])
  })
  mockHead.mockImplementation(async (channel: string) => {
    if (channel === 'staging') {
      return { headReleaseId: 1, release: release(1, 'staging', true) }
    }
    if (channel === 'live') {
      return { headReleaseId: 2, release: release(2, 'live', true) }
    }
    return { headReleaseId: 3, release: release(3, 'dev', true) }
  })
  mockGetRelease.mockImplementation(async (channel: string, id: number) =>
    release(id, channel, true),
  )
})

describe('ReleaseHistoryView tabs', () => {
  it('reads the channel from the URL and follows a tab click', async () => {
    const { wrapper, router } = await mountView('?channel=dev', ['viewer'])

    expect(mockHistory).toHaveBeenCalledWith('dev', expect.objectContaining({ limit: 10 }))
    expect(wrapper.get('[data-testid="channel-tab-dev"]').classes()).toContain(
      'releases__tab--active',
    )

    await wrapper.get('[data-testid="channel-tab-staging"]').trigger('click')
    await flushPromises()
    await flushPromises()

    expect(router.currentRoute.value.query.channel).toBe('staging')
    expect(mockHistory).toHaveBeenLastCalledWith('staging', expect.objectContaining({ limit: 10 }))
    expect(wrapper.get('[data-testid="channel-tab-staging"]').classes()).toContain(
      'releases__tab--active',
    )
    wrapper.unmount()
  })

  it('defaults an absent channel to dev without trusting an unknown one', async () => {
    const { wrapper } = await mountView('?channel=nightly', ['viewer'])
    expect(mockHistory).toHaveBeenCalledWith('dev', expect.anything())
    wrapper.unmount()
  })
})

describe('ReleaseHistoryView head and history', () => {
  it('marks by head_release_id rather than by the newest row', async () => {
    mockHistory.mockResolvedValue(page(2, [3, 2, 1]))
    const { wrapper } = await mountView('?channel=dev', ['viewer'])

    expect(wrapper.get('[data-testid="head-release-id"]').text()).toBe('2')
    const badges = wrapper.findAll('[data-testid="release-head-badge"]')
    expect(badges).toHaveLength(1)
    expect(badges[0].element.closest('tr')?.textContent).toContain('2')
    wrapper.unmount()
  })

  it('appends the older page and hides Load older at the end', async () => {
    mockHistory.mockReset()
    mockHistory.mockResolvedValueOnce(page(3, [3, 2], 2)).mockResolvedValueOnce(page(3, [1], null))
    const { wrapper } = await mountView('?channel=dev', ['viewer'])

    expect(wrapper.findAll('.data-table__row')).toHaveLength(2)
    await wrapper.get('[data-testid="load-older"]').trigger('click')
    await flushPromises()

    expect(mockHistory).toHaveBeenLastCalledWith('dev', { before: 2, limit: 10 })
    expect(wrapper.findAll('.data-table__row')).toHaveLength(3)
    expect(wrapper.find('[data-testid="load-older"]').exists()).toBe(false)
    wrapper.unmount()
  })
})

describe('ReleaseHistoryView role gating', () => {
  it('shows a live_ops session neither rollback nor promote', async () => {
    const { wrapper } = await mountView('?channel=staging', ['live_ops'])
    expect(rollbackButtons(wrapper)).toHaveLength(0)
    expect(wrapper.find('[data-testid="promote-release"]').exists()).toBe(false)
    // The composer is still reachable for dev/staging.
    expect(wrapper.find('[data-testid="compose-release"]').exists()).toBe(true)
    wrapper.unmount()
  })

  it('shows a viewer neither mutation nor the composer', async () => {
    const { wrapper } = await mountView('?channel=staging', ['viewer'])
    expect(rollbackButtons(wrapper)).toHaveLength(0)
    expect(wrapper.find('[data-testid="promote-release"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="compose-release"]').exists()).toBe(false)
    wrapper.unmount()
  })
})

describe('ReleaseHistoryView live confirmation', () => {
  it('keeps a live rollback disabled until exactly live is typed', async () => {
    const { wrapper } = await mountView('?channel=live', ['admin'])

    await rollbackButtons(wrapper)[0].trigger('click')
    await flushPromises()

    expect(wrapper.get('[data-testid="rollback-summary"]').text()).toContain('from release 2')
    const confirm = wrapper.get('[data-testid="rollback-confirm"]')
    expect(confirm.attributes('disabled')).toBeDefined()

    await wrapper.get('[data-testid="rollback-live-confirm"]').setValue('Live')
    expect(confirm.attributes('disabled')).toBeDefined()

    await wrapper.get('[data-testid="rollback-live-confirm"]').setValue('live')
    expect(confirm.attributes('disabled')).toBeUndefined()
    wrapper.unmount()
  })

  it('requires a message and the word live to promote to live', async () => {
    const { wrapper } = await mountView('?channel=live', ['admin'])

    await wrapper.get('[data-testid="promote-release"]').trigger('click')
    await flushPromises()

    const confirm = wrapper.get('[data-testid="promote-confirm"]')
    expect(confirm.attributes('disabled')).toBeDefined()

    await wrapper.get('[data-testid="promote-message"]').setValue('Ship the balance pass')
    expect(confirm.attributes('disabled')).toBeDefined()

    await wrapper.get('[data-testid="promote-live-confirm"]').setValue('live')
    expect(confirm.attributes('disabled')).toBeUndefined()
    wrapper.unmount()
  })
})

describe('ReleaseHistoryView refusals', () => {
  it('opens the stale dialog when the head moved and refreshes on review', async () => {
    mockRollback.mockRejectedValue(
      new ApiError({
        status: 409,
        code: 'stale_release',
        message: 'channel dev has moved from release 3 to 4',
        requestId: 'req_test',
      }),
    )
    const { wrapper } = await mountView('?channel=dev', ['admin'])

    await rollbackButtons(wrapper)[0].trigger('click')
    await flushPromises()
    await wrapper.get('[data-testid="rollback-confirm"]').trigger('click')
    await flushPromises()

    const dialog = wrapper.findComponent(StaleReleaseDialog)
    expect(dialog.exists()).toBe(true)
    expect(dialog.props('from')).toBe(3)
    expect(dialog.props('to')).toBe(4)

    const loadsBefore = mockHistory.mock.calls.length
    dialog.vm.$emit('review')
    await flushPromises()
    expect(mockHistory.mock.calls.length).toBeGreaterThan(loadsBefore)
    wrapper.unmount()
  })

  it('shows Already the head on no_changes rather than the stale dialog', async () => {
    mockRollback.mockRejectedValue(
      new ApiError({
        status: 409,
        code: 'no_changes',
        message: 'head is that release',
        requestId: null,
      }),
    )
    const { wrapper } = await mountView('?channel=dev', ['admin'])

    await rollbackButtons(wrapper)[0].trigger('click')
    await flushPromises()
    await wrapper.get('[data-testid="rollback-confirm"]').trigger('click')
    await flushPromises()

    expect(wrapper.get('[data-testid="release-no-changes"]').text()).toContain('Already the head')
    expect(wrapper.findComponent(StaleReleaseDialog).exists()).toBe(false)
    wrapper.unmount()
  })
})

describe('ReleaseHistoryView refresh', () => {
  it('refetches and shows the new head after a successful rollback', async () => {
    mockHistory.mockReset()
    mockHistory.mockResolvedValueOnce(page(3, [3, 2, 1])).mockResolvedValueOnce(page(2, [3, 2, 1]))
    const moved = release(2, 'dev', true)
    mockRollback.mockResolvedValue(moved)
    const { wrapper } = await mountView('?channel=dev', ['admin'])

    await rollbackButtons(wrapper)[0].trigger('click')
    await flushPromises()
    await wrapper.get('[data-testid="rollback-confirm"]').trigger('click')
    await flushPromises()

    expect(mockRollback).toHaveBeenCalledWith('dev', { release_id: 2, base_release_id: 3 })
    expect(wrapper.get('[data-testid="head-release-id"]').text()).toBe('2')
    expect(wrapper.get('[data-testid="release-toast"]').text()).toContain(
      'Rolled back dev to release 2',
    )
    expect(mockHistory).toHaveBeenCalledTimes(2)
    wrapper.unmount()
  })

  it('promotes the source head with the message and refetches', async () => {
    mockHistory.mockReset()
    mockHistory
      .mockResolvedValueOnce({
        headReleaseId: 1,
        releases: [release(1, 'staging', true)],
        nextBefore: null,
      })
      .mockResolvedValueOnce({
        headReleaseId: 4,
        releases: [release(4, 'staging', true), release(1, 'staging')],
        nextBefore: null,
      })
    mockPromote.mockResolvedValue(release(4, 'staging', true))
    const { wrapper } = await mountView('?channel=staging', ['admin'])

    await wrapper.get('[data-testid="promote-release"]').trigger('click')
    await flushPromises()
    await wrapper.get('[data-testid="promote-message"]').setValue('Promote the balance pass')
    await wrapper.get('[data-testid="promote-confirm"]').trigger('click')
    await flushPromises()

    expect(mockPromote).toHaveBeenCalledWith('staging', 'dev', {
      base_release_id: 1,
      message: 'Promote the balance pass',
    })
    expect(wrapper.get('[data-testid="head-release-id"]').text()).toBe('4')
    expect(wrapper.get('[data-testid="release-toast"]').text()).toContain('Promoted dev release 3')
    wrapper.unmount()
  })
})
