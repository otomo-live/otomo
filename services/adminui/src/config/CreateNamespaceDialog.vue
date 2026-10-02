<script setup lang="ts">
import { IonButton, IonModal, IonSpinner } from '@ionic/vue'
import { computed, ref, watch } from 'vue'

import { createNamespace, type NamespaceSummary } from '@/api/config'
import { ApiError } from '@/api/errors'
import { describeFailure } from '@/api/messages'
import { NAMESPACE_NAME_EXAMPLE, namespaceNameProblem } from '@/config/namespaceName'

/**
 * The create-namespace dialog, shown only to admins (the list wraps it in a
 * RoleGate).
 *
 * The name is checked live with Config's own regexp so the pattern is explained
 * before the request rather than after a 400, but the server is still the
 * authority: a 409 shows the duplicate on the field, and a 400 shows the
 * server's message rather than closing over it. Audience is a radio pair rather
 * than a select because there are two and they are not interchangeable: the
 * chosen one is fixed for the namespace's life, and the form says so.
 */

const props = defineProps<{ isOpen: boolean }>()

const emit = defineEmits<{
  created: [namespace: NamespaceSummary]
  dismiss: []
}>()

const name = ref('')
const audience = ref<'client' | 'server'>('client')
const description = ref('')
const touched = ref(false)
const attempted = ref(false)
const submitting = ref(false)
const formError = ref<string | null>(null)

const nameProblem = computed(() => namespaceNameProblem(name.value.trim()))
const showNameProblem = computed(
  () => (touched.value || attempted.value) && nameProblem.value !== null,
)

watch(
  () => props.isOpen,
  (open) => {
    if (!open) return
    // A reopened dialog starts clean: the previous namespace's values are not a
    // sensible default for the next one.
    name.value = ''
    audience.value = 'client'
    description.value = ''
    touched.value = false
    attempted.value = false
    formError.value = null
  },
)

async function submit(): Promise<void> {
  if (submitting.value) return
  attempted.value = true
  formError.value = null

  // The trim is what the regexp sees and what is sent: a name with a stray space
  // is a typo, not a different namespace.
  const trimmed = name.value.trim()
  if (namespaceNameProblem(trimmed) !== null) return

  submitting.value = true
  try {
    const created = await createNamespace({
      name: trimmed,
      audience: audience.value,
      description: description.value,
    })
    emit('created', created)
  } catch (error) {
    // The server's own words on the field: `already_exists` and
    // `validation_failed` are both about what is in the form, and both have a
    // sentence worth showing there. Everything else is not the form's fault.
    if (
      error instanceof ApiError &&
      (error.code === 'already_exists' || error.code === 'validation_failed')
    ) {
      formError.value = error.message
    } else {
      formError.value = describeFailure(error).summary
    }
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <IonModal :is-open="props.isOpen" @did-dismiss="emit('dismiss')">
    <div class="create" role="dialog" aria-modal="true" aria-labelledby="create-namespace-title">
      <h2 id="create-namespace-title" class="create__title">Create namespace</h2>

      <form class="create__form" @submit.prevent="submit">
        <label class="create__label" for="create-namespace-name">Name</label>
        <input
          id="create-namespace-name"
          v-model="name"
          class="create__input"
          type="text"
          autocomplete="off"
          spellcheck="false"
          :aria-invalid="showNameProblem"
          :aria-describedby="showNameProblem ? 'create-namespace-name-error' : undefined"
          data-testid="create-namespace-name"
          @blur="touched = true"
        />
        <p class="create__hint">Lowercase, dot-separated segments. {{ NAMESPACE_NAME_EXAMPLE }}</p>
        <p
          v-if="showNameProblem"
          id="create-namespace-name-error"
          class="create__error"
          role="alert"
          data-testid="create-namespace-name-error"
        >
          {{ nameProblem }}
        </p>

        <fieldset class="create__fieldset">
          <legend class="create__label">Audience</legend>
          <label class="create__radio">
            <input
              v-model="audience"
              type="radio"
              value="client"
              data-testid="create-namespace-audience-client"
            />
            <span>Client</span>
          </label>
          <p class="create__hint">Read by the game client and shipped to players.</p>
          <label class="create__radio">
            <input
              v-model="audience"
              type="radio"
              value="server"
              data-testid="create-namespace-audience-server"
            />
            <span>Server</span>
          </label>
          <p class="create__hint">Read by the gameplay backend and never shipped to clients.</p>
          <p class="create__note">Audience cannot be changed later.</p>
        </fieldset>

        <label class="create__label" for="create-namespace-description">Description</label>
        <textarea
          id="create-namespace-description"
          v-model="description"
          class="create__input create__textarea"
          rows="3"
          data-testid="create-namespace-description"
        />

        <p
          v-if="formError !== null"
          class="create__error"
          role="alert"
          data-testid="create-namespace-error"
        >
          {{ formError }}
        </p>

        <div class="create__actions">
          <IonButton fill="clear" color="medium" type="button" @click="emit('dismiss')">
            Cancel
          </IonButton>
          <IonButton type="submit" :disabled="submitting" data-testid="create-namespace-submit">
            <IonSpinner v-if="submitting" name="dots" />
            <span v-else>Create</span>
          </IonButton>
        </div>
      </form>
    </div>
  </IonModal>
</template>

<style scoped>
.create {
  margin: 0 auto;
  max-width: 32rem;
  padding: var(--ds-space-4);
}

.create__title {
  font-size: var(--ds-font-size-lg);
  margin: 0 0 var(--ds-space-4);
}

.create__form {
  display: flex;
  flex-direction: column;
  gap: var(--ds-space-1);
}

.create__label {
  color: var(--ds-text);
  font-size: var(--ds-font-size-sm);
  font-weight: 600;
}

.create__input {
  background: var(--ds-surface);
  border: 1px solid var(--ds-border);
  border-radius: var(--ds-radius-sm);
  color: var(--ds-text);
  font-family: inherit;
  font-size: var(--ds-font-size-md);
  padding: var(--ds-space-2);
  width: 100%;
}

.create__input:focus-visible {
  border-color: var(--ds-accent);
  outline: 2px solid var(--ds-accent);
  outline-offset: 1px;
}

.create__textarea {
  resize: vertical;
}

.create__fieldset {
  border: 0;
  margin: var(--ds-space-3) 0 0;
  padding: 0;
}

.create__radio {
  align-items: center;
  display: flex;
  gap: var(--ds-space-2);
  margin-top: var(--ds-space-2);
}

.create__hint,
.create__note {
  color: var(--ds-text-muted);
  font-size: var(--ds-font-size-sm);
  margin: var(--ds-space-1) 0 0;
}

.create__error {
  color: var(--ds-danger);
  font-size: var(--ds-font-size-sm);
  margin: var(--ds-space-1) 0 0;
}

.create__actions {
  display: flex;
  gap: var(--ds-space-2);
  justify-content: flex-end;
  margin-top: var(--ds-space-4);
}
</style>
