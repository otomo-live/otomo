/* eslint-disable vue/one-component-per-file -- several small local stubs share this test file. */
import { flushPromises, mount, shallowMount } from '@vue/test-utils'
import { createPinia, setActivePinia, type Pinia } from 'pinia'
import { createMemoryHistory, createRouter, type RouteRecordRaw, type Router } from 'vue-router'
import { defineComponent, h, nextTick, watch } from 'vue'
import { afterEach, describe, expect, it, vi } from 'vitest'

import AppShell from '@/components/AppShell.vue'
import RoutePalette from '@/components/RoutePalette.vue'
import { environmentBanner } from '@/shell/environment'
import { useSessionStore } from '@/stores/session'

/**
 * The shell's environment banner and the route palette.
 *
 * Ionic is stubbed: these tests are about the shell's own logic and markup, and
 * mounting real Ion components would drag a shadow-DOM layout into a happy-dom
 * test for no extra coverage. The end-to-end suite is where Ionic renders for
 * real.
 */

const env = vi.hoisted(() => ({ environment: 'live' }))

vi.mock('@/api/env', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/api/env')>()
  return {
    ...actual,
    envConfig: () => ({ ...actual.envConfig(), environment: env.environment }),
  }
})

const Blank = defineComponent({ template: '<div />' })

function testRouter(): Router {
  const records: RouteRecordRaw[] = [
    { path: '/dashboard', component: Blank },
    { path: '/dashboard/logs', component: Blank },
    { path: '/dashboard/audit', component: Blank },
    { path: '/config', component: Blank },
  ]
  return createRouter({ history: createMemoryHistory(), routes: records })
}

function activePinia(): Pinia {
  const pinia = createPinia()
  setActivePinia(pinia)
  return pinia
}

function signedIn(roles: string[]): void {
  const store = useSessionStore()
  store.user = { id: 'usr_test', name: 'Test Operator', roles }
  store.status = 'authenticated'
}

/** An IonModal that renders its content only while open. */
const IonModalStub = defineComponent({
  name: 'IonModal',
  props: { isOpen: { type: Boolean, default: false } },
  emits: ['did-present', 'did-dismiss'],
  setup(props, { slots, emit }) {
    // Like the real modal: the content renders on open and did-present fires after.
    watch(
      () => props.isOpen,
      async (open) => {
        if (!open) return
        await nextTick()
        emit('did-present')
      },
    )
    return () => (props.isOpen ? h('div', { class: 'ion-modal' }, slots.default?.()) : null)
  },
})

afterEach(() => {
  env.environment = 'live'
})

describe('the environment banner', () => {
  it('maps each environment to a role and its wording', () => {
    expect(environmentBanner('live')).toEqual({
      kind: 'danger',
      text: 'LIVE — changes reach players',
    })
    expect(environmentBanner('staging')).toEqual({
      kind: 'warn',
      text: 'STAGING — changes do not reach players',
    })
    expect(environmentBanner('dev')).toEqual({ kind: 'info', text: 'dev — not live' })
  })

  it('renders the live banner with the danger class', () => {
    const pinia = activePinia()
    signedIn(['admin'])
    const wrapper = shallowMount(AppShell, {
      global: { plugins: [testRouter(), pinia] },
    })

    const banner = wrapper.get('[data-environment-banner]')
    expect(banner.text()).toContain('LIVE — changes reach players')
    expect(banner.classes()).toContain('shell__banner--danger')
    wrapper.unmount()
  })

  it('renders the staging and dev banners with their classes', () => {
    const pinia = activePinia()
    signedIn(['admin'])

    env.environment = 'staging'
    const staging = shallowMount(AppShell, { global: { plugins: [testRouter(), pinia] } })
    expect(staging.get('[data-environment-banner]').classes()).toContain('shell__banner--warn')
    expect(staging.get('[data-environment-banner]').text()).toContain('STAGING')
    staging.unmount()

    env.environment = 'dev'
    const dev = shallowMount(AppShell, { global: { plugins: [testRouter(), pinia] } })
    const banner = dev.get('[data-environment-banner]')
    expect(banner.classes()).toContain('shell__banner--info')
    expect(banner.text()).toContain('dev')
    dev.unmount()
  })
})

describe('the route palette', () => {
  function mountPalette(roles: string[]) {
    const pinia = activePinia()
    signedIn(roles)
    const router = testRouter()
    const Host = defineComponent({
      components: { RoutePalette },
      data: () => ({ open: false }),
      template: '<RoutePalette v-model:open="open" />',
    })
    const wrapper = mount(Host, {
      attachTo: document.body,
      global: {
        plugins: [router, pinia],
        stubs: { IonModal: IonModalStub },
      },
    })
    return { wrapper, router }
  }

  async function openWithCombo(): Promise<void> {
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'k', ctrlKey: true }))
    await flushPromises()
  }

  it('opens on Ctrl+K and focuses the search input', async () => {
    const { wrapper } = mountPalette(['admin'])

    expect(wrapper.find('[role="combobox"]').exists()).toBe(false)
    await openWithCombo()

    const input = wrapper.get('input[role="combobox"]')
    expect(input.attributes('aria-expanded')).toBe('true')
    expect(document.activeElement).toBe(input.element)
    wrapper.unmount()
  })

  it('filters by title and navigates on Enter', async () => {
    const { wrapper, router } = mountPalette(['admin'])
    await openWithCombo()

    await wrapper.get('input[role="combobox"]').setValue('logs')
    const options = wrapper.findAll('[role="option"]')
    expect(options).toHaveLength(1)
    expect(options[0].text()).toContain('Logs')
    expect(options[0].attributes('aria-selected')).toBe('true')

    await wrapper.get('input[role="combobox"]').trigger('keydown', { key: 'Enter' })
    await flushPromises()
    expect(router.currentRoute.value.path).toBe('/dashboard/logs')
    // Closed once it has navigated.
    expect(wrapper.find('[role="combobox"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('moves the highlight with the arrow keys', async () => {
    const { wrapper } = mountPalette(['admin'])
    await openWithCombo()

    expect(wrapper.get('[role="option"]').attributes('aria-selected')).toBe('true')

    await wrapper.get('input[role="combobox"]').trigger('keydown', { key: 'ArrowDown' })
    const options = wrapper.findAll('[role="option"]')
    expect(options[0].attributes('aria-selected')).toBe('false')
    expect(options[1].attributes('aria-selected')).toBe('true')
    expect(wrapper.get('input[role="combobox"]').attributes('aria-activedescendant')).toBe(
      options[1].attributes('id'),
    )
    wrapper.unmount()
  })

  it('closes on Escape', async () => {
    const { wrapper } = mountPalette(['admin'])
    await openWithCombo()
    expect(wrapper.find('[role="combobox"]').exists()).toBe(true)

    await wrapper.get('input[role="combobox"]').trigger('keydown', { key: 'Escape' })
    await nextTick()
    expect(wrapper.find('[role="combobox"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('offers a viewer every route its role reaches, including the namespace list', async () => {
    const { wrapper } = mountPalette(['viewer'])
    await openWithCombo()

    const text = wrapper.get('[role="listbox"]').text()
    expect(text).toContain('Logs')
    // GET /namespaces is a viewer read, so the list is in a viewer's palette.
    expect(text).toContain('Namespaces')
    wrapper.unmount()
  })
})
