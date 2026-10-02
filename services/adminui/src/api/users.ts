import { request } from '@/api/client'
import { bool, nullableStr, objectIn, recordsIn, str } from '@/api/shape'

/**
 * The staff user-management client, coded against `admin_auth`'s
 * `internal/api/admin_users.go`.
 *
 * The paths are the ones gateway_dev forwards unstripped (`/api/admin/users*`),
 * so nothing here is relative to a base URL. Every route is `admin` at the
 * gateway and again in the service's own `requireAdmin` middleware; the SPA's
 * role gate is presentation and this client is not an authorization point.
 *
 * Two shapes deserve a note:
 *
 *  - `is_root` is a property of the TARGET row, never of the caller. The session
 *    store's `/admin-auth/me` payload has no root flag (`loginUser` carries only
 *    id/name/roles), so the caller's own root bit is read off their row in the
 *    users list; see `UsersView` and `admin/userActions.ts`.
 *  - a link (`invite_url`/`reset_url`) is returned by exactly one response and
 *    is absent from every later read. It is not a field on `Invite`, on purpose:
 *    once the create response is gone, the link is gone.
 */

const BASE = '/api/admin/users'

/** The three roles the service's schema accepts, mirroring `auth/roles.ts`. */
export type StaffRole = 'viewer' | 'live_ops' | 'admin'

export interface AdminUser {
  id: string
  email: string
  name: string
  /** Exactly as stored: usually one of viewer/live_ops/admin. */
  roles: string[]
  isRoot: boolean
  status: 'active' | 'disabled'
  mfaEnrolled: boolean
  /** Null when the account has never signed in. */
  lastLoginAt: string | null
  createdAt: string
}

/** A live invite or password-reset link as the list returns it. Role is null for a reset. */
export interface Invite {
  id: string
  purpose: 'invite' | 'reset'
  email: string
  name: string
  role: string | null
  createdBy: string
  createdAt: string
  expiresAt: string
}

/**
 * The one and only response that carries a link. `id` is the invite id, so a
 * caller can reconcile it with the pending table after a refresh.
 */
export interface OneTimeLink {
  id: string
  url: string
  expiresAt: string
}

export interface InviteUserInput {
  email: string
  name: string
  role: StaffRole
}

export interface UpdateUserInput {
  /** Omitted leaves the roles alone. The service rejects an empty array. */
  roles?: string[]
  status?: 'active' | 'disabled'
}

function readRoles(value: unknown): string[] {
  return Array.isArray(value)
    ? value.filter((role): role is string => typeof role === 'string')
    : []
}

function readUser(row: Record<string, unknown>): AdminUser {
  return {
    id: str(row.id),
    email: str(row.email),
    name: str(row.name),
    roles: readRoles(row.roles),
    isRoot: bool(row.is_root),
    status: str(row.status) === 'disabled' ? 'disabled' : 'active',
    mfaEnrolled: bool(row.mfa_enrolled),
    lastLoginAt: nullableStr(row.last_login_at),
    createdAt: str(row.created_at),
  }
}

function readInvite(row: Record<string, unknown>): Invite {
  const purpose = str(row.purpose)
  return {
    id: str(row.id),
    purpose: purpose === 'reset' ? 'reset' : 'invite',
    email: str(row.email),
    name: str(row.name),
    role: typeof row.role === 'string' ? row.role : null,
    createdBy: str(row.created_by),
    createdAt: str(row.created_at),
    expiresAt: str(row.expires_at),
  }
}

/** Every staff account, newest last (the service's `ORDER BY created_at`). */
export async function listUsers(): Promise<AdminUser[]> {
  return recordsIn(await request<unknown>(BASE), 'users', BASE).map(readUser)
}

/** The live invite and reset links. Carries no token. */
export async function listInvites(): Promise<Invite[]> {
  const path = `${BASE}/invites`
  return recordsIn(await request<unknown>(path), 'invites', path).map(readInvite)
}

/**
 * Creates an invite. The response's `invite_url` is the only copy that will ever
 * exist; the caller must show it and then let it go.
 */
export async function inviteUser(input: InviteUserInput): Promise<OneTimeLink> {
  const body = objectIn(await request<unknown>(BASE, { method: 'POST', body: input }), BASE)
  return { id: str(body.invite_id), url: str(body.invite_url), expiresAt: str(body.expires_at) }
}

/** Revokes a pending invite or reset link. 204, so there is no body to read. */
export async function revokeInvite(id: string): Promise<void> {
  await request<void>(`${BASE}/invites/${encodeURIComponent(id)}`, { method: 'DELETE' })
}

/** Applies a roles and/or status change and returns the updated row. */
export async function updateUser(id: string, input: UpdateUserInput): Promise<AdminUser> {
  const path = `${BASE}/${encodeURIComponent(id)}`
  return readUser(objectIn(await request<unknown>(path, { method: 'PATCH', body: input }), path))
}

/** Mints a one-time password-reset link for a user. */
export async function resetUserPassword(id: string): Promise<OneTimeLink> {
  const path = `${BASE}/${encodeURIComponent(id)}/reset`
  const body = objectIn(await request<unknown>(path, { method: 'POST' }), path)
  return { id: str(body.invite_id), url: str(body.reset_url), expiresAt: str(body.expires_at) }
}

/**
 * Clears a user's second factor, recovery codes and sessions. The
 * 409 `mfa_not_enrolled` is the caller telling us there was nothing to reset;
 * the view hides the action for a user with no factor.
 */
export async function resetUserMfa(id: string): Promise<AdminUser> {
  const path = `${BASE}/${encodeURIComponent(id)}/mfa/reset`
  return readUser(objectIn(await request<unknown>(path, { method: 'POST' }), path))
}
