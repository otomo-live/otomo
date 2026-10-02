import type { StaffRole } from '@/api/users'

/**
 * Which row actions D3 permits, in one pure module.
 *
 * The service is still the authority (`admin_auth`'s `UpdateUser`, `CreateReset`
 * and `ResetMFA` re-derive each of these under a row lock), but the page must
 * not offer something the server will refuse: a disabled "Change role" button is
 * a worse explanation than no button, and a menu full of buttons that all 403 is
 * worse still. Keeping the decision here rather than in the template is what
 * lets the 2x4 matrix be tested without mounting a page.
 *
 * The rules, read from `store/admin_users.go`:
 *
 *  - the root account is break-glass and every mutation refuses it
 *    (`root_protected`);
 *  - nobody may change their own account (`self_modification`), except that a
 *    password reset is not self-modification on the server — it is still hidden
 *    here for a different reason, below;
 *  - granting or removing `admin`, and changing an admin's status, requires root
 *    (`insufficient_role`);
 *  - MFA can only be reset when there is a factor to reset (`mfa_not_enrolled`).
 */

/** The caller's identity as far as the rules need it. */
export interface ActionCaller {
  id: string
  /** Derived from the caller's own row in the users list; see UsersView. */
  isRoot: boolean
}

/** The target row's facts. */
export interface ActionTarget {
  id: string
  isRoot: boolean
  roles: readonly string[]
  status: string
  mfaEnrolled: boolean
}

export type UserAction = 'change-role' | 'toggle-status' | 'reset-link' | 'reset-mfa'

export const ADMIN_ROLE = 'admin'

function holdsAdmin(target: ActionTarget): boolean {
  return target.roles.includes(ADMIN_ROLE)
}

function isSelf(caller: ActionCaller, target: ActionTarget): boolean {
  return caller.id === target.id
}

/**
 * The root-only bar: the target is the root row, or it carries `admin` and the
 * caller is not root. Every action except the reset link shares this; the reset
 * link only differs by not caring about self-modification.
 */
function blockedByRootRule(caller: ActionCaller, target: ActionTarget): boolean {
  if (target.isRoot) return true
  if (holdsAdmin(target) && !caller.isRoot) return true
  return false
}

export function canChangeRole(caller: ActionCaller, target: ActionTarget): boolean {
  if (blockedByRootRule(caller, target)) return false
  return !isSelf(caller, target)
}

export function canToggleStatus(caller: ActionCaller, target: ActionTarget): boolean {
  if (blockedByRootRule(caller, target)) return false
  return !isSelf(caller, target)
}

export function canSendResetLink(caller: ActionCaller, target: ActionTarget): boolean {
  // The service's CreateReset does not reject self, but it does reject the root
  // row and an admin target when the caller is not root; those are the cases
  // that would 403, so they are the cases hidden here.
  return !blockedByRootRule(caller, target)
}

export function canResetMfa(caller: ActionCaller, target: ActionTarget): boolean {
  if (blockedByRootRule(caller, target)) return false
  if (isSelf(caller, target)) return false
  return target.mfaEnrolled
}

export function availableActions(caller: ActionCaller, target: ActionTarget): UserAction[] {
  const actions: UserAction[] = []
  if (canChangeRole(caller, target)) actions.push('change-role')
  if (canToggleStatus(caller, target)) actions.push('toggle-status')
  if (canSendResetLink(caller, target)) actions.push('reset-link')
  if (canResetMfa(caller, target)) actions.push('reset-mfa')
  return actions
}

/**
 * The roles a caller may put on a user. `admin` is root-only (D3: granting admin
 * requires root); a non-root admin sees viewer and live_ops.
 */
export function roleOptions(caller: ActionCaller): StaffRole[] {
  return caller.isRoot ? ['viewer', 'live_ops', 'admin'] : ['viewer', 'live_ops']
}

/** The role a change-role dialog should start on, given the target's current roles. */
export function initialRole(caller: ActionCaller, target: ActionTarget): StaffRole {
  const options = roleOptions(caller)
  const current = options.find((role) => target.roles.includes(role))
  return current ?? options[0]
}
