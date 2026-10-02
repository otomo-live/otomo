import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { http, HttpResponse } from 'msw'
import { describe, expect, it } from 'vitest'

import CreateNamespaceDialog from '@/config/CreateNamespaceDialog.vue'
import { server } from '@/mocks/server'

/**
 * The create dialog's own behaviour: what it refuses before the request and how
 * it shows a refusal that came back. The POST itself is the fixture API, so the
 * duplicate and the bad-name paths are the service's, not a stub's.
 */

const IONIC_STUBS = { IonModal: true, IonButton: true, IonSpinner: true }

function mountDialog(): VueWrapper {
  return mount(CreateNamespaceDialog, {
    props: { isOpen: true },
    global: {
      stubs: IONIC_STUBS,
      renderStubDefaultSlot: true,
    },
  })
}

async function fill(wrapper: VueWrapper, name: string): Promise<void> {
  await wrapper.get('[data-testid="create-namespace-name"]').setValue(name)
}

describe('CreateNamespaceDialog', () => {
  it('refuses a name Config would reject before sending anything', async () => {
    const wrapper = mountDialog()
    let posted = false
    server.use(
      http.post('/api/admin/config/namespaces', () => {
        posted = true
        return HttpResponse.json({}, { status: 201 })
      }),
    )

    await fill(wrapper, 'Not A Slug')
    await wrapper.get('form').trigger('submit')
    await flushPromises()

    expect(wrapper.get('[data-testid="create-namespace-name-error"]').text()).toContain('lowercase')
    expect(posted).toBe(false)
    expect(wrapper.emitted('created')).toBeUndefined()
    wrapper.unmount()
  })

  it('shows the server 409 on the form instead of closing', async () => {
    const wrapper = mountDialog()

    await fill(wrapper, 'balance.weapons')
    await wrapper.get('form').trigger('submit')
    await flushPromises()

    const error = wrapper.get('[data-testid="create-namespace-error"]')
    expect(error.text()).toContain('already exists')
    expect(wrapper.emitted('created')).toBeUndefined()
    wrapper.unmount()
  })

  it('shows the server 400 message verbatim', async () => {
    server.use(
      http.post('/api/admin/config/namespaces', () =>
        HttpResponse.json(
          {
            error: {
              code: 'validation_failed',
              message: 'name must match ^[a-z][a-z0-9_]*(\\.[a-z][a-z0-9_]*)*$',
              request_id: 'req_test',
            },
          },
          { status: 400 },
        ),
      ),
    )
    const wrapper = mountDialog()

    await fill(wrapper, 'unit.server_reject')
    await wrapper.get('form').trigger('submit')
    await flushPromises()

    expect(wrapper.get('[data-testid="create-namespace-error"]').text()).toContain(
      'name must match',
    )
    wrapper.unmount()
  })

  it('emits the created namespace and closes through the caller', async () => {
    const wrapper = mountDialog()

    await fill(wrapper, 'unit.created')
    await wrapper.get('form').trigger('submit')
    await flushPromises()

    const created = wrapper.emitted('created')
    expect(created).toHaveLength(1)
    expect((created![0][0] as { name: string }).name).toBe('unit.created')
    wrapper.unmount()
  })
})
