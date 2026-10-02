import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { HttpResponse, http } from 'msw'
import { describe, expect, it } from 'vitest'

import { server } from '@/mocks/server'
import ConflictDialog from '@/config/ConflictDialog.vue'
import CreateVersionDialog from '@/config/CreateVersionDialog.vue'

/**
 * The create-version dialog against the shared fixture API.
 *
 * The fixture's `balance.weapons` draft is byte-for-byte version 11. With that
 * preview the dialog refuses up front ("nothing to version"); the server-side
 * `no_changes` and `stale_revision` refusals are races the preview cannot see, so
 * those tests show a changed preview (`previewWithAChange`) and let the server
 * refuse. `ui.presentation`'s draft has diverged from version 6, so it is the
 * success case. The 201 test runs last because it moves that fixture's history.
 */

// The preview says the draft differs from the latest version, as it would have
// when the dialog opened, before someone else's save made the two equal.
function previewWithAChange(): void {
  server.use(
    http.get('*/api/admin/config/namespaces/balance.weapons/diff', () =>
      HttpResponse.json({
        from: { ref: '11', document: { range: 21 } },
        to: { ref: 'draft', document: { range: 18.5 } },
        changes: [{ op: 'replace', path: '/range', from: 21, to: 18.5 }],
      }),
    ),
  )
}

const IONIC_STUBS = {
  IonModal: true,
  IonHeader: true,
  IonToolbar: true,
  IonTitle: true,
  IonButtons: true,
  IonContent: true,
  IonSpinner: true,
  IonButton: true,
  IonItem: true,
  IonLabel: true,
  IonList: true,
  IonNote: true,
}

async function mountDialog(namespace = 'balance.weapons', revision = 12): Promise<VueWrapper> {
  const wrapper = mount(CreateVersionDialog, {
    props: { isOpen: true, namespace, revision, localChanges: [] },
    global: { stubs: IONIC_STUBS, renderStubDefaultSlot: true },
  })
  await flushPromises()
  return wrapper
}

async function typeAndSubmit(wrapper: VueWrapper, message: string): Promise<void> {
  await wrapper.get('[data-testid="create-version-message"]').setValue(message)
  await wrapper.get('[data-testid="create-version-submit"]').trigger('click')
  await flushPromises()
}

describe('CreateVersionDialog nothing to version', () => {
  it('disables submit and says why when the draft equals the latest version', async () => {
    const wrapper = await mountDialog('balance.weapons', 12)
    await wrapper.get('[data-testid="create-version-message"]').setValue('A message')
    await flushPromises()

    expect(wrapper.get('[data-testid="create-version-nothing"]').text()).toContain(
      'Nothing to version',
    )
    expect(
      wrapper.get('[data-testid="create-version-submit"]').attributes('disabled'),
    ).toBeDefined()
  })
})

describe('CreateVersionDialog message rule', () => {
  it('blocks an empty or whitespace-only message and says why', async () => {
    previewWithAChange()
    const wrapper = await mountDialog()

    const submit = wrapper.get('[data-testid="create-version-submit"]')
    expect(submit.attributes('disabled')).toBeDefined()
    expect(wrapper.get('[data-testid="create-version-message-hint"]').text()).toContain('required')

    await wrapper.get('[data-testid="create-version-message"]').setValue('   ')
    expect(submit.attributes('disabled')).toBeDefined()

    await wrapper.get('[data-testid="create-version-message"]').setValue('Nerf the Arc Rifle')
    expect(submit.attributes('disabled')).toBeUndefined()
  })
})

describe('CreateVersionDialog submit paths', () => {
  it('shows the no_changes refusal inline rather than as a conflict', async () => {
    previewWithAChange()
    const wrapper = await mountDialog('balance.weapons', 12)

    await typeAndSubmit(wrapper, 'A message')

    const error = wrapper.get('[data-testid="create-version-error"]').text()
    expect(error).toContain('Nothing to version')
    expect(error).toContain('version 11')
    // no_changes is not the reload-or-overwrite question.
    expect(wrapper.findComponent(ConflictDialog).exists()).toBe(false)
  })

  it('opens the conflict dialog when the draft moved under the reviewed revision', async () => {
    previewWithAChange()
    server.use(
      http.post('/api/admin/config/namespaces/balance.weapons/versions', () =>
        HttpResponse.json(
          {
            error: {
              code: 'stale_revision',
              message: 'the draft has moved on since revision 12',
              request_id: 'req_test',
            },
          },
          { status: 409 },
        ),
      ),
    )
    const wrapper = await mountDialog('balance.weapons', 12)

    await typeAndSubmit(wrapper, 'A message')

    const conflict = wrapper.findComponent(ConflictDialog)
    expect(conflict.exists()).toBe(true)
    expect(conflict.props('serverRevision')).toBe(12)
    expect(wrapper.text()).toContain('Someone else changed the draft')
    expect(wrapper.text()).toContain('Version the newer draft')
  })

  it('creates a version and reports the number on the 201 path', async () => {
    // Last: this one moves the fixture's version history.
    const wrapper = await mountDialog('ui.presentation', 7)

    await typeAndSubmit(wrapper, 'Describe the weapons list again')

    expect(wrapper.emitted('created')?.[0]).toEqual([7])
    expect(wrapper.find('[data-testid="create-version-error"]').exists()).toBe(false)
  })
})
