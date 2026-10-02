import { describe, expect, it } from 'vitest'

import { ApiError } from '@/api/errors'
import { describeFailure } from '@/api/messages'
import {
  availableActions,
  canChangeRole,
  canResetMfa,
  canSendResetLink,
  canToggleStatus,
  initialRole,
  roleOptions,
  type ActionCaller,
  type ActionTarget,
} from '@/admin/userActions'

/**
 * The D3 visibility matrix, with no page mounted.
 *
 * The service re-derives every one of these under a row lock; the value of the
 * matrix here is that the page does not offer a control the server will refuse.
 * The caller's root bit is the one the view reads off their own row in the users
 * list, since `/admin-auth/me` does not carry it.
 */

const CALLER: ActionCaller = { id: 'usr_caller', isRoot: false }
const ROOT_CALLER: ActionCaller = { id: 'usr_root', isRoot: true }

function target(overrides: Partial<ActionTarget> = {}): ActionTarget {
  return {
    id: 'usr_target',
    isRoot: false,
    roles: ['live_ops'],
    status: 'active',
    mfaEnrolled: true,
    ...overrides,
  }
}

const ROOT_TARGET = target({ id: 'usr_root', isRoot: true, roles: ['admin'] })
const ADMIN_TARGET = target({ id: 'usr_admin', roles: ['admin'] })
const OPS_TARGET = target({ id: 'usr_liveops', roles: ['live_ops'] })
const VIEWER_TARGET = target({ id: 'usr_viewer', roles: ['viewer'], mfaEnrolled: false })

describe('action visibility — non-root caller', () => {
  it('hides role, status and MFA on the caller’s own row', () => {
    const self = target({ id: CALLER.id, roles: ['admin'] })
    expect(canChangeRole(CALLER, self)).toBe(false)
    expect(canToggleStatus(CALLER, self)).toBe(false)
    expect(canResetMfa(CALLER, self)).toBe(false)
    // Reset is refused for an admin target by a non-root caller, so it is hidden too.
    expect(canSendResetLink(CALLER, self)).toBe(false)
    expect(availableActions(CALLER, self)).toEqual([])
  })

  it('hides everything on the root row', () => {
    expect(availableActions(CALLER, ROOT_TARGET)).toEqual([])
  })

  it('hides everything on another admin row', () => {
    expect(availableActions(CALLER, ADMIN_TARGET)).toEqual([])
  })

  it('offers the full set on a live_ops row', () => {
    expect(availableActions(CALLER, OPS_TARGET)).toEqual([
      'change-role',
      'toggle-status',
      'reset-link',
      'reset-mfa',
    ])
  })

  it('offers a viewer the reset link but no MFA action when not enrolled', () => {
    expect(availableActions(CALLER, VIEWER_TARGET)).toEqual([
      'change-role',
      'toggle-status',
      'reset-link',
    ])
  })
})

describe('action visibility — root caller', () => {
  it('hides everything on the root row, including its own', () => {
    expect(availableActions(ROOT_CALLER, ROOT_TARGET)).toEqual([])
    const self = target({ id: ROOT_CALLER.id, isRoot: true, roles: ['admin'] })
    expect(availableActions(ROOT_CALLER, self)).toEqual([])
  })

  it('offers the full set on an admin row', () => {
    expect(availableActions(ROOT_CALLER, ADMIN_TARGET)).toEqual([
      'change-role',
      'toggle-status',
      'reset-link',
      'reset-mfa',
    ])
  })

  it('still hides MFA reset when the target has no factor', () => {
    const adminNoMfa = target({ id: 'usr_admin2', roles: ['admin'], mfaEnrolled: false })
    expect(canResetMfa(ROOT_CALLER, adminNoMfa)).toBe(false)
    expect(canChangeRole(ROOT_CALLER, adminNoMfa)).toBe(true)
  })
})

describe('role options and initial role', () => {
  it('only root is offered admin', () => {
    expect(roleOptions(CALLER)).toEqual(['viewer', 'live_ops'])
    expect(roleOptions(ROOT_CALLER)).toEqual(['viewer', 'live_ops', 'admin'])
  })

  it('starts the change-role dialog on the target’s current role', () => {
    expect(initialRole(CALLER, OPS_TARGET)).toBe('live_ops')
    expect(initialRole(ROOT_CALLER, ADMIN_TARGET)).toBe('admin')
    // A role the caller cannot grant falls back to the first offerable one.
    expect(initialRole(CALLER, ADMIN_TARGET)).toBe('viewer')
  })
})

describe('user-management error mapping', () => {
  const cases: Record<string, string> = {
    root_protected: 'The root account can only be changed through the CLI.',
    self_modification: 'You cannot change your own account.',
    insufficient_role: 'That account does not have access to this area.',
    mfa_not_enrolled: 'That account has no second factor to reset.',
    invite_pending: 'A pending invite already exists for that address.',
    already_exists: 'That already exists.',
    not_found: 'That is no longer there. It may have been removed.',
  }

  for (const [code, summary] of Object.entries(cases)) {
    it(`maps ${code} to a sentence`, () => {
      const error = new ApiError({ status: 403, code, message: 'raw', requestId: 'req_1' })
      expect(describeFailure(error).summary).toBe(summary)
    })
  }
})
