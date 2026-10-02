import { beforeEach, describe, expect, it } from 'vitest'

import { ApiError } from '@/api/errors'
import {
  inviteUser,
  listInvites,
  listUsers,
  resetUserMfa,
  resetUserPassword,
  revokeInvite,
  updateUser,
} from '@/api/users'

/**
 * The fixture's user-management routes through the real typed client.
 *
 * The mock enforces D3 the way `admin_auth` does, so these assert both halves at
 * once: that the reader parses the wire shape, and that a forbidden request
 * comes back as the COM-5 code the page maps to a sentence. The caller is the
 * fixture's last login, so each test signs in first.
 */

async function login(email: string): Promise<void> {
  const response = await fetch('/admin-auth/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ email, password: 'x' }),
  })
  expect(response.status).toBe(200)
}

async function codeOf(action: () => Promise<unknown>): Promise<string> {
  try {
    await action()
  } catch (error) {
    if (error instanceof ApiError) return error.code
    throw error
  }
  throw new Error('the call was expected to fail')
}

beforeEach(async () => {
  // The fixture keeps one session; a test that does not sign in would inherit
  // the previous test's caller and role.
  await fetch('/admin-auth/logout', { method: 'POST' })
})

describe('users fixture', () => {
  it('lists every account and marks the root row', async () => {
    await login('root@example.com')
    const users = await listUsers()

    const root = users.find((user) => user.email === 'root@example.com')
    expect(root?.isRoot).toBe(true)
    expect(root?.roles).toEqual(['admin'])
    expect(users.some((user) => user.email === 'admin@example.com' && !user.isRoot)).toBe(true)
    expect(users.some((user) => user.status === 'disabled')).toBe(true)
    expect(users.some((user) => user.mfaEnrolled)).toBe(true)
  })

  it('refuses the list to a non-admin caller', async () => {
    await login('viewer@example.com')
    expect(await codeOf(() => listUsers())).toBe('insufficient_role')
  })

  it('lets root grant admin and returns the link once', async () => {
    await login('root@example.com')
    const link = await inviteUser({
      email: 'new.admin@example.com',
      name: 'New Admin',
      role: 'admin',
    })
    expect(link.url).toContain('http://localhost:8090/admin/onboard#token=')
    expect(link.id).not.toBe('')

    // The pending list knows the invite but never the token.
    const invites = await listInvites()
    const pending = invites.find((invite) => invite.id === link.id)
    expect(pending?.role).toBe('admin')
    expect(JSON.stringify(pending)).not.toContain('token=')
  })

  it('refuses a non-root admin granting admin', async () => {
    await login('admin@example.com')
    expect(
      await codeOf(() => inviteUser({ email: 'nope@example.com', name: 'Nope', role: 'admin' })),
    ).toBe('insufficient_role')
  })

  it('reports a duplicate or pending invite', async () => {
    await login('admin@example.com')
    expect(
      await codeOf(() => inviteUser({ email: 'liveops@example.com', name: 'Dup', role: 'viewer' })),
    ).toBe('already_exists')

    const first = await inviteUser({
      email: 'pending@example.com',
      name: 'Pending',
      role: 'viewer',
    })
    expect(
      await codeOf(() =>
        inviteUser({ email: 'pending@example.com', name: 'Again', role: 'viewer' }),
      ),
    ).toBe('invite_pending')

    await revokeInvite(first.id)
    expect((await listInvites()).some((invite) => invite.id === first.id)).toBe(false)
  })
})

describe('user updates fixture', () => {
  it('protects the root row and the caller’s own row', async () => {
    await login('admin@example.com')
    const users = await listUsers()
    const root = users.find((user) => user.isRoot)!
    const self = users.find((user) => user.email === 'admin@example.com')!

    expect(await codeOf(() => updateUser(root.id, { status: 'disabled' }))).toBe('root_protected')
    expect(await codeOf(() => updateUser(self.id, { roles: ['viewer'] }))).toBe('self_modification')
  })

  it('requires root to move admin access', async () => {
    await login('admin@example.com')
    const users = await listUsers()
    // Adding admin to a live_ops row is the admin-membership change the store
    // reserves to root; the root row itself is protected before this rule runs.
    const ops = users.find((user) => user.email === 'liveops@example.com')!
    expect(await codeOf(() => updateUser(ops.id, { roles: ['admin'] }))).toBe('insufficient_role')
  })

  it('lets an admin change a live_ops role and disable the account', async () => {
    await login('admin@example.com')
    const users = await listUsers()
    const ops = users.find((user) => user.email === 'liveops@example.com')!

    const renamed = await updateUser(ops.id, { roles: ['viewer'] })
    expect(renamed.roles).toEqual(['viewer'])

    const disabled = await updateUser(ops.id, { status: 'disabled' })
    expect(disabled.status).toBe('disabled')
  })

  it('mints a reset link whose token is not in the invites list', async () => {
    await login('admin@example.com')
    const users = await listUsers()
    const ops = users.find((user) => user.email === 'liveops@example.com')!

    const link = await resetUserPassword(ops.id)
    expect(link.url).toContain('#token=')
    const invites = await listInvites()
    const reset = invites.find((invite) => invite.id === link.id)
    expect(reset?.purpose).toBe('reset')
    expect(reset?.role).toBeNull()
    expect(JSON.stringify(reset)).not.toContain('token=')
  })
})

describe('mfa reset fixture', () => {
  it('refuses when the account has no factor', async () => {
    await login('root@example.com')
    const users = await listUsers()
    const ops = users.find((user) => user.email === 'liveops@example.com')!
    expect(ops.mfaEnrolled).toBe(false)
    expect(await codeOf(() => resetUserMfa(ops.id))).toBe('mfa_not_enrolled')
  })

  it('clears the factor when one is enrolled', async () => {
    await login('root@example.com')
    const users = await listUsers()
    const admin = users.find((user) => user.email === 'admin@example.com')!
    expect(admin.mfaEnrolled).toBe(true)

    const updated = await resetUserMfa(admin.id)
    expect(updated.mfaEnrolled).toBe(false)
  })
})
