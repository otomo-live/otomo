<script setup lang="ts">
import {
  IonButton,
  IonContent,
  IonHeader,
  IonMenuButton,
  IonPage,
  IonSpinner,
  IonToolbar,
} from '@ionic/vue'
import { computed, onMounted, ref } from 'vue'

import { describeFailure, type FailureMessage } from '@/api/messages'
import {
  listInvites,
  listUsers,
  resetUserMfa,
  resetUserPassword,
  revokeInvite,
  updateUser,
  type AdminUser,
  type Invite,
  type OneTimeLink,
} from '@/api/users'
import ConfirmActionDialog from '@/admin/ConfirmActionDialog.vue'
import ChangeRoleDialog from '@/admin/ChangeRoleDialog.vue'
import InviteUserDialog from '@/admin/InviteUserDialog.vue'
import OneTimeLinkDialog from '@/admin/OneTimeLinkDialog.vue'
import {
  availableActions,
  canChangeRole,
  canResetMfa,
  canSendResetLink,
  canToggleStatus,
  type ActionCaller,
} from '@/admin/userActions'
import {
  Breadcrumbs,
  DataTable,
  EmptyState,
  ErrorPanel,
  PageHeader,
  Panel,
  StatusDot,
} from '@/components/ui'
import type { DataTableColumn } from '@/components/ui'
import { useSessionStore } from '@/stores/session'

/**
 * The staff user-management page, at `/admin/users`.
 *
 * The page mirrors D3's rules so a control the server would refuse is not shown:
 * `admin/userActions.ts` owns that decision and this view only renders what it
 * allows. The server still enforces every rule; when it refuses anyway (a race,
 * or a build older than the service), its COM-5 code is mapped to a sentence by
 * `describeFailure` and shown next to the tables.
 *
 * Root: `/admin-auth/me` returns `{id, name, roles}` and no `is_root` bit, and
 * the session store keeps exactly that payload. So the caller's root flag is read
 * from their own row in the users list (`is_root`), matched by id and falling
 * back to the sign-in email. Until the list has loaded, root is assumed false:
 * that hides the admin-role option for a moment rather than offering it.
 */

const store = useSessionStore()

const users = ref<AdminUser[]>([])
const invites = ref<Invite[]>([])
const loading = ref(true)
const failure = ref<FailureMessage | null>(null)
const toast = ref<string | null>(null)
const actionError = ref<string | null>(null)

const openMenu = ref<string | null>(null)

const inviteOpen = ref(false)
const changeRoleTarget = ref<AdminUser | null>(null)
const statusTarget = ref<AdminUser | null>(null)
const mfaTarget = ref<AdminUser | null>(null)
const revokeTarget = ref<Invite | null>(null)
const busy = ref(false)

/** A link exists only while this holds it; closing the dialog clears it. */
const oneTime = ref<{ title: string; link: OneTimeLink } | null>(null)

// `is_root` comes from the caller's own list row, not from the session.
const caller = computed<ActionCaller>(() => {
  const id = store.user?.id ?? ''
  const email = (store.email ?? '').toLowerCase()
  const own =
    users.value.find((user) => user.id === id) ??
    (email === '' ? undefined : users.value.find((user) => user.email.toLowerCase() === email))
  return { id, isRoot: own?.isRoot ?? false }
})

const userColumns: DataTableColumn[] = [
  { key: 'name', label: 'Name' },
  { key: 'email', label: 'Email' },
  { key: 'roles', label: 'Roles' },
  { key: 'status', label: 'Status' },
  { key: 'mfaEnrolled', label: 'MFA' },
  { key: 'lastLoginAt', label: 'Last login' },
  { key: 'actions', label: 'Actions' },
]

const inviteColumns: DataTableColumn[] = [
  { key: 'email', label: 'Email' },
  { key: 'role', label: 'Role' },
  { key: 'purpose', label: 'Purpose' },
  { key: 'expiresAt', label: 'Expires' },
  { key: 'createdBy', label: 'Created by' },
  { key: 'actions', label: '' },
]

function formatWhen(value: string | null): string {
  if (value === null || value === '') return 'Never'
  const at = Date.parse(value)
  return Number.isNaN(at) ? value : new Date(at).toLocaleString()
}

function toggleMenu(id: string): void {
  openMenu.value = openMenu.value === id ? null : id
}

async function load(): Promise<void> {
  loading.value = true
  failure.value = null
  try {
    const [loadedUsers, loadedInvites] = await Promise.all([listUsers(), listInvites()])
    users.value = loadedUsers
    invites.value = loadedInvites
  } catch (error) {
    failure.value = describeFailure(error)
  } finally {
    loading.value = false
  }
}

/** Re-reads both tables after a mutation without blanking them behind a spinner. */
async function refresh(): Promise<void> {
  try {
    const [loadedUsers, loadedInvites] = await Promise.all([listUsers(), listInvites()])
    users.value = loadedUsers
    invites.value = loadedInvites
  } catch (error) {
    actionError.value = describeFailure(error).summary
  }
}

onMounted(load)

async function onInvited(link: OneTimeLink): Promise<void> {
  inviteOpen.value = false
  oneTime.value = { title: 'Invite link', link }
  toast.value = 'Invite created'
  await refresh()
}

function openChangeRole(user: AdminUser): void {
  openMenu.value = null
  changeRoleTarget.value = user
}

async function onRoleChanged(user: AdminUser): Promise<void> {
  changeRoleTarget.value = null
  toast.value = `Role updated for ${user.email}`
  await refresh()
}

async function openResetLink(user: AdminUser): Promise<void> {
  openMenu.value = null
  actionError.value = null
  try {
    const link = await resetUserPassword(user.id)
    oneTime.value = { title: 'Password reset link', link }
  } catch (error) {
    actionError.value = describeFailure(error).summary
  }
}

async function confirmStatus(): Promise<void> {
  const user = statusTarget.value
  if (user === null || busy.value) return
  busy.value = true
  actionError.value = null
  try {
    const next = user.status === 'active' ? 'disabled' : 'active'
    await updateUser(user.id, { status: next })
    statusTarget.value = null
    toast.value = next === 'disabled' ? `Disabled ${user.email}` : `Enabled ${user.email}`
    await refresh()
  } catch (error) {
    statusTarget.value = null
    actionError.value = describeFailure(error).summary
  } finally {
    busy.value = false
  }
}

async function confirmResetMfa(): Promise<void> {
  const user = mfaTarget.value
  if (user === null || busy.value) return
  busy.value = true
  actionError.value = null
  try {
    await resetUserMfa(user.id)
    mfaTarget.value = null
    toast.value = `Second factor reset for ${user.email}`
    await refresh()
  } catch (error) {
    mfaTarget.value = null
    actionError.value = describeFailure(error).summary
  } finally {
    busy.value = false
  }
}

async function confirmRevoke(): Promise<void> {
  const invite = revokeTarget.value
  if (invite === null || busy.value) return
  busy.value = true
  actionError.value = null
  try {
    await revokeInvite(invite.id)
    revokeTarget.value = null
    toast.value = `Revoked the ${invite.purpose} link for ${invite.email}`
    await refresh()
  } catch (error) {
    revokeTarget.value = null
    actionError.value = describeFailure(error).summary
  } finally {
    busy.value = false
  }
}

const statusTitle = computed(() =>
  statusTarget.value?.status === 'active' ? 'Disable user' : 'Enable user',
)
const statusLabel = computed(() => (statusTarget.value?.status === 'active' ? 'Disable' : 'Enable'))
const statusMessage = computed(() => {
  const user = statusTarget.value
  if (user === null) return ''
  return user.status === 'active'
    ? `Disable ${user.name}? Their sessions are signed out and they cannot sign in until they are enabled again.`
    : `Enable ${user.name}? They will be able to sign in again.`
})
const statusDanger = computed(() => statusTarget.value?.status === 'active')

const mfaMessage = computed(() => {
  const user = mfaTarget.value
  if (user === null) return ''
  return `Reset the second factor for ${user.name}? Their authenticator factor and recovery codes are cleared, their sessions are signed out, and they must enroll again before their next second-factor sign-in.`
})

const revokeMessage = computed(() => {
  const invite = revokeTarget.value
  if (invite === null) return ''
  return `Revoke the ${invite.purpose === 'reset' ? 'reset' : 'invite'} link for ${invite.email}? It stops working immediately.`
})
</script>

<template>
  <IonPage>
    <IonHeader>
      <IonToolbar>
        <IonMenuButton slot="start" />
      </IonToolbar>
    </IonHeader>

    <IonContent>
      <div class="ds-page" data-testid="users-page">
        <PageHeader title="Users">
          <template #breadcrumb>
            <Breadcrumbs />
          </template>
          <template #actions>
            <IonButton data-testid="invite-user-open" @click="inviteOpen = true">
              Invite user
            </IonButton>
          </template>
        </PageHeader>

        <p v-if="toast !== null" class="users__toast" role="status" data-testid="users-toast">
          {{ toast }}
        </p>
        <p v-if="actionError !== null" class="users__error" role="alert" data-testid="users-error">
          {{ actionError }}
        </p>

        <div v-if="loading" class="users__busy">
          <IonSpinner name="dots" />
          <p>Loading staff users...</p>
        </div>

        <ErrorPanel
          v-else-if="failure !== null"
          :message="failure.summary"
          :detail="failure.detail"
          :request-id="failure.reference"
          retry-label="Try again"
          @retry="load"
        />

        <template v-else>
          <Panel title="Staff users" :padding="false">
            <template #actions>
              <IonButton fill="clear" :disabled="loading" @click="load">Refresh</IonButton>
            </template>
            <EmptyState
              v-if="users.length === 0"
              title="No staff users"
              detail="Nobody has been invited yet."
            />
            <DataTable v-else :columns="userColumns" :rows="users" row-key="id">
              <template #cell-name="{ row }">
                {{ row.name }}
                <span v-if="row.isRoot" class="users__badge" data-testid="user-root-badge">
                  root
                </span>
              </template>
              <template #cell-roles="{ row }">
                <span
                  v-for="role in row.roles"
                  :key="role"
                  class="users__badge"
                  data-testid="user-role"
                >
                  {{ role }}
                </span>
                <span v-if="row.roles.length === 0">—</span>
              </template>
              <template #cell-status="{ row }">
                <StatusDot
                  :status="row.status === 'active' ? 'ok' : 'danger'"
                  :label="row.status === 'active' ? 'Active' : 'Disabled'"
                />
              </template>
              <template #cell-mfaEnrolled="{ row }">
                <StatusDot
                  :status="row.mfaEnrolled ? 'ok' : 'neutral'"
                  :label="row.mfaEnrolled ? 'Enrolled' : 'Not enrolled'"
                />
              </template>
              <template #cell-lastLoginAt="{ row }">{{ formatWhen(row.lastLoginAt) }}</template>
              <template #cell-actions="{ row }">
                <div class="users__actions">
                  <button
                    type="button"
                    class="users__menu-toggle"
                    data-testid="user-actions-toggle"
                    @click.stop="toggleMenu(row.id)"
                  >
                    Actions
                  </button>
                  <div v-if="openMenu === row.id" class="users__menu" role="menu">
                    <button
                      v-if="canChangeRole(caller, row)"
                      type="button"
                      class="users__menu-item"
                      data-testid="user-action-change-role"
                      @click.stop="openChangeRole(row)"
                    >
                      Change role
                    </button>
                    <button
                      v-if="canToggleStatus(caller, row)"
                      type="button"
                      class="users__menu-item"
                      data-testid="user-action-toggle-status"
                      @click.stop="statusTarget = row"
                    >
                      {{ row.status === 'active' ? 'Disable' : 'Enable' }}
                    </button>
                    <button
                      v-if="canSendResetLink(caller, row)"
                      type="button"
                      class="users__menu-item"
                      data-testid="user-action-reset-link"
                      @click.stop="openResetLink(row)"
                    >
                      Send reset link
                    </button>
                    <button
                      v-if="canResetMfa(caller, row)"
                      type="button"
                      class="users__menu-item"
                      data-testid="user-action-reset-mfa"
                      @click.stop="mfaTarget = row"
                    >
                      Reset MFA
                    </button>
                    <p
                      v-if="availableActions(caller, row).length === 0"
                      class="users__menu-empty"
                      data-testid="user-actions-none"
                    >
                      No actions
                    </p>
                  </div>
                </div>
              </template>
            </DataTable>
          </Panel>

          <Panel title="Pending invites" :padding="false" class="users__invites">
            <EmptyState
              v-if="invites.length === 0"
              title="No pending invites"
              detail="Invites and reset links appear here until they are used or revoked."
            />
            <DataTable v-else :columns="inviteColumns" :rows="invites" row-key="id">
              <template #cell-role="{ row }">{{ row.role ?? '—' }}</template>
              <template #cell-purpose="{ row }">{{ row.purpose }}</template>
              <template #cell-expiresAt="{ row }">{{ formatWhen(row.expiresAt) }}</template>
              <template #cell-actions="{ row }">
                <button
                  type="button"
                  class="users__revoke"
                  data-testid="revoke-invite"
                  @click.stop="revokeTarget = row"
                >
                  Revoke
                </button>
              </template>
            </DataTable>
          </Panel>
        </template>
      </div>
    </IonContent>

    <InviteUserDialog
      :is-open="inviteOpen"
      :is-root="caller.isRoot"
      @invited="onInvited"
      @dismiss="inviteOpen = false"
    />

    <ChangeRoleDialog
      v-if="changeRoleTarget !== null"
      :is-open="true"
      :user="changeRoleTarget"
      :caller="caller"
      @changed="onRoleChanged"
      @dismiss="changeRoleTarget = null"
    />

    <ConfirmActionDialog
      v-if="statusTarget !== null"
      :is-open="true"
      :title="statusTitle"
      :message="statusMessage"
      :confirm-label="statusLabel"
      :danger="statusDanger"
      :pending="busy"
      @confirm="confirmStatus"
      @cancel="statusTarget = null"
    />

    <ConfirmActionDialog
      v-if="mfaTarget !== null"
      :is-open="true"
      title="Reset second factor"
      :message="mfaMessage"
      confirm-label="Reset MFA"
      danger
      :pending="busy"
      @confirm="confirmResetMfa"
      @cancel="mfaTarget = null"
    />

    <ConfirmActionDialog
      v-if="revokeTarget !== null"
      :is-open="true"
      title="Revoke pending link"
      :message="revokeMessage"
      confirm-label="Revoke"
      danger
      :pending="busy"
      @confirm="confirmRevoke"
      @cancel="revokeTarget = null"
    />

    <OneTimeLinkDialog
      v-if="oneTime !== null"
      :is-open="true"
      :link="oneTime.link"
      :title="oneTime.title"
      @close="oneTime = null"
    />
  </IonPage>
</template>

<style scoped>
.users__toast {
  background: var(--ds-ok-soft);
  border: 1px solid var(--ds-ok);
  border-radius: var(--ds-radius-sm);
  color: var(--ds-ok);
  font-weight: 600;
  margin-bottom: var(--ds-space-4);
  padding: var(--ds-space-2) var(--ds-space-3);
}

.users__error {
  background: var(--ds-danger-soft);
  border: 1px solid var(--ds-danger);
  border-radius: var(--ds-radius-sm);
  color: var(--ds-danger);
  margin-bottom: var(--ds-space-4);
  padding: var(--ds-space-3);
}

.users__busy {
  align-items: center;
  display: flex;
  flex-direction: column;
  gap: var(--ds-space-2);
  padding-top: var(--ds-space-6);
}

.users__invites {
  margin-top: var(--ds-space-5);
}

.users__badge {
  background: var(--ds-neutral-soft);
  border-radius: var(--ds-radius-sm);
  color: var(--ds-accent);
  display: inline-block;
  font-size: var(--ds-font-size-xs);
  font-weight: 600;
  margin-right: var(--ds-space-1);
  padding: var(--ds-space-1) var(--ds-space-2);
  text-transform: uppercase;
}

.users__actions {
  position: relative;
}

.users__menu-toggle,
.users__revoke {
  background: var(--ds-surface);
  border: 1px solid var(--ds-border);
  border-radius: var(--ds-radius-sm);
  color: var(--ds-text);
  cursor: pointer;
  font: inherit;
  font-size: var(--ds-font-size-sm);
  padding: var(--ds-space-1) var(--ds-space-3);
}

.users__revoke {
  color: var(--ds-danger);
}

.users__menu {
  background: var(--ds-surface-raised);
  border: 1px solid var(--ds-border);
  border-radius: var(--ds-radius-sm);
  box-shadow: var(--ds-shadow-1);
  display: flex;
  flex-direction: column;
  min-width: 11rem;
  padding: var(--ds-space-1);
  position: absolute;
  right: 0;
  top: 100%;
  z-index: 2;
}

.users__menu-item {
  background: none;
  border: none;
  border-radius: var(--ds-radius-sm);
  color: var(--ds-text);
  cursor: pointer;
  font: inherit;
  font-size: var(--ds-font-size-sm);
  padding: var(--ds-space-2);
  text-align: left;
}

.users__menu-item:hover {
  background: var(--ds-neutral-soft);
}

.users__menu-empty {
  color: var(--ds-text-muted);
  font-size: var(--ds-font-size-sm);
  margin: 0;
  padding: var(--ds-space-2);
}
</style>
