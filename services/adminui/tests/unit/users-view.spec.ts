import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter } from 'vue-router'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import { ApiError } from '@/api/errors'
import {
  inviteUser,
  listInvites,
  listUsers,
  resetUserMfa,
  resetUserPassword,
  revokeInvite,
  updateUser,
  type AdminUser,
  type Invite,
} from '@/api/users'
import { useSessionStore } from '@/stores/session'
import UsersView from '@/views/admin/UsersView.vue'

/**
 * The users page's D3 behaviour: which row actions exist for whom, that only
 * root may offer `admin`, that a one-time link is shown and then discarded, and
 * that a mutation's COM-5 refusal reaches the page as a sentence.
 *
 * The API module is stubbed so the page's rendering can be driven directly; the
 * real request path and the fixture's own refusals are the `users-api` suite's
 * job.
 */

vi.mock('@/api/users', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/api/users')>()
  return {
    ...actual,
    listUsers: vi.fn(),
    listInvites: vi.fn(),
    inviteUser: vi.fn(),
    revokeInvite: vi.fn(),
    updateUser: vi.fn(),
    resetUserPassword: vi.fn(),
    resetUserMfa: vi.fn(),
  }
})

const mockListUsers = vi.mocked(listUsers)
const mockListInvites = vi.mocked(listInvites)
const mockInviteUser = vi.mocked(inviteUser)
const mockRevokeInvite = vi.mocked(revokeInvite)
const mockUpdateUser = vi.mocked(updateUser)
const mockResetPassword = vi.mocked(resetUserPassword)
const mockResetMfa = vi.mocked(resetUserMfa)

const ROOT: AdminUser = {
  id: 'usr_root',
  email: 'root@example.com',
  name: 'Root',
  roles: ['admin'],
  isRoot: true,
  status: 'active',
  mfaEnrolled: true,
  lastLoginAt: null,
  createdAt: '2026-01-04T09:00:00Z',
}

const ADMIN: AdminUser = {
  id: 'usr_admin',
  email: 'admin@example.com',
  name: 'Admin',
  roles: ['admin'],
  isRoot: false,
  status: 'active',
  mfaEnrolled: true,
  lastLoginAt: '2026-09-23T17:40:00Z',
  createdAt: '2026-01-06T10:30:00Z',
}

const OPS: AdminUser = {
  id: 'usr_liveops',
  email: 'liveops@example.com',
  name: 'Live Ops',
  roles: ['live_ops'],
  isRoot: false,
  status: 'active',
  mfaEnrolled: true,
  lastLoginAt: null,
  createdAt: '2026-02-11T09:45:00Z',
}

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

async function mountPage(
  callerId: string,
  users: AdminUser[],
  invites: Invite[] = [],
): Promise<VueWrapper> {
  const pinia = createPinia()
  setActivePinia(pinia)
  const store = useSessionStore()
  store.user = { id: callerId, name: 'Caller', roles: ['admin'] }
  store.status = 'authenticated'

  const router = createRouter({
    history: createMemoryHistory(),
    routes: [{ path: '/users', name: 'admin-users', component: UsersView }],
  })
  await router.push('/users')
  await router.isReady()

  mockListUsers.mockResolvedValue(users)
  mockListInvites.mockResolvedValue(invites)

  const wrapper = mount(UsersView, {
    global: { plugins: [router, pinia], stubs: IONIC_STUBS, renderStubDefaultSlot: true },
  })
  await flushPromises()
  await flushPromises()
  return wrapper
}

async function openRowMenu(wrapper: VueWrapper, rowIndex: number): Promise<void> {
  const row = wrapper.findAll('.data-table__row')[rowIndex]
  await row.get('[data-testid="user-actions-toggle"]').trigger('click')
  await flushPromises()
}

beforeEach(() => {
  mockListUsers.mockReset()
  mockListInvites.mockReset()
  mockInviteUser.mockReset()
  mockRevokeInvite.mockReset()
  mockUpdateUser.mockReset()
  mockResetPassword.mockReset()
  mockResetMfa.mockReset()
  mockListUsers.mockResolvedValue([])
  mockListInvites.mockResolvedValue([])
})

describe('UsersView action visibility', () => {
  it('offers a non-root admin no actions on self, root or another admin', async () => {
    const wrapper = await mountPage('usr_admin', [ROOT, ADMIN, OPS])

    await openRowMenu(wrapper, 0) // root
    expect(wrapper.find('[data-testid="user-actions-none"]').exists()).toBe(true)

    await openRowMenu(wrapper, 1) // the caller's own admin row
    expect(wrapper.find('[data-testid="user-actions-none"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="user-action-change-role"]').exists()).toBe(false)

    await openRowMenu(wrapper, 2) // live_ops
    expect(wrapper.find('[data-testid="user-action-change-role"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="user-action-toggle-status"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="user-action-reset-link"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="user-action-reset-mfa"]').exists()).toBe(true)
    wrapper.unmount()
  })

  it('offers root the admin role when changing an admin', async () => {
    const wrapper = await mountPage('usr_root', [ROOT, ADMIN, OPS])

    await openRowMenu(wrapper, 1)
    await wrapper.get('[data-testid="user-action-change-role"]').trigger('click')
    await flushPromises()

    expect(wrapper.find('[data-testid="change-role-admin"]').exists()).toBe(true)
    mockUpdateUser.mockResolvedValue(ADMIN)
    await wrapper.get('[data-testid="change-role-submit"]').trigger('click')
    await flushPromises()

    expect(mockUpdateUser).toHaveBeenCalledWith('usr_admin', { roles: ['admin'] })
    wrapper.unmount()
  })
})

describe('UsersView invite dialog', () => {
  it('offers admin only to a root caller', async () => {
    const asAdmin = await mountPage('usr_admin', [ROOT, ADMIN])
    const adminOptions = asAdmin
      .findAll('#invite-role option')
      .map((option) => option.attributes('value'))
    expect(adminOptions).toEqual(['viewer', 'live_ops'])
    asAdmin.unmount()

    const asRoot = await mountPage('usr_root', [ROOT, ADMIN])
    const rootOptions = asRoot
      .findAll('#invite-role option')
      .map((option) => option.attributes('value'))
    expect(rootOptions).toEqual(['viewer', 'live_ops', 'admin'])
    asRoot.unmount()
  })

  it('shows the one-time link, then discards it on close', async () => {
    mockInviteUser.mockResolvedValue({
      id: 'invite_1',
      url: 'http://localhost:8090/admin/onboard#token=secret-token',
      expiresAt: new Date(Date.now() + 72 * 60 * 60 * 1000).toISOString(),
    })
    const wrapper = await mountPage('usr_admin', [ROOT, ADMIN])

    await wrapper.get('[data-testid="invite-user-open"]').trigger('click')
    await wrapper.get('[data-testid="invite-email"]').setValue('new@example.com')
    await wrapper.get('[data-testid="invite-name"]').setValue('New Person')
    await wrapper.get('[data-testid="invite-submit"]').trigger('click')
    await flushPromises()

    expect(wrapper.get('[data-testid="one-time-link"]').text()).toContain('token=secret-token')
    expect(wrapper.get('[data-testid="one-time-link-warning"]').text()).toContain('shown once')
    expect(wrapper.get('[data-testid="one-time-link-expiry"]').text()).toContain('72 h')

    await wrapper.get('[data-testid="one-time-link-close"]').trigger('click')
    await flushPromises()
    expect(wrapper.find('[data-testid="one-time-link"]').exists()).toBe(false)
    wrapper.unmount()
  })
})

describe('UsersView destructive confirmations', () => {
  it('confirms a revoke and refreshes the invites', async () => {
    const invite: Invite = {
      id: 'invite_1',
      purpose: 'invite',
      email: 'new@example.com',
      name: 'New Person',
      role: 'live_ops',
      createdBy: 'usr_admin',
      createdAt: '2026-09-25T10:00:00Z',
      expiresAt: '2026-09-28T10:00:00Z',
    }
    const wrapper = await mountPage('usr_admin', [ADMIN], [invite])

    await wrapper.get('[data-testid="revoke-invite"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[data-testid="confirm-dialog-message"]').text()).toContain(
      'new@example.com',
    )

    mockRevokeInvite.mockResolvedValue(undefined)
    await wrapper.get('[data-testid="confirm-dialog-confirm"]').trigger('click')
    await flushPromises()

    expect(mockRevokeInvite).toHaveBeenCalledWith('invite_1')
    wrapper.unmount()
  })

  it('explains what an MFA reset clears before doing it', async () => {
    const wrapper = await mountPage('usr_root', [ROOT, OPS])

    await openRowMenu(wrapper, 1)
    await wrapper.get('[data-testid="user-action-reset-mfa"]').trigger('click')
    await flushPromises()

    const message = wrapper.get('[data-testid="confirm-dialog-message"]').text()
    expect(message).toContain('recovery codes')
    expect(message).toContain('signed out')

    mockResetMfa.mockResolvedValue({ ...OPS, mfaEnrolled: false })
    await wrapper.get('[data-testid="confirm-dialog-confirm"]').trigger('click')
    await flushPromises()
    expect(mockResetMfa).toHaveBeenCalledWith('usr_liveops')
    wrapper.unmount()
  })

  it('sends the disable request the toggle names', async () => {
    const wrapper = await mountPage('usr_root', [ROOT, OPS])

    await openRowMenu(wrapper, 1)
    await wrapper.get('[data-testid="user-action-toggle-status"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[data-testid="confirm-dialog-message"]').text()).toContain('Disable')

    mockUpdateUser.mockResolvedValue({ ...OPS, status: 'disabled' })
    await wrapper.get('[data-testid="confirm-dialog-confirm"]').trigger('click')
    await flushPromises()
    expect(mockUpdateUser).toHaveBeenCalledWith('usr_liveops', { status: 'disabled' })
    wrapper.unmount()
  })

  it('shows a mapped sentence when the server refuses a reset', async () => {
    const wrapper = await mountPage('usr_root', [ROOT, OPS])

    await openRowMenu(wrapper, 1)
    await wrapper.get('[data-testid="user-action-reset-mfa"]').trigger('click')
    await flushPromises()
    mockResetMfa.mockRejectedValue(
      new ApiError({
        status: 409,
        code: 'mfa_not_enrolled',
        message: 'the account has no second factor to reset',
        requestId: 'req_9',
      }),
    )
    await wrapper.get('[data-testid="confirm-dialog-confirm"]').trigger('click')
    await flushPromises()

    expect(wrapper.get('[data-testid="users-error"]').text()).toContain('no second factor to reset')
    wrapper.unmount()
  })
})

describe('UsersView reset link', () => {
  it('shows the reset link once and clears it when closed', async () => {
    const wrapper = await mountPage('usr_root', [ROOT, OPS])

    await openRowMenu(wrapper, 1)
    mockResetPassword.mockResolvedValue({
      id: 'invite_reset',
      url: 'http://localhost:8090/admin/onboard#token=reset-token',
      expiresAt: new Date(Date.now() + 72 * 60 * 60 * 1000).toISOString(),
    })
    await wrapper.get('[data-testid="user-action-reset-link"]').trigger('click')
    await flushPromises()

    expect(wrapper.get('[data-testid="one-time-link"]').text()).toContain('token=reset-token')
    await wrapper.get('[data-testid="one-time-link-close"]').trigger('click')
    await flushPromises()
    expect(wrapper.find('[data-testid="one-time-link"]').exists()).toBe(false)
    wrapper.unmount()
  })
})
