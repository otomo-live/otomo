import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter, type Router } from 'vue-router'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import {
  getChannelHead,
  listNamespaces,
  listPacks,
  listVersions,
  publishRelease,
  type NamespaceSummary,
  type PackSummary,
  type Release,
  type VersionPage,
} from '@/api/config'
import { ApiError } from '@/api/errors'
import StaleReleaseDialog from '@/config/StaleReleaseDialog.vue'
import { useSessionStore } from '@/stores/session'
import ReleaseComposeView from '@/views/config/ReleaseComposeView.vue'

/**
 * The composer page, with its five reads and its publish stubbed.
 *
 * The page's own logic is selection and preview, so the fixtures are literals: a
 * client namespace at v11, another at v6, a server-only one, and a head that
 * carries the first two plus two packs. The real diff walker runs, so the preview
 * under test is the one the page renders rather than a second implementation.
 */

vi.mock('@/api/config', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/api/config')>()
  return {
    ...actual,
    getChannelHead: vi.fn(),
    listNamespaces: vi.fn(),
    listVersions: vi.fn(),
    listPacks: vi.fn(),
    publishRelease: vi.fn(),
  }
})

const mockHead = vi.mocked(getChannelHead)
const mockNamespaces = vi.mocked(listNamespaces)
const mockVersions = vi.mocked(listVersions)
const mockPacks = vi.mocked(listPacks)
const mockPublish = vi.mocked(publishRelease)

const AUDIO_SHA = '22'.repeat(32)
const WEAPONS_SHA = '11'.repeat(32)

function namespace(name: string, audience: string, latestVersion: number): NamespaceSummary {
  return {
    name,
    audience,
    description: '',
    latestVersion,
    draft: { revision: 1, updatedAt: '2026-09-20T00:00:00Z', hasUnpublishedChanges: false },
  }
}

function versionPage(versions: number[]): VersionPage {
  return {
    versions: versions.map((version) => ({
      version,
      schemaVersion: 1,
      sha256: `${version}`.padStart(64, '0'),
      message: `v${version}`,
      createdBy: 'admin@example.com',
      createdAt: '2026-09-20T00:00:00Z',
    })),
    nextBefore: null,
  }
}

const BALANCE = namespace('balance.weapons', 'client', 11)
const UI = namespace('ui.presentation', 'client', 6)
const SERVER = namespace('server.tuning', 'server', 2)

function release(id: number, channel: string): Release {
  return {
    releaseId: id,
    channel,
    manifestSha256: 'ff'.repeat(32),
    serverManifestSha256: 'ee'.repeat(32),
    minClientVersion: '1.1.0',
    message: `release ${id}`,
    createdBy: 'admin@example.com',
    createdAt: '2026-09-20T00:00:00Z',
    manifest: {
      format: 1,
      channel,
      releaseId: id,
      createdAt: '2026-09-20T00:00:00Z',
      minClientVersion: '1.1.0',
      config: {
        'balance.weapons': { version: 11, sha256: '33'.repeat(32), size: 640 },
        'ui.presentation': { version: 6, sha256: '66'.repeat(32), size: 256 },
      },
      packs: [
        { name: 'audio.pak', sha256: AUDIO_SHA, size: 4096 },
        { name: 'weapons.pak', sha256: WEAPONS_SHA, size: 2048 },
      ],
    },
    serverManifest: {
      format: 1,
      channel,
      releaseId: id,
      createdAt: '2026-09-20T00:00:00Z',
      minClientVersion: '1.1.0',
      config: {
        'server.tuning': { version: 2, sha256: 'aa'.repeat(32), size: 128 },
      },
      packs: [],
    },
  }
}

const PACKS: PackSummary[] = [
  {
    packId: 'p1',
    name: 'audio.pak',
    sha256: AUDIO_SHA,
    size: 4096,
    uploadedBy: 'admin@example.com',
    uploadedAt: '2026-09-18T09:00:00Z',
  },
  {
    packId: 'p2',
    name: 'weapons.pak',
    sha256: WEAPONS_SHA,
    size: 2048,
    uploadedBy: 'liveops@example.com',
    uploadedAt: '2026-09-20T11:30:00Z',
  },
]

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

async function mountComposer(
  channel: string,
  roles: string[],
): Promise<{ wrapper: VueWrapper; router: Router }> {
  const pinia = createPinia()
  setActivePinia(pinia)
  const store = useSessionStore()
  store.user = { id: 'usr_test', name: 'Test Operator', roles }
  store.status = 'authenticated'

  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      {
        path: '/config/releases/:channel/compose',
        name: 'config-release-compose',
        component: ReleaseComposeView,
      },
      {
        path: '/config/releases/:channel/:releaseId',
        name: 'config-release-diff',
        component: { template: '<div />' },
      },
    ],
  })
  await router.push(`/config/releases/${channel}/compose`)
  await router.isReady()

  const wrapper = mount(ReleaseComposeView, {
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

function publishButton(wrapper: VueWrapper) {
  return wrapper.get('[data-testid="compose-publish"]')
}

beforeEach(() => {
  mockHead.mockResolvedValue({ headReleaseId: 3, release: release(3, 'dev') })
  mockNamespaces.mockResolvedValue([BALANCE, UI, SERVER])
  mockVersions.mockImplementation(async (namespaceName) => {
    if (namespaceName === 'balance.weapons') return versionPage([11, 10])
    if (namespaceName === 'server.tuning') return versionPage([2, 1])
    return versionPage([6])
  })
  mockPacks.mockResolvedValue(PACKS)
  mockPublish.mockReset()
})

describe('ReleaseComposeView defaults', () => {
  it('defaults every namespace to its latest version and pre-checks the head packs', async () => {
    const { wrapper } = await mountComposer('dev', ['live_ops'])

    expect(
      wrapper.get<HTMLSelectElement>('[data-testid="compose-version-balance.weapons"]').element
        .value,
    ).toBe('11')
    expect(
      wrapper.get<HTMLSelectElement>('[data-testid="compose-version-ui.presentation"]').element
        .value,
    ).toBe('6')
    expect(
      wrapper.get<HTMLSelectElement>('[data-testid="compose-version-server.tuning"]').element.value,
    ).toBe('2')

    const checks = wrapper.findAll<HTMLInputElement>('[data-testid="compose-pack"] input')
    expect(checks).toHaveLength(2)
    expect(checks.every((check) => check.element.checked)).toBe(true)
    wrapper.unmount()
  })

  it('groups a server namespace under its own heading with a version select', async () => {
    const { wrapper } = await mountComposer('dev', ['live_ops'])

    const group = wrapper.get('[data-testid="compose-server-group"]')
    expect(group.text()).toContain('Server namespaces')
    expect(group.text()).toContain('sent to game servers and Session only, never to clients')
    expect(
      group.get<HTMLSelectElement>('[data-testid="compose-version-server.tuning"]').element.value,
    ).toBe('2')
    wrapper.unmount()
  })

  it('disables publish and says nothing would change when the draft equals the head', async () => {
    const { wrapper } = await mountComposer('dev', ['live_ops'])

    expect(wrapper.get('[data-testid="compose-nothing"]').text()).toContain('Nothing would change')
    expect(publishButton(wrapper).attributes('disabled')).toBeDefined()
    wrapper.unmount()
  })

  it('still sees nothing to change on a head released before server manifests existed', async () => {
    // Every release on a deployment that predates server manifests looks like this.
    // Its null server manifest must not read as a min-client-version change.
    mockHead.mockResolvedValue({
      headReleaseId: 3,
      release: { ...release(3, 'dev'), serverManifest: null, serverManifestSha256: null },
    })
    mockNamespaces.mockResolvedValue([BALANCE, UI])
    const { wrapper } = await mountComposer('dev', ['live_ops'])

    expect(wrapper.get('[data-testid="compose-nothing"]').text()).toContain('Nothing would change')
    expect(publishButton(wrapper).attributes('disabled')).toBeDefined()
    wrapper.unmount()
  })
})

describe('ReleaseComposeView validation', () => {
  it('rejects a min client version that is not three dot-separated numbers', async () => {
    const { wrapper } = await mountComposer('dev', ['live_ops'])

    await wrapper.get('[data-testid="compose-min-client"]').setValue('1.1')
    expect(wrapper.get('[data-testid="compose-min-hint"]').text()).toContain('three dot-separated')
    expect(publishButton(wrapper).attributes('disabled')).toBeDefined()

    await wrapper.get('[data-testid="compose-min-client"]').setValue('1.2.0')
    expect(wrapper.find('[data-testid="compose-min-hint"]').exists()).toBe(false)
    wrapper.unmount()
  })
})

describe('ReleaseComposeView live confirmation', () => {
  it('keeps publish disabled until exactly the word live is typed', async () => {
    const { wrapper } = await mountComposer('live', ['admin'])

    // Make the draft differ from the head so the only remaining gate is the word.
    await wrapper.get('[data-testid="compose-message"]').setValue('Ship it')
    await wrapper
      .get<HTMLSelectElement>('[data-testid="compose-version-balance.weapons"]')
      .setValue('10')
    await flushPromises()
    expect(publishButton(wrapper).attributes('disabled')).toBeDefined()

    await wrapper.get('[data-testid="compose-live-confirm"]').setValue('Live')
    expect(publishButton(wrapper).attributes('disabled')).toBeDefined()

    await wrapper.get('[data-testid="compose-live-confirm"]').setValue('live')
    expect(publishButton(wrapper).attributes('disabled')).toBeUndefined()
    wrapper.unmount()
  })

  it('refuses to compose live for a live_ops session', async () => {
    const { wrapper } = await mountComposer('live', ['live_ops'])
    expect(wrapper.get('[data-testid="compose-live-denied"]').text()).toContain('administrators')
    wrapper.unmount()
  })
})

describe('ReleaseComposeView publish', () => {
  async function readyToPublish(wrapper: VueWrapper): Promise<void> {
    await wrapper.get('[data-testid="compose-message"]').setValue('Move balance 11 to 10')
    await wrapper
      .get<HTMLSelectElement>('[data-testid="compose-version-balance.weapons"]')
      .setValue('10')
    await flushPromises()
  }

  it('opens the stale dialog on 409 and names where the channel moved', async () => {
    mockPublish.mockRejectedValue(
      new ApiError({
        status: 409,
        code: 'stale_release',
        message: 'channel dev has moved from release 3 to 4',
        requestId: 'req_test',
      }),
    )
    const { wrapper } = await mountComposer('dev', ['live_ops'])
    await readyToPublish(wrapper)

    await publishButton(wrapper).trigger('click')
    await flushPromises()

    const dialog = wrapper.findComponent(StaleReleaseDialog)
    expect(dialog.exists()).toBe(true)
    expect(dialog.props('channel')).toBe('dev')
    expect(dialog.props('from')).toBe(3)
    expect(dialog.props('to')).toBe(4)
    expect(wrapper.text()).toContain('Someone published to dev since you opened this')
    expect(wrapper.text()).toContain('Review the new head')
    wrapper.unmount()
  })

  it('navigates to the new release on 201', async () => {
    mockPublish.mockResolvedValue(release(4, 'dev'))
    const { wrapper, router } = await mountComposer('dev', ['live_ops'])
    await readyToPublish(wrapper)

    await publishButton(wrapper).trigger('click')
    await flushPromises()

    expect(mockPublish).toHaveBeenCalledWith(
      'dev',
      expect.objectContaining({
        base_release_id: 3,
        min_client_version: '1.1.0',
        message: 'Move balance 11 to 10',
      }),
    )
    expect(router.currentRoute.value.fullPath).toBe('/config/releases/dev/4')
    wrapper.unmount()
  })

  it('enables publish and previews a server-only change, sending it in versions', async () => {
    mockPublish.mockResolvedValue(release(4, 'dev'))
    const { wrapper } = await mountComposer('dev', ['live_ops'])

    await wrapper.get('[data-testid="compose-message"]').setValue('Retune the servers')
    await wrapper
      .get<HTMLSelectElement>('[data-testid="compose-version-server.tuning"]')
      .setValue('1')
    await flushPromises()

    expect(wrapper.get('[data-testid="compose-preview-server"]').text()).toContain('changed')
    expect(publishButton(wrapper).attributes('disabled')).toBeUndefined()

    await publishButton(wrapper).trigger('click')
    await flushPromises()

    expect(mockPublish).toHaveBeenCalledWith(
      'dev',
      expect.objectContaining({
        versions: expect.arrayContaining([{ namespace: 'server.tuning', version: 1 }]),
      }),
    )
    wrapper.unmount()
  })
})
