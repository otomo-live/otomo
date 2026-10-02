import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter, type Router } from 'vue-router'
import { describe, expect, it } from 'vitest'

import { useSessionStore } from '@/stores/session'
import NamespaceListView from '@/views/config/NamespaceListView.vue'

/**
 * The namespace list's role gating and its URL-backed search.
 *
 * Ionic is stubbed: the interesting logic is which cells are links and which
 * controls exist for a role, and the browser rendering is the e2e suite's job.
 * The rows come from the shared fixture API, so the names are the real ones.
 */

const IONIC_STUBS = {
  IonPage: true,
  IonHeader: true,
  IonToolbar: true,
  IonMenuButton: true,
  IonContent: true,
  IonSpinner: true,
  IonButton: true,
}

async function mountList(roles: string[]): Promise<{ wrapper: VueWrapper; router: Router }> {
  const pinia = createPinia()
  setActivePinia(pinia)
  const store = useSessionStore()
  store.user = { id: 'usr_test', name: 'Test Operator', roles }
  store.status = 'authenticated'

  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/config', name: 'config-namespaces', component: NamespaceListView },
      {
        path: '/config/namespaces/:namespace',
        name: 'config-namespace-editor',
        component: { template: '<div />' },
      },
      {
        path: '/config/namespaces/:name/schema',
        name: 'config-namespace-schema',
        component: { template: '<div />' },
      },
    ],
  })
  await router.push('/config')
  await router.isReady()

  const wrapper = mount(NamespaceListView, {
    global: {
      plugins: [router, pinia],
      stubs: { ...IONIC_STUBS, CreateNamespaceDialog: true },
      renderStubDefaultSlot: true,
    },
  })
  await flushPromises()
  return { wrapper, router }
}

function rows(wrapper: VueWrapper) {
  return wrapper.findAll('.data-table__row')
}

describe('NamespaceListView role gating', () => {
  it('lets a viewer read the list, with the editor link, but no Create', async () => {
    const { wrapper } = await mountList(['viewer'])

    expect(rows(wrapper).length).toBeGreaterThan(0)
    // The editor route is `viewer`: a viewer follows the name to a read-only
    // editor, so the link is rendered for them too.
    expect(wrapper.findAll('a.namespaces__name').length).toBeGreaterThan(0)
    // The viewer-read schema page is linked as well.
    expect(wrapper.findAll('a.namespaces__schema').length).toBeGreaterThan(0)
    // Creating a namespace is an admin write, so it stays hidden.
    expect(wrapper.find('[data-testid="create-namespace"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('shows an admin the editor link and the Create control', async () => {
    const { wrapper } = await mountList(['admin'])

    expect(wrapper.findAll('a.namespaces__name').length).toBeGreaterThan(0)
    expect(wrapper.find('[data-testid="create-namespace"]').exists()).toBe(true)
    wrapper.unmount()
  })

  it('shows live_ops the editor link but not Create', async () => {
    const { wrapper } = await mountList(['live_ops'])

    expect(wrapper.findAll('a.namespaces__name').length).toBeGreaterThan(0)
    expect(wrapper.find('[data-testid="create-namespace"]').exists()).toBe(false)
    wrapper.unmount()
  })
})

describe('NamespaceListView search', () => {
  it('filters by name and keeps the term in the URL', async () => {
    const { wrapper, router } = await mountList(['admin'])
    const before = rows(wrapper).length

    await wrapper.get('[data-testid="namespace-search"]').setValue('presentation')
    await flushPromises()

    expect(router.currentRoute.value.query.q).toBe('presentation')
    const filtered = rows(wrapper)
    expect(filtered.length).toBeGreaterThan(0)
    expect(filtered.length).toBeLessThan(before)
    expect(filtered.every((row) => row.text().includes('presentation'))).toBe(true)
    wrapper.unmount()
  })

  it('reads the term back out of the URL on the way in', async () => {
    const pinia = createPinia()
    setActivePinia(pinia)
    const store = useSessionStore()
    store.user = { id: 'usr_test', name: 'Test Operator', roles: ['admin'] }
    store.status = 'authenticated'

    const router = createRouter({
      history: createMemoryHistory(),
      routes: [{ path: '/config', name: 'config-namespaces', component: NamespaceListView }],
    })
    await router.push('/config?q=weapons')
    await router.isReady()

    const wrapper = mount(NamespaceListView, {
      global: {
        plugins: [router, pinia],
        stubs: { ...IONIC_STUBS, CreateNamespaceDialog: true },
        renderStubDefaultSlot: true,
      },
    })
    await flushPromises()

    expect(wrapper.get<HTMLInputElement>('[data-testid="namespace-search"]').element.value).toBe(
      'weapons',
    )
    expect(rows(wrapper).every((row) => row.text().includes('weapons'))).toBe(true)
    wrapper.unmount()
  })
})
