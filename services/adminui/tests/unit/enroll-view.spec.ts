import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter } from 'vue-router'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import { useSessionStore } from '@/stores/session'
import EnrollView from '@/views/auth/EnrollView.vue'

/**
 * The enrollment screen's two load-bearing behaviours: it renders the pending
 * secret as a QR and grouped text, and the recovery-codes step cannot be left
 * until the checkbox says the codes were saved.
 *
 * `qrcode` is mocked because the happy-dom test environment has no canvas, and
 * the session store's two network calls are stubbed so the view can be walked
 * without the fixture API. The API shapes themselves are the mock suite's job.
 */

vi.mock('qrcode', () => ({
  default: { toDataURL: vi.fn(async () => 'data:image/png;base64,AAAA') },
}))

const CODES = [
  'K7F2-9QDA',
  'M3P8-1LZX',
  'T6R4-0WVB',
  'H9N2-5ECY',
  'B1D7-8KJU',
  'Q4S6-3MPA',
  'X8C1-6RTN',
  'L2V9-7FHE',
  'G5Y3-0ZOW',
  'J0A4-2SDI',
]

/**
 * Wrappers that support `v-model` where the view uses it, rather than blank
 * stubs: the code field and the saved checkbox are the interaction under test.
 */
const IONIC_STUBS = {
  IonPage: { template: '<div><slot /></div>' },
  IonContent: { template: '<div><slot /></div>' },
  IonCard: { template: '<div><slot /></div>' },
  IonCardContent: { template: '<div><slot /></div>' },
  IonItem: { template: '<div><slot /></div>' },
  IonList: { template: '<div><slot /></div>' },
  IonNote: { template: '<div><slot /></div>' },
  IonSpinner: true,
  IonButton: {
    props: ['disabled'],
    template: '<button :disabled="disabled"><slot /></button>',
  },
  IonInput: {
    props: ['modelValue', 'label'],
    emits: ['update:modelValue'],
    template:
      '<input :aria-label="label" :value="modelValue" @input="$emit(\'update:modelValue\', $event.target.value)" />',
  },
  IonCheckbox: {
    props: ['modelValue'],
    emits: ['update:modelValue'],
    template:
      '<input type="checkbox" :checked="modelValue" @change="$emit(\'update:modelValue\', $event.target.checked)" />',
  },
}

async function mountEnroll(): Promise<{
  wrapper: VueWrapper
  store: ReturnType<typeof useSessionStore>
  router: ReturnType<typeof createRouter>
}> {
  const pinia = createPinia()
  setActivePinia(pinia)
  const store = useSessionStore()
  store.mfaTicket = 'mfa_enroll_ticket_new.admin@example.com'
  store.mfaPurpose = 'enroll'
  store.mfaTicketExpiresAt = Date.now() + 5 * 60 * 1000

  const router = createRouter({
    history: createMemoryHistory(),
    routes: [{ path: '/login/enroll', name: 'mfa-enroll', component: EnrollView }],
  })
  await router.push('/login/enroll?returnTo=/dashboard')
  await router.isReady()

  vi.spyOn(store, 'beginEnrollment').mockResolvedValue({
    secret: 'JBSWY3DPEHPK3PXP',
    otpauthUrl: 'otpauth://totp/Otomo%20Admin:new.admin@example.com?secret=JBSWY3DPEHPK3PXP',
  })
  vi.spyOn(store, 'confirmEnrollment').mockResolvedValue(CODES)

  const wrapper = mount(EnrollView, {
    global: { plugins: [pinia, router], stubs: IONIC_STUBS },
  })
  await flushPromises()
  return { wrapper, store, router }
}

function continueButton(wrapper: VueWrapper) {
  return wrapper.get('[data-testid="recovery-continue"]')
}

beforeEach(() => {
  vi.restoreAllMocks()
})

describe('EnrollView', () => {
  it('renders the QR, the grouped secret and the code field', async () => {
    const { wrapper } = await mountEnroll()

    expect(wrapper.find('[data-testid="enroll-qr"] img').exists()).toBe(true)
    expect(wrapper.get('[data-testid="enroll-secret"]').text()).toContain('JBSW Y3DP EHPK 3PXP')
    expect(wrapper.find('input[aria-label="Code"]').exists()).toBe(true)

    wrapper.unmount()
  })

  it('shows the ten recovery codes and gates Continue on the checkbox', async () => {
    const { wrapper, router } = await mountEnroll()

    await wrapper.get('input[aria-label="Code"]').setValue('123456')
    await wrapper.get('form').trigger('submit')
    await flushPromises()

    const listed = wrapper.findAll('[data-testid="recovery-codes"] code')
    expect(listed).toHaveLength(CODES.length)
    expect(listed.map((entry) => entry.text())).toEqual(CODES)

    // Not yet acknowledged: Continue is disabled.
    expect(continueButton(wrapper).attributes('disabled')).toBeDefined()

    // Ticking the box enables it, and only then does Continue navigate.
    await wrapper.get('[data-testid="recovery-saved"]').setValue(true)
    await flushPromises()
    expect(continueButton(wrapper).attributes('disabled')).toBeUndefined()

    await continueButton(wrapper).trigger('click')
    await flushPromises()
    expect(router.currentRoute.value.fullPath).toBe('/dashboard')

    wrapper.unmount()
  })
})
