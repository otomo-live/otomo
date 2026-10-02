import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter, type Router } from 'vue-router'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import { listPacks, uploadPack, type PackSummary } from '@/api/config'
import { useSessionStore } from '@/stores/session'
import PacksView from '@/views/config/PacksView.vue'

/**
 * The packs page's role gating and its two upload outcomes.
 *
 * `listPacks`/`uploadPack` are the module's only IO, and both are stubbed here so
 * the page can be pointed at a 200 (already uploaded) without a 512 MiB body; the
 * real request path is the client and fixture suites' job.
 */

vi.mock('@/api/config', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/api/config')>()
  return { ...actual, listPacks: vi.fn(), uploadPack: vi.fn() }
})

const mockList = vi.mocked(listPacks)
const mockUpload = vi.mocked(uploadPack)

const SEED: PackSummary = {
  packId: 'pack_seed_weapons',
  name: 'weapons_pack',
  sha256: '2b'.repeat(32),
  size: 2621440,
  uploadedBy: 'liveops@example.com',
  uploadedAt: '2026-09-20T11:30:00Z',
}

const IONIC_STUBS = {
  IonPage: true,
  IonHeader: true,
  IonToolbar: true,
  IonMenuButton: true,
  IonContent: true,
  IonSpinner: true,
  IonButton: true,
}

async function mountPage(roles: string[]): Promise<{ wrapper: VueWrapper; router: Router }> {
  const pinia = createPinia()
  setActivePinia(pinia)
  const store = useSessionStore()
  store.user = { id: 'usr_test', name: 'Test Operator', roles }
  store.status = 'authenticated'

  const router = createRouter({
    history: createMemoryHistory(),
    routes: [{ path: '/config/packs', name: 'config-packs', component: PacksView }],
  })
  await router.push('/config/packs')
  await router.isReady()

  const wrapper = mount(PacksView, {
    global: { plugins: [router, pinia], stubs: IONIC_STUBS, renderStubDefaultSlot: true },
  })
  await flushPromises()
  return { wrapper, router }
}

/** Put a real File on the hidden input, as the browser would. */
async function chooseFile(wrapper: VueWrapper, file: File): Promise<void> {
  const input = wrapper.get('[data-testid="pack-file-input"]')
  Object.defineProperty(input.element, 'files', { value: [file], configurable: true })
  await input.trigger('change')
  await flushPromises()
}

function gdpcFile(name: string, tag: number): File {
  return new File([Uint8Array.from([0x47, 0x44, 0x50, 0x43, tag])], name)
}

beforeEach(() => {
  mockList.mockResolvedValue([SEED])
  mockUpload.mockReset()
})

describe('PacksView role gating', () => {
  it('lets a viewer read the list but hides the upload panel', async () => {
    const { wrapper } = await mountPage(['viewer'])

    expect(wrapper.findAll('.data-table__row').length).toBe(1)
    expect(wrapper.find('[data-testid="pack-upload-submit"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="pack-file-input"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('shows a live_ops operator the upload controls', async () => {
    const { wrapper } = await mountPage(['live_ops'])

    expect(wrapper.find('[data-testid="pack-file-input"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="pack-upload-submit"]').exists()).toBe(true)
    wrapper.unmount()
  })
})

describe('PacksView upload results', () => {
  it('shows Uploaded for a 201 and the pack SHA', async () => {
    mockUpload.mockResolvedValue({
      pack: { ...SEED, packId: 'pack_new', sha256: 'ab'.repeat(32) },
      created: true,
    })
    const { wrapper } = await mountPage(['live_ops'])
    await chooseFile(wrapper, gdpcFile('weapons_pack.pck', 1))
    await wrapper.get('[data-testid="pack-upload-submit"]').trigger('click')
    await flushPromises()

    const result = wrapper.get('[data-testid="pack-result"]')
    expect(result.text()).toContain('Uploaded')
    expect(result.text()).not.toContain('Already uploaded')
    wrapper.unmount()
  })

  it('shows "Already uploaded" for a 200 and names the existing pack', async () => {
    mockUpload.mockResolvedValue({ pack: { ...SEED, name: 'audio_pack' }, created: false })
    const { wrapper } = await mountPage(['live_ops'])
    await chooseFile(wrapper, gdpcFile('weapons_pack.pck', 2))
    await wrapper.get('[data-testid="pack-upload-submit"]').trigger('click')
    await flushPromises()

    expect(wrapper.get('[data-testid="pack-result"]').text()).toContain(
      'Already uploaded — same bytes as audio_pack',
    )
    wrapper.unmount()
  })

  it('refuses a file without the GDPC header before uploading', async () => {
    const { wrapper } = await mountPage(['live_ops'])
    await chooseFile(
      wrapper,
      new File([Uint8Array.from([0x50, 0x4b, 0x03, 0x04])], 'not_a_pack.pck'),
    )
    await flushPromises()

    expect(wrapper.get('[data-testid="pack-file-error"]').text()).toContain('GDPC')
    expect(wrapper.get('[data-testid="pack-upload-submit"]').attributes('disabled')).toBeDefined()
    expect(mockUpload).not.toHaveBeenCalled()
    wrapper.unmount()
  })
})
