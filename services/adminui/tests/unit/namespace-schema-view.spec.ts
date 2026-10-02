/* eslint-disable vue/one-component-per-file -- the host and the editor stub share this test file. */
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { http, HttpResponse } from 'msw'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter, RouterView, type Router } from 'vue-router'
import { defineComponent, h } from 'vue'
import { describe, expect, it, vi } from 'vitest'

import { server } from '@/mocks/server'
import { useSessionStore } from '@/stores/session'
import NamespaceSchemaView from '@/views/config/NamespaceSchemaView.vue'

/**
 * The schema page: who may edit, what a refused save shows, and the
 * lost-update guard.
 *
 * The CodeMirror editor is replaced by a textarea stub that keeps the same
 * contract (a `modelValue` string, a `readonly` flag, an `update:modelValue`
 * event) so the page's logic can be driven without measuring a real editor. The
 * API is the shared fixture server.
 */

const IONIC_STUBS = {
  IonPage: true,
  IonHeader: true,
  IonToolbar: true,
  IonMenuButton: true,
  IonContent: true,
  IonSpinner: true,
  IonButton: true,
  IonButtons: true,
  IonBackButton: true,
}

const RawJsonEditorStub = defineComponent({
  name: 'RawJsonEditor',
  props: {
    modelValue: { type: String, default: '' },
    readonly: { type: Boolean, default: false },
    ariaLabel: { type: String, default: '' },
  },
  emits: ['update:modelValue'],
  setup(props, { emit }) {
    return () =>
      h('textarea', {
        'data-testid': 'raw-editor',
        'aria-label': props.ariaLabel,
        readonly: props.readonly,
        value: props.modelValue,
        onInput: (event: Event) =>
          emit('update:modelValue', (event.target as HTMLTextAreaElement).value),
      })
  },
})

const Host = defineComponent({ components: { RouterView }, template: '<router-view />' })

interface Mounted {
  wrapper: VueWrapper
  router: Router
  view: VueWrapper
}

async function mountSchema(roles: string[]): Promise<Mounted> {
  const pinia = createPinia()
  setActivePinia(pinia)
  const store = useSessionStore()
  store.user = { id: 'usr_test', name: 'Test Operator', roles }
  store.status = 'authenticated'

  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/config', name: 'config-namespaces', component: { template: '<div />' } },
      {
        path: '/config/namespaces/:namespace',
        name: 'config-namespace-editor',
        component: { template: '<div />' },
      },
      {
        path: '/config/namespaces/:name/schema',
        name: 'config-namespace-schema',
        component: NamespaceSchemaView,
      },
    ],
  })
  await router.push('/config/namespaces/balance.weapons/schema')
  await router.isReady()

  const wrapper = mount(Host, {
    global: {
      plugins: [router, pinia],
      stubs: { ...IONIC_STUBS, RawJsonEditor: RawJsonEditorStub },
      renderStubDefaultSlot: true,
    },
  })
  await flushPromises()
  return { wrapper, router, view: wrapper.findComponent(NamespaceSchemaView) }
}

async function editAndSave(view: VueWrapper, json: string): Promise<void> {
  await view.get('[data-testid="schema-edit"]').trigger('click')
  await view.get('[data-testid="raw-editor"]').setValue(json)
  await view.get('[data-testid="schema-save"]').trigger('click')
  await flushPromises()
}

describe('NamespaceSchemaView edit gating', () => {
  it('shows an admin the Edit control', async () => {
    const { view } = await mountSchema(['admin'])
    expect(view.find('[data-testid="schema-edit"]').exists()).toBe(true)
    expect(view.get('[data-testid="raw-editor"]').element.hasAttribute('readonly')).toBe(true)
  })

  it('never shows Edit to a viewer', async () => {
    const { view } = await mountSchema(['viewer'])
    expect(view.find('[data-testid="schema-edit"]').exists()).toBe(false)
    expect(view.find('[data-testid="schema-save"]').exists()).toBe(false)
  })

  it('never shows Edit to live_ops, since replacing a schema is admin', async () => {
    const { view } = await mountSchema(['live_ops'])
    expect(view.find('[data-testid="schema-edit"]').exists()).toBe(false)
  })
})

describe('NamespaceSchemaView save paths', () => {
  it('shows a server 400 under the editor and keeps the text', async () => {
    const { view } = await mountSchema(['admin'])
    const bad = JSON.stringify({ type: 12 }, null, 2)

    await editAndSave(view, bad)

    expect(view.get('[data-testid="schema-server-error"]').text()).toContain(
      'not a valid JSON Schema',
    )
    // The editor still holds the document that was refused.
    expect((view.get('[data-testid="raw-editor"]').element as HTMLTextAreaElement).value).toBe(bad)
    expect(view.find('[data-testid="schema-toast"]').exists()).toBe(false)
  })

  it('shows a JSON parse error with its line and does not call the server', async () => {
    const { view } = await mountSchema(['admin'])
    let putCalled = false
    server.use(
      http.put('/api/admin/config/namespaces/balance.weapons/schema', () => {
        putCalled = true
        return HttpResponse.json({}, { status: 200 })
      }),
    )

    await editAndSave(view, '{"a": 1,,}')

    expect(view.get('[data-testid="schema-parse-error"]').text()).toContain('Line 1')
    expect(putCalled).toBe(false)
  })

  it('sends the loaded version and turns a stale refusal into Reload or Save anyway', async () => {
    const { view } = await mountSchema(['admin'])
    await view.get('[data-testid="schema-edit"]').trigger('click')
    await view.get('[data-testid="raw-editor"]').setValue(JSON.stringify({ type: 'object' }))

    let ifMatch: string | null = null
    server.use(
      http.get('/api/admin/config/namespaces/balance.weapons/schema', () =>
        HttpResponse.json({
          namespace: 'balance.weapons',
          schema_version: 4,
          schema: { type: 'object' },
          created_by: 'other@example.com',
          created_at: '2026-09-22T10:00:00Z',
        }),
      ),
      // Config refuses the write under its row lock: someone saved v4 after this
      // page loaded v3.
      http.put('/api/admin/config/namespaces/balance.weapons/schema', ({ request }) => {
        ifMatch = request.headers.get('If-Match')
        return HttpResponse.json(
          {
            error: {
              code: 'stale_schema',
              message: 'schema v4 was saved since v3',
              request_id: 'req_test',
            },
          },
          { status: 409 },
        )
      }),
    )

    await view.get('[data-testid="schema-save"]').trigger('click')
    await flushPromises()

    expect(ifMatch).toBe('"3"')
    expect(view.get('[data-testid="schema-conflict-message"]').text()).toContain(
      'Someone saved schema v4',
    )
    expect(view.get('[data-testid="schema-conflict-message"]').text()).toContain(
      'while you were editing',
    )
    expect(view.find('[data-testid="schema-conflict-reload"]').exists()).toBe(true)
    expect(view.find('[data-testid="schema-conflict-save-anyway"]').exists()).toBe(true)
  })

  it('saves as the next version and returns to read-only', async () => {
    // Last in the file: this one moves the fixture's schema version.
    const { view } = await mountSchema(['admin'])

    await editAndSave(view, JSON.stringify({ type: 'object' }, null, 2))

    expect(view.get('[data-testid="schema-toast"]').text()).toContain('Schema v4 saved')
    expect(view.get('[data-testid="raw-editor"]').element.hasAttribute('readonly')).toBe(true)
    expect(view.find('[data-testid="schema-edit"]').exists()).toBe(true)
  })
})

describe('NamespaceSchemaView unsaved-changes guard', () => {
  it('asks before leaving with edits and honours the answer', async () => {
    const { view, router } = await mountSchema(['admin'])
    await view.get('[data-testid="schema-edit"]').trigger('click')
    await view.get('[data-testid="raw-editor"]').setValue(JSON.stringify({ type: 'object' }))

    const confirm = vi.fn().mockReturnValue(false)
    vi.stubGlobal('confirm', confirm)
    await router.push('/config')
    expect(router.currentRoute.value.name).toBe('config-namespace-schema')

    confirm.mockReturnValue(true)
    await router.push('/config')
    expect(router.currentRoute.value.name).toBe('config-namespaces')
    vi.unstubAllGlobals()
  })
})
