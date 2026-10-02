import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter } from 'vue-router'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import {
  changePassword,
  confirmMfa,
  disableMfa,
  enrollMfa,
  fetchAccountStatus,
  listSessions,
  regenerateRecoveryCodes,
  revokeOtherSessions,
  type AccountSession,
} from '@/api/account'
import { ApiError } from '@/api/errors'
import { describeUserAgent } from '@/account/userAgent'
import { useSessionStore } from '@/stores/session'
import AccountView from '@/views/account/AccountView.vue'

/**
 * The account page's three panels.
 *
 * The API module is stubbed so each panel's success and refusal path can be
 * driven directly; the real request path and the fixture's own state are the
 * `account-api` suite's job. Ionic is stubbed by hand so `v-model` fields and
 * disabled buttons behave like the real ones.
 */

vi.mock('qrcode', () => ({
  default: { toDataURL: vi.fn(async () => 'data:image/png;base64,AAAA') },
}))

vi.mock('@/api/account', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/api/account')>()
  return {
    ...actual,
    changePassword: vi.fn(),
    listSessions: vi.fn(),
    revokeOtherSessions: vi.fn(),
    regenerateRecoveryCodes: vi.fn(),
    disableMfa: vi.fn(),
    enrollMfa: vi.fn(),
    confirmMfa: vi.fn(),
    fetchAccountStatus: vi.fn(),
  }
})

const mockChangePassword = vi.mocked(changePassword)
const mockListSessions = vi.mocked(listSessions)
const mockRevokeOthers = vi.mocked(revokeOtherSessions)
const mockRegenerate = vi.mocked(regenerateRecoveryCodes)
const mockDisable = vi.mocked(disableMfa)
const mockEnroll = vi.mocked(enrollMfa)
const mockConfirm = vi.mocked(confirmMfa)
const mockFetchStatus = vi.mocked(fetchAccountStatus)

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

const CHROME_MAC =
  'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36'

const CURRENT: AccountSession = {
  id: 'sess_current',
  createdAt: '2026-09-27T09:00:00.000Z',
  lastUsedAt: '2026-09-27T09:05:00.000Z',
  ip: '203.0.113.7',
  userAgent: CHROME_MAC,
  current: true,
}

const OTHER: AccountSession = {
  id: 'sess_other',
  createdAt: '2026-09-26T09:00:00.000Z',
  lastUsedAt: '2026-09-26T12:00:00.000Z',
  ip: '198.51.100.24',
  userAgent: 'Mozilla/5.0 (X11; Linux x86_64; rv:125.0) Gecko/20100101 Firefox/125.0',
  current: false,
}

const IONIC_STUBS = {
  IonPage: { template: '<div><slot /></div>' },
  IonHeader: { template: '<div><slot /></div>' },
  IonToolbar: { template: '<div><slot /></div>' },
  IonMenuButton: true,
  IonContent: { template: '<div><slot /></div>' },
  IonSpinner: true,
  IonButton: {
    props: ['disabled'],
    template: '<button :disabled="disabled"><slot /></button>',
  },
  IonInput: {
    props: ['modelValue', 'label', 'type'],
    emits: ['update:modelValue'],
    template:
      '<input :aria-label="label" :type="type ?? \'text\'" :value="modelValue" @input="$emit(\'update:modelValue\', $event.target.value)" />',
  },
  IonItem: { template: '<div><slot /></div>' },
  IonList: { template: '<div><slot /></div>' },
  IonNote: { template: '<div><slot /></div>' },
  IonCheckbox: {
    props: ['modelValue'],
    emits: ['update:modelValue'],
    template:
      '<input type="checkbox" :checked="modelValue" @change="$emit(\'update:modelValue\', $event.target.checked)" />',
  },
  IonModal: {
    props: ['isOpen'],
    template: '<div v-if="isOpen"><slot /></div>',
  },
}

function apiError(code: string, status: number): ApiError {
  return new ApiError({ status, code, message: `mock ${code}`, requestId: 'req_test' })
}

interface MountOptions {
  roles?: string[]
  isRoot?: boolean
  mfaEnabled?: boolean
  statusError?: boolean
  sessions?: AccountSession[]
}

async function mountPage(options: MountOptions = {}): Promise<{
  wrapper: VueWrapper
  store: ReturnType<typeof useSessionStore>
}> {
  const pinia = createPinia()
  setActivePinia(pinia)
  const store = useSessionStore()
  store.user = { id: 'usr_test', name: 'Test Operator', roles: options.roles ?? ['live_ops'] }
  store.email = 'liveops@example.com'
  store.status = 'authenticated'

  const router = createRouter({
    history: createMemoryHistory(),
    routes: [{ path: '/account', name: 'account', component: AccountView }],
  })
  await router.push('/account')
  await router.isReady()

  mockListSessions.mockResolvedValue(options.sessions ?? [CURRENT, OTHER])
  if (options.statusError === true) {
    mockFetchStatus.mockRejectedValue(apiError('internal', 500))
  } else {
    mockFetchStatus.mockResolvedValue({
      isRoot: options.isRoot ?? false,
      mfaEnabled: options.mfaEnabled ?? true,
    })
  }

  const wrapper = mount(AccountView, {
    global: { plugins: [router, pinia], stubs: IONIC_STUBS, renderStubDefaultSlot: true },
  })
  await flushPromises()
  await flushPromises()
  return { wrapper, store }
}

async function fillPasswordForm(wrapper: VueWrapper): Promise<void> {
  await wrapper.get('input[aria-label="Current password"]').setValue('old password')
  await wrapper.get('input[aria-label="New password"]').setValue('correct horse battery staple')
  await wrapper.get('input[aria-label="Confirm password"]').setValue('correct horse battery staple')
}

beforeEach(() => {
  mockChangePassword.mockReset().mockResolvedValue(undefined)
  mockListSessions.mockReset().mockResolvedValue([CURRENT, OTHER])
  mockRevokeOthers.mockReset().mockResolvedValue({ revoked: 1, currentKept: true })
  mockRegenerate.mockReset().mockResolvedValue(CODES)
  mockDisable.mockReset().mockResolvedValue(undefined)
  mockEnroll.mockReset().mockResolvedValue({
    secret: 'JBSWY3DPEHPK3PXP',
    otpauthUrl: 'otpauth://totp/Otomo%20Admin:liveops?secret=JBSWY3DPEHPK3PXP',
  })
  mockConfirm.mockReset().mockResolvedValue(CODES)
  mockFetchStatus.mockReset().mockResolvedValue({ isRoot: false, mfaEnabled: true })
})

describe('AccountView password panel', () => {
  it('changes the password, clears the fields and says other sessions went', async () => {
    const { wrapper } = await mountPage()
    await fillPasswordForm(wrapper)
    await wrapper.get('[data-testid="password-form"]').trigger('submit')
    await flushPromises()

    expect(mockChangePassword).toHaveBeenCalledWith('old password', 'correct horse battery staple')
    expect(wrapper.get('[data-testid="account-toast"]').text()).toContain(
      'other sessions were signed out',
    )
    expect(
      (wrapper.get('input[aria-label="Current password"]').element as HTMLInputElement).value,
    ).toBe('')
    // The list is re-read because the server signed the others out.
    expect(mockListSessions).toHaveBeenCalledTimes(2)
    wrapper.unmount()
  })

  it('shows the current-password refusal inline', async () => {
    mockChangePassword.mockRejectedValue(apiError('invalid_credentials', 401))
    const { wrapper } = await mountPage()
    await fillPasswordForm(wrapper)
    await wrapper.get('[data-testid="password-form"]').trigger('submit')
    await flushPromises()

    expect(wrapper.text()).toContain('Your current password is not correct.')
    wrapper.unmount()
  })
})

describe('AccountView two-factor panel', () => {
  it('hides Turn off for an admin and says the factor is required', async () => {
    const { wrapper } = await mountPage({ roles: ['admin'] })

    expect(wrapper.get('[data-testid="mfa-status"]').text()).toContain('is enabled')
    expect(wrapper.get('[data-testid="mfa-admin-note"]').text()).toContain('required for admin')
    expect(wrapper.find('[data-testid="mfa-disable"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('offers Turn off to a non-admin whose factor is enabled', async () => {
    const { wrapper } = await mountPage({ roles: ['live_ops'] })
    expect(wrapper.find('[data-testid="mfa-disable"]').exists()).toBe(true)
    wrapper.unmount()
  })

  it('does not call the enroll endpoint just by loading the page', async () => {
    const { wrapper } = await mountPage({ roles: ['live_ops'], mfaEnabled: false })

    expect(mockEnroll).not.toHaveBeenCalled()
    expect(wrapper.get('[data-testid="mfa-status"]').text()).toContain('is not enabled')
    wrapper.unmount()
  })

  it('shows the root password-only note from is_root, without enrolling', async () => {
    const { wrapper } = await mountPage({ roles: ['admin'], isRoot: true })

    expect(wrapper.get('[data-testid="mfa-root-note"]').text()).toContain('password-only')
    expect(wrapper.find('[data-testid="mfa-setup"]').exists()).toBe(false)
    expect(mockEnroll).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('shows the error state with a retry when the status read fails', async () => {
    const { wrapper } = await mountPage({ statusError: true })

    expect(wrapper.text()).toContain('That did not work')
    expect(wrapper.find('[data-testid="mfa-status"]').exists()).toBe(false)

    mockFetchStatus.mockResolvedValue({ isRoot: false, mfaEnabled: true })
    await wrapper.get('.error-panel__retry').trigger('click')
    await flushPromises()

    expect(wrapper.get('[data-testid="mfa-status"]').text()).toContain('is enabled')
    wrapper.unmount()
  })

  it('runs the setup flow and shows the recovery codes', async () => {
    const { wrapper } = await mountPage({ roles: ['live_ops'], mfaEnabled: false })

    await wrapper.get('[data-testid="mfa-setup"]').trigger('click')
    await flushPromises()

    expect(mockEnroll).toHaveBeenCalledTimes(1)
    expect(wrapper.find('[data-testid="account-qr"] img').exists()).toBe(true)
    expect(wrapper.get('[data-testid="account-secret"]').text()).toContain('JBSW Y3DP EHPK 3PXP')

    await wrapper.get('input[aria-label="Code"]').setValue('123456')
    await wrapper.get('[data-testid="mfa-setup-form"]').trigger('submit')
    await flushPromises()

    expect(mockConfirm).toHaveBeenCalledWith('123456')
    expect(wrapper.findAll('[data-testid="recovery-codes"] code')).toHaveLength(10)
    wrapper.unmount()
  })

  it('requires a code before regenerating recovery codes', async () => {
    const { wrapper } = await mountPage({ roles: ['live_ops'] })

    await wrapper.get('[data-testid="mfa-regenerate"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[data-testid="mfa-code-submit"]').attributes('disabled')).toBeDefined()

    await wrapper.get('input[aria-label="Code"]').setValue('123456')
    await wrapper.get('[data-testid="mfa-code-form"]').trigger('submit')
    await flushPromises()

    expect(mockRegenerate).toHaveBeenCalledWith('123456')
    expect(wrapper.findAll('[data-testid="recovery-codes"] code')).toHaveLength(10)
    wrapper.unmount()
  })

  it('shows an invalid-code refusal when regeneration is refused', async () => {
    mockRegenerate.mockRejectedValue(apiError('invalid_code', 401))
    const { wrapper } = await mountPage({ roles: ['live_ops'] })

    await wrapper.get('[data-testid="mfa-regenerate"]').trigger('click')
    await wrapper.get('input[aria-label="Code"]').setValue('000000')
    await wrapper.get('[data-testid="mfa-code-form"]').trigger('submit')
    await flushPromises()

    expect(wrapper.text()).toContain('That code is not valid')
    expect(wrapper.find('[data-testid="recovery-codes"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('warns what turning the factor off removes, then turns it off', async () => {
    const { wrapper } = await mountPage({ roles: ['live_ops'] })

    await wrapper.get('[data-testid="mfa-disable"]').trigger('click')
    expect(wrapper.get('[data-testid="mfa-disable-warning"]').text()).toContain('removes')

    await wrapper.get('input[aria-label="Code"]').setValue('123456')
    await wrapper.get('[data-testid="mfa-code-form"]').trigger('submit')
    await flushPromises()

    expect(mockDisable).toHaveBeenCalledWith('123456')
    expect(wrapper.get('[data-testid="mfa-status"]').text()).toContain('is not enabled')
    wrapper.unmount()
  })
})

describe('AccountView sessions panel', () => {
  it('marks exactly the current session as this device', async () => {
    const { wrapper } = await mountPage()

    const badges = wrapper.findAll('[data-testid="session-current"]')
    expect(badges).toHaveLength(1)
    expect(badges[0].text()).toBe('This device')
    expect(wrapper.text()).toContain('Chrome on macOS')
    expect(wrapper.text()).toContain('198.51.100.24')
    wrapper.unmount()
  })

  it('confirms before signing out other sessions and reports the count', async () => {
    const { wrapper } = await mountPage()

    await wrapper.get('[data-testid="revoke-others"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[data-testid="confirm-dialog-message"]').text()).toContain(
      'except this one',
    )

    mockListSessions.mockResolvedValue([CURRENT])
    await wrapper.get('[data-testid="confirm-dialog-confirm"]').trigger('click')
    await flushPromises()

    expect(mockRevokeOthers).toHaveBeenCalledTimes(1)
    expect(wrapper.get('[data-testid="account-toast"]').text()).toContain('1 other session')
    expect(wrapper.findAll('.data-table__row')).toHaveLength(1)
    wrapper.unmount()
  })
})

describe('describeUserAgent', () => {
  it('names the common browser and platform pairs briefly', () => {
    expect(describeUserAgent(CHROME_MAC)).toBe('Chrome on macOS')
    expect(
      describeUserAgent('Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:125.0) Firefox/125.0'),
    ).toBe('Firefox on Windows')
    expect(
      describeUserAgent('Mozilla/5.0 (iPhone; CPU iPhone OS 17_4 like Mac OS X) Safari/604.1'),
    ).toBe('Safari on iOS')
    expect(describeUserAgent('')).toBe('Unknown device')
  })
})
