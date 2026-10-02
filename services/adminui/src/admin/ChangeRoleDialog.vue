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
import { updateUser, type AdminUser, type StaffRole } from '@/api/users'
import { initialRole, roleOptions, type ActionCaller } from '@/admin/userActions'

/**
 * The change-role form for one user.
 *
 * Roles are a single choice here even though the wire carries an array: this
 * service treats the three roles as a ladder and every seeded account holds one,
 * so the dialog sends `roles: [chosen]`. The admin option exists only for a root
 * caller, which is D3 on screen; the server re-checks.
 */

const props = defineProps<{
  isOpen: boolean
  user: AdminUser
  caller: ActionCaller
}>()

const emit = defineEmits<{ changed: [user: AdminUser]; dismiss: [] }>()

const selected = ref<StaffRole>(initialRole(props.caller, props.user))
const submitting = ref(false)
const formError = ref<string | null>(null)

const roles = computed(() => roleOptions(props.caller))

watch(
  () => props.isOpen,
  (open) => {
    if (!open) return
    selected.value = initialRole(props.caller, props.user)
    formError.value = null
  },
  { immediate: true },
)

async function submit(): Promise<void> {
  if (submitting.value) return
  submitting.value = true
  formError.value = null
  try {
    const updated = await updateUser(props.user.id, { roles: [selected.value] })
    emit('changed', updated)
  } catch (error) {
    formError.value = describeFailure(error).summary
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <IonModal :is-open="props.isOpen" data-testid="change-role-dialog" @did-dismiss="emit('dismiss')">
    <IonHeader>
      <IonToolbar>
        <IonTitle>Change role — {{ props.user.email }}</IonTitle>
      </IonToolbar>
    </IonHeader>
    <IonContent class="ion-padding">
      <fieldset class="change-role__roles">
        <legend>Role</legend>
        <label v-for="option in roles" :key="option" class="change-role__option">
          <input
            v-model="selected"
            type="radio"
            name="change-role"
            :value="option"
            :data-testid="`change-role-${option}`"
          />
          <span>{{ option }}</span>
        </label>
      </fieldset>

      <p
        v-if="formError !== null"
        class="change-role__error"
        role="alert"
        data-testid="change-role-error"
      >
        {{ formError }}
      </p>

      <div class="change-role__actions">
        <IonButton fill="clear" color="medium" type="button" @click="emit('dismiss')">
          Cancel
        </IonButton>
        <IonButton :disabled="submitting" data-testid="change-role-submit" @click="submit">
          <IonSpinner v-if="submitting" name="dots" />
          <span v-else>Save role</span>
        </IonButton>
      </div>
    </IonContent>
  </IonModal>
</template>

<style scoped>
.change-role__roles {
  border: 0;
  display: flex;
  flex-direction: column;
  gap: var(--ds-space-2);
  margin: 0;
  padding: 0;
}

.change-role__option {
  align-items: center;
  display: flex;
  gap: var(--ds-space-2);
}

.change-role__error {
  color: var(--ds-danger);
  font-size: var(--ds-font-size-sm);
}

.change-role__actions {
  display: flex;
  gap: var(--ds-space-2);
  justify-content: flex-end;
  margin-top: var(--ds-space-4);
}
</style>
