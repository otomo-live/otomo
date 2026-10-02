<script setup lang="ts">
import {
  IonButton,
  IonInput,
  IonItem,
  IonNote,
  IonSelect,
  IonSelectOption,
  IonText,
  IonToggle,
} from '@ionic/vue'
import { computed, ref, watch } from 'vue'

import type { ValidationIssue } from '@/api/config'
import type { FormField } from '@/config/presentation'
import {
  controlFor,
  enumOptions,
  memberForToken,
  numberFrom,
  textFor,
  tokenForValue,
} from '@/config/controls'
import { messagesFor } from '@/config/document'
import RawJsonEditor from '@/config/RawJsonEditor.vue'

/**
 * One field of a schema-driven form.
 *
 * The control comes from the schema (`controlFor`) and nothing about it is
 * specific to a game: the whole point of the Config design is that a namespace is
 * arbitrary JSON plus its own schema, so a form generated from it is what makes
 * an editor possible without the SPA knowing any project's tables.
 *
 * This component is also its own recursion. An array of scalars is a repeater of
 * one control per item, and that control is the same decision applied to the
 * item's schema, so it renders one of itself per item. The recursion terminates
 * because `controlFor` never answers `scalars` for a scalar item schema: an array
 * of arrays is `json`.
 *
 * It emits the VALUE, never a serialised form of it, and it emits `undefined` to
 * say "this field should not be in the document". The owner of the document
 * decides what that means, which is what keeps absent and null distinct all the
 * way to the saved JSON.
 */

const props = withDefaults(
  defineProps<{
    field: FormField
    /** The JSON Pointer of this field, for matching validation issues to it. */
    pointer: string
    value: unknown
    issues?: ValidationIssue[]
    disabled?: boolean
  }>(),
  { issues: () => [], disabled: false },
)

const emit = defineEmits<{ 'update:value': [value: unknown] }>()

const kind = computed(() => controlFor(props.field.schema))
const messages = computed(() => messagesFor(props.issues, props.pointer))

const options = computed(() => enumOptions(props.field.schema))

/**
 * What a number field is showing.
 *
 * A local buffer, because a half-typed number is not a value: `-` and `1.` are
 * on the way to a number and are not one yet. Committing only complete numbers
 * means the document never holds something the schema's `type` rejects, and the
 * buffer is what keeps the character a person just typed on screen.
 */
const draft = ref<string | null>(null)

watch(
  () => props.value,
  () => {
    // The document changed under us (a reload, a conflict, a revert), so
    // whatever was half-typed is no longer describing this field.
    draft.value = null
  },
)

const numberText = computed(() => draft.value ?? textFor(props.value))

function onNumber(text: string | number | null | undefined): void {
  const raw = String(text ?? '')
  const outcome = numberFrom(raw, kind.value === 'integer')
  if (outcome.kind === 'value') {
    draft.value = null
    emit('update:value', outcome.value)
    return
  }
  if (outcome.kind === 'empty') {
    draft.value = null
    emit('update:value', undefined)
    return
  }
  draft.value = raw
}

function onText(text: string | number | null | undefined): void {
  const raw = String(text ?? '')
  emit('update:value', raw === '' ? undefined : raw)
}

function onDate(text: string | number | null | undefined): void {
  const raw = String(text ?? '')
  emit('update:value', raw === '' ? undefined : raw)
}

function onEnum(token: string | number | null | undefined): void {
  if (token === '' || token === null || token === undefined) {
    emit('update:value', undefined)
    return
  }
  const member = memberForToken(String(token))
  emit('update:value', member === undefined ? undefined : member)
}

function onToggle(checked: unknown): void {
  emit('update:value', checked === true)
}

// ---------------------------------------------------------------------------
// Arrays of scalars
// ---------------------------------------------------------------------------

const items = computed<unknown[]>(() => (Array.isArray(props.value) ? props.value : []))

/**
 * The item's schema as a field of its own, so the repeater renders the same
 * component rather than a second implementation of "a string input".
 */
const itemField = computed<FormField>(() => {
  const schema = props.field.schema
  const items = schema.items
  return {
    name: props.field.name,
    label: props.field.label,
    required: false,
    schema: typeof items === 'object' && items !== null ? (items as Record<string, unknown>) : {},
  }
})

function setItem(index: number, value: unknown): void {
  const next = [...items.value]
  // An item emptied to nothing is removed rather than left as a hole: an array
  // with an undefined in the middle cannot survive JSON.stringify, and a
  // placeholder `null` would be a value the person did not type.
  if (value === undefined) next.splice(index, 1)
  else next[index] = value
  emit('update:value', next)
}

function addItem(): void {
  emit('update:value', [...items.value, null])
}

function removeItem(index: number): void {
  const next = [...items.value]
  next.splice(index, 1)
  emit('update:value', next)
}

// ---------------------------------------------------------------------------
// The raw sub-editor, for what the form cannot lay out
// ---------------------------------------------------------------------------

const rawText = ref('')
const rawError = ref<string | null>(null)

watch(
  () => props.value,
  () => {
    rawText.value = JSON.stringify(props.value ?? null, null, 2)
    rawError.value = null
  },
  { immediate: true },
)

function onRaw(text: string): void {
  rawText.value = text
  try {
    emit('update:value', JSON.parse(text))
    rawError.value = null
  } catch {
    // The editor keeps what was typed and says so. Refusing to emit leaves the
    // last valid value in the document, which is the honest state while the text
    // is unparsable.
    rawError.value = 'Not valid JSON yet. The last valid value is still in the document.'
  }
}
</script>

<template>
  <div class="field">
    <!-- The label is the control's own prop wherever the control has one, so the
         text is associated with the field rather than merely drawn next to it. -->
    <IonItem v-if="kind === 'string'">
      <IonInput
        :model-value="textFor(value)"
        :label="field.label"
        label-placement="stacked"
        :required="field.required"
        :disabled="disabled"
        @update:model-value="onText"
      />
    </IonItem>

    <IonItem v-else-if="kind === 'date'">
      <IonInput
        :model-value="textFor(value)"
        :label="field.label"
        label-placement="stacked"
        type="date"
        :required="field.required"
        :disabled="disabled"
        @update:model-value="onDate"
      />
    </IonItem>

    <IonItem v-else-if="kind === 'integer' || kind === 'number'">
      <IonInput
        :model-value="numberText"
        :label="field.label"
        label-placement="stacked"
        type="number"
        inputmode="decimal"
        :step="kind === 'integer' ? '1' : 'any'"
        :required="field.required"
        :disabled="disabled"
        @update:model-value="onNumber"
        @ion-blur="draft = null"
      />
    </IonItem>

    <IonItem v-else-if="kind === 'boolean'">
      <IonToggle
        :model-value="value === true"
        :disabled="disabled"
        justify="space-between"
        @update:model-value="onToggle"
      >
        {{ field.label }}
      </IonToggle>
    </IonItem>

    <IonItem v-else-if="kind === 'enum'">
      <IonSelect
        :model-value="tokenForValue(value)"
        :label="field.label"
        label-placement="stacked"
        placeholder="Not set"
        :disabled="disabled"
        @update:model-value="onEnum"
      >
        <IonSelectOption v-for="option in options" :key="option.token" :value="option.token">
          {{ option.label }}
        </IonSelectOption>
      </IonSelect>
    </IonItem>

    <div v-else-if="kind === 'scalars'" class="field__group">
      <p class="field__label">{{ field.label }}</p>
      <div v-for="(item, index) in items" :key="index" class="field__repeater-row">
        <SchemaField
          :field="itemField"
          :pointer="`${pointer}/${index}`"
          :value="item"
          :issues="issues"
          :disabled="disabled"
          @update:value="(next) => setItem(index, next)"
        />
        <IonButton
          fill="clear"
          color="danger"
          size="small"
          :aria-label="`Remove ${field.label} item ${index + 1}`"
          @click="removeItem(index)"
        >
          Remove
        </IonButton>
      </div>
      <IonButton size="small" fill="outline" :disabled="disabled" @click="addItem">
        Add {{ field.label }}
      </IonButton>
    </div>

    <div v-else class="field__group">
      <p class="field__label">{{ field.label }}</p>
      <IonNote class="field__hint">
        This field's shape is not one the form lays out, so it is edited as JSON.
      </IonNote>
      <RawJsonEditor
        :model-value="rawText"
        :readonly="disabled"
        :aria-label="`${field.label} as JSON`"
        @update:model-value="onRaw"
      />
      <IonText v-if="rawError !== null" color="danger">
        <p class="field__message">{{ rawError }}</p>
      </IonText>
    </div>

    <!-- The property name, when the label is not it. A reader who has to write a
         query or read a diff needs the key, and a title is not it. -->
    <IonNote v-if="field.label !== field.name" class="field__key">{{ field.name }}</IonNote>

    <IonText v-if="messages.length > 0" color="danger" role="alert">
      <p v-for="message in messages" :key="message" class="field__message">
        {{ field.label }} {{ message }}
      </p>
    </IonText>
  </div>
</template>

<style scoped>
.field {
  margin-bottom: 0.5rem;
}

.field__group {
  margin: 0.75rem 0;
}

.field__label {
  font-weight: 600;
  margin: 0 0 0.25rem;
}

.field__hint,
.field__key {
  display: block;
  font-size: 0.8rem;
  margin-bottom: 0.25rem;
}

.field__key {
  font-family: var(--app-font-mono);
}

.field__message {
  font-size: 0.85rem;
  margin: 0.25rem 0 0;
}

.field__repeater-row {
  align-items: center;
  display: flex;
  gap: 0.5rem;
}

.field__repeater-row .field {
  flex: 1;
}
</style>
