import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { createMemoryHistory, createRouter, type Router } from 'vue-router'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import { getPreviousRelease, getRelease, type Release, type ReleaseManifest } from '@/api/config'
import ReleaseDiffView from '@/views/config/ReleaseDiffView.vue'

/**
 * The release diff page. The two manifests are read from two releases and
 * diffed locally, so the reads are stubbed and the real diff walker runs: the
 * server section under test is the one the page renders.
 */

vi.mock('@/api/config', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/api/config')>()
  return {
    ...actual,
    getRelease: vi.fn(),
    getPreviousRelease: vi.fn(),
  }
})

const mockGetRelease = vi.mocked(getRelease)
const mockPrevious = vi.mocked(getPreviousRelease)

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

function manifest(serverTuning: number | null): ReleaseManifest {
  return {
    format: 1,
    channel: 'dev',
    releaseId: 1,
    createdAt: '2026-09-20T00:00:00Z',
    minClientVersion: '1.0.0',
    config: {},
    packs: [],
    ...(serverTuning === null
      ? {}
      : {
          config: { 'server.tuning': { version: serverTuning, sha256: 'aa', size: 10 } },
        }),
  }
}

function release(id: number, serverManifest: ReleaseManifest | null): Release {
  return {
    releaseId: id,
    channel: 'dev',
    manifestSha256: 'ff'.repeat(32),
    serverManifestSha256: serverManifest === null ? null : 'ee'.repeat(32),
    minClientVersion: '1.0.0',
    message: `release ${id}`,
    createdBy: 'admin@example.com',
    createdAt: '2026-09-20T00:00:00Z',
    manifest: {
      format: 1,
      channel: 'dev',
      releaseId: id,
      createdAt: '2026-09-20T00:00:00Z',
      minClientVersion: '1.0.0',
      config: { 'balance.weapons': { version: id, sha256: '33'.repeat(32), size: 640 } },
      packs: [],
    },
    serverManifest,
  }
}

async function mountView(): Promise<{ wrapper: VueWrapper; router: Router }> {
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [{ path: '/config/releases/:channel/:releaseId', component: { template: '<div />' } }],
  })
  await router.push('/config/releases/dev/2')
  await router.isReady()

  const wrapper = mount(ReleaseDiffView, {
    global: { plugins: [router], stubs: IONIC_STUBS, renderStubDefaultSlot: true },
  })
  await flushPromises()
  await flushPromises()
  return { wrapper, router }
}

beforeEach(() => {
  mockGetRelease.mockReset()
  mockPrevious.mockReset()
})

describe('ReleaseDiffView server manifest', () => {
  it('shows the server diff against the previous release server manifest', async () => {
    mockGetRelease.mockResolvedValue(release(2, manifest(2)))
    mockPrevious.mockResolvedValue(release(1, manifest(1)))

    const { wrapper } = await mountView()

    expect(wrapper.text()).toContain('Server manifest')
    expect(wrapper.text()).toContain('server.tuning')
    expect(wrapper.text()).toContain('1 → 2')
    expect(wrapper.find('[data-testid="release-server-empty"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('says when a release predates server manifests', async () => {
    mockGetRelease.mockResolvedValue(release(2, null))
    mockPrevious.mockResolvedValue(release(1, null))

    const { wrapper } = await mountView()

    expect(wrapper.get('[data-testid="release-server-empty"]').text()).toContain(
      'No server manifest (released before server manifests existed)',
    )
    wrapper.unmount()
  })
})
