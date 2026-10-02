<script setup lang="ts">
import { IonButton, IonCheckbox, IonItem, IonNote } from '@ionic/vue'
import { ref } from 'vue'

import { downloadRecoveryCodes } from '@/auth/recoveryCodes'

/**
 * The ten one-time recovery codes and the "I have saved these" gate, shared by
 * the enrollment screen and the account page's setup and regenerate flows.
 *
 * The confirmation is a `v-model` rather than internal state: the page that owns
 * the codes decides what "continue" does, and both callers disable their own
 * continue button until the model says the codes were saved.
 */

const props = defineProps<{
  codes: string[]
  /** Whether the person has confirmed they saved the codes. */
  modelValue: boolean
}>()

const emit = defineEmits<{ 'update:modelValue': [value: boolean] }>()

const copied = ref(false)

async function copyAll(): Promise<void> {
  try {
    await navigator.clipboard?.writeText(props.codes.join('\n'))
    copied.value = true
    setTimeout(() => {
      copied.value = false
    }, 1500)
  } catch {
    // Denied or unavailable: the codes stay selectable on screen.
  }
}
</script>

<template>
  <div class="recovery">
    <ul class="recovery__codes" data-testid="recovery-codes">
      <li v-for="recovery in codes" :key="recovery">
        <code>{{ recovery }}</code>
      </li>
    </ul>

    <div class="recovery__actions">
      <IonButton fill="outline" size="small" @click="copyAll">
        {{ copied ? 'Copied' : 'Copy all' }}
      </IonButton>
      <IonButton
        fill="outline"
        size="small"
        data-testid="recovery-download"
        @click="downloadRecoveryCodes(codes)"
      >
        Download .txt
      </IonButton>
    </div>

    <IonItem lines="none" class="recovery__saved">
      <IonCheckbox
        :model-value="modelValue"
        data-testid="recovery-saved"
        label-placement="end"
        justify="start"
        @update:model-value="emit('update:modelValue', Boolean($event))"
      >
        I have saved these codes
      </IonCheckbox>
    </IonItem>

    <IonNote v-if="!modelValue" class="recovery__note">
      Continue is disabled until you confirm you have saved them.
    </IonNote>
  </div>
</template>

<style scoped>
.recovery__codes {
  display: grid;
  gap: var(--ds-space-2);
  grid-template-columns: repeat(2, minmax(0, 1fr));
  list-style: none;
  margin: 0 0 var(--ds-space-3);
  padding: 0;
}

.recovery__codes code {
  background: var(--ds-neutral-soft);
  border-radius: var(--ds-radius-sm);
  display: block;
  font-family: var(--ds-font-mono);
  font-size: var(--ds-font-size-sm);
  padding: var(--ds-space-2);
  text-align: center;
}

.recovery__actions {
  display: flex;
  gap: var(--ds-space-2);
  margin-bottom: var(--ds-space-3);
}

.recovery__saved {
  --padding-start: 0;
}

.recovery__note {
  color: var(--ds-text-muted);
  display: block;
  font-size: var(--ds-font-size-sm);
  margin-top: var(--ds-space-2);
}
</style>
