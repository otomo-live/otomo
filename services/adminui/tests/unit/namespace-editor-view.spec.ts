import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter, type Router } from 'vue-router'
import { defineComponent, h } from 'vue'
import { describe, expect, it } from 'vitest'

import SchemaField from '@/config/SchemaField.vue'
import VersionsTab from '@/config/VersionsTab.vue'
import { useSessionStore } from '@/stores/session'
import NamespaceEditorView from '@/views/config/NamespaceEditorView.vue'

/**
 * The editor's role gating and its URL-backed version selection.
 *
 * The CodeMirror editor is a textarea stub with the same contract, and Ionic is
 * stubbed: the interesting facts are which controls exist for a role, whether a
 * field is disabled, and what a selection writes into the URL. The rows and the
 * diff come from the shared fixture API.
 */

const IONIC_STUBS = {
  IonPage: true,
  IonHeader: true,
  IonToolbar: true,
  IonMenuButton: true,
  IonBackButton: true,
  IonContent: true,
  IonSpinner: true,
  IonButton: true,
  IonButtons: true,
  IonSegment: true,
  IonSegmentButton: true,
  IonModal: true,
  IonItem: true,
  IonLabel: true,
  IonList: true,
  IonNote: true,
  IonInput: true,
  IonSelect: true,
  IonSelectOption: true,
  IonToggle: true,
  IonText: true,
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

interface Mounted {
  wrapper: VueWrapper
  router: Router
}

async function mountEditor(roles: string[], path: string): Promise<Mounted> {
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
        component: NamespaceEditorView,
      },
      {
        path: '/config/namespaces/:name/schema',
        name: 'config-namespace-schema',
        component: { template: '<div />' },
      },
    ],
  })
  await router.push(path)
  await router.isReady()

  const wrapper = mount(NamespaceEditorView, {
    global: {
      plugins: [router, pinia],
      stubs: { ...IONIC_STUBS, RawJsonEditor: RawJsonEditorStub },
      renderStubDefaultSlot: true,
    },
  })
  await flushPromises()
  return { wrapper, router }
}

describe('NamespaceEditorView role gating', () => {
  it('renders a viewer a read-only editor with no mutating controls', async () => {
    const { wrapper } = await mountEditor(['viewer'], '/config/namespaces/balance.weapons')

    expect(wrapper.find('[data-testid="save-draft"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="create-version"]').exists()).toBe(false)

    const fields = wrapper.findAllComponents(SchemaField)
    expect(fields.length).toBeGreaterThan(0)
    expect(fields.every((field) => field.props('disabled') === true)).toBe(true)
  })

  it('gives a viewer the JSON tab read-only', async () => {
    const { wrapper } = await mountEditor(['viewer'], '/config/namespaces/balance.weapons?tab=raw')

    const editor = wrapper.get('[data-testid="raw-editor"]')
    expect(editor.element.hasAttribute('readonly')).toBe(true)
  })

  it('lets live_ops edit and cut a version', async () => {
    const { wrapper } = await mountEditor(['live_ops'], '/config/namespaces/balance.weapons')

    expect(wrapper.find('[data-testid="save-draft"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="create-version"]').exists()).toBe(true)
    const fields = wrapper.findAllComponents(SchemaField)
    expect(fields.length).toBeGreaterThan(0)
    expect(fields.every((field) => field.props('disabled') === false)).toBe(true)
  })
})

describe('NamespaceEditorView versions tab', () => {
  it('reads a version diff deep link back out of the URL', async () => {
    const { wrapper, router } = await mountEditor(
      ['viewer'],
      '/config/namespaces/balance.weapons?tab=versions&from=10&to=draft',
    )

    expect(router.currentRoute.value.query.tab).toBe('versions')
    expect(router.currentRoute.value.query.from).toBe('10')
    expect(router.currentRoute.value.query.to).toBe('draft')

    const versions = wrapper.findComponent(VersionsTab)
    expect(versions.exists()).toBe(true)
    expect(versions.props('from')).toBe('10')
    expect(versions.props('to')).toBe('draft')
  })

  it('writes a row selection into the URL and replaces the older ref', async () => {
    const { wrapper, router } = await mountEditor(
      ['viewer'],
      '/config/namespaces/balance.weapons?tab=versions',
    )
    const versions = wrapper.findComponent(VersionsTab)

    versions.vm.$emit('select', '10')
    await flushPromises()
    expect(router.currentRoute.value.query.from).toBe('10')
    expect(router.currentRoute.value.query.to).toBeUndefined()

    versions.vm.$emit('select', '11')
    await flushPromises()
    expect(router.currentRoute.value.query.from).toBe('10')
    expect(router.currentRoute.value.query.to).toBe('11')
    expect(router.currentRoute.value.query.tab).toBe('versions')

    // A third pick keeps the two most recent, so `10` is dropped and the newest
    // click is always the `to` side.
    versions.vm.$emit('select', 'draft')
    await flushPromises()
    expect(router.currentRoute.value.query.from).toBe('11')
    expect(router.currentRoute.value.query.to).toBe('draft')
  })
})
