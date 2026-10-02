<script setup lang="ts">
import {
  IonButton,
  IonContent,
  IonHeader,
  IonModal,
  IonSpinner,
  IonTitle,
  IonToolbar,
} from '@ionic/vue'
import { computed, ref, watch } from 'vue'

import { describeFailure } from '@/api/messages'
import { inviteUser, type OneTimeLink, type StaffRole } from '@/api/users'
import { roleOptions } from '@/admin/userActions'

/**
 * The invite form: email, name, role, then the one-time link the parent shows.
 *
 * The role list is the D3 gate made visible: `admin` is offered only when the
 * caller is root (`roleOptions`). The service repeats the check and answers
 * `insufficient_role` if a non-root request smuggles it in anyway, which is what
 * the error slot is for.
 *
 * On success the dialog emits the link and closes immediately. It does not hold
 * the link after that: the parent owns showing it once, and keeping a second
 * copy here would be exactly the leak the one-time rule forbids.
 */

const props = defineProps<{
  isOpen: boolean
  /** Whether the signed-in caller is root; `admin` is only offered when it is. */
  isRoot: boolean
}>()

const emit = defineEmits<{ invited: [link: OneTimeLink]; dismiss: [] }>()

const email = ref('')
const name = ref('')
const role = ref<StaffRole>('viewer')
const submitting = ref(false)
const formError = ref<string | null>(null)

const roles = computed(() => roleOptions({ id: '', isRoot: props.isRoot }))

watch(
  () => props.isOpen,
  (open) => {
    if (!open) return
    email.value = ''
    name.value = ''
    role.value = 'viewer'
    formError.value = null
  },
)

const emailValid = computed(() => {
  const value = email.value.trim()
  return value.length <= 254 && value.length >= 3 && value.split('@').length === 2
})

const canSubmit = computed(() => !submitting.value && emailValid.value && name.value.trim() !== '')

async function submit(): Promise<void> {
  if (submitting.value) return
  formError.value = null
  if (!canSubmit.value) {
    formError.value = 'A valid email and a name are required.'
    return
  }
  submitting.value = true
  try {
    const link = await inviteUser({
      email: email.value.trim(),
      name: name.value.trim(),
      role: role.value,
    })
    emit('invited', link)
  } catch (error) {
    formError.value = describeFailure(error).summary
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <IonModal :is-open="props.isOpen" data-testid="invite-dialog" @did-dismiss="emit('dismiss')">
    <IonHeader>
      <IonToolbar>
        <IonTitle>Invite a user</IonTitle>
      </IonToolbar>
    </IonHeader>
    <IonContent class="ion-padding">
      <form class="invite" @submit.prevent="submit">
        <label class="invite__field" for="invite-email">
          <span>Email</span>
          <input
            id="invite-email"
            v-model="email"
            class="invite__input"
            type="email"
            autocomplete="off"
            data-testid="invite-email"
          />
        </label>

        <label class="invite__field" for="invite-name">
          <span>Name</span>
          <input
            id="invite-name"
            v-model="name"
            class="invite__input"
            type="text"
            autocomplete="off"
            data-testid="invite-name"
          />
        </label>

        <label class="invite__field" for="invite-role">
          <span>Role</span>
          <select id="invite-role" v-model="role" class="invite__input" data-testid="invite-role">
            <option
              v-for="option in roles"
              :key="option"
              :value="option"
              :data-testid="`invite-role-${option}`"
            >
              {{ option }}
            </option>
          </select>
        </label>
        <p v-if="!props.isRoot" class="invite__hint" data-testid="invite-admin-hint">
          Only root can grant the admin role.
        </p>

        <p v-if="formError !== null" class="invite__error" role="alert" data-testid="invite-error">
          {{ formError }}
        </p>

        <div class="invite__actions">
          <IonButton fill="clear" color="medium" type="button" @click="emit('dismiss')">
            Cancel
          </IonButton>
          <IonButton
            type="submit"
            :disabled="!canSubmit"
            data-testid="invite-submit"
            @click="submit"
          >
            <IonSpinner v-if="submitting" name="dots" />
            <span v-else>Send invite</span>
          </IonButton>
        </div>
      </form>
    </IonContent>
  </IonModal>
</template>

<style scoped>
.invite {
  display: flex;
  flex-direction: column;
  gap: var(--ds-space-2);
  max-width: 32rem;
}

.invite__field {
  display: flex;
  flex-direction: column;
  gap: var(--ds-space-1);
}

.invite__input {
  background: var(--ds-surface);
  border: 1px solid var(--ds-border);
  border-radius: var(--ds-radius-sm);
  color: var(--ds-text);
  font-family: inherit;
  font-size: var(--ds-font-size-md);
  padding: var(--ds-space-2);
}

.invite__hint {
  color: var(--ds-text-muted);
  font-size: var(--ds-font-size-sm);
  margin: 0;
}

.invite__error {
  color: var(--ds-danger);
  font-size: var(--ds-font-size-sm);
  margin: 0;
}

.invite__actions {
  display: flex;
  gap: var(--ds-space-2);
  justify-content: flex-end;
  margin-top: var(--ds-space-4);
}
</style>
