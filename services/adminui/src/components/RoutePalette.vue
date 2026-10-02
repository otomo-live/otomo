<script setup lang="ts">
import { IonModal } from '@ionic/vue'
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRouter } from 'vue-router'

import { routes } from '@/router'
import { paletteEntries, type PaletteEntry } from '@/router/nav'
import { useSessionStore } from '@/stores/session'

/**
 * The ⌘K / Ctrl+K route palette.
 *
 * The list is the same role-filtered navigation the sidebar shows, from the same
 * route table, so the two cannot offer different pages. It is mounted by the
 * shell, which owns `open`, and carries its own window listener so the combo
 * works from anywhere in the app, including while a text field has focus.
 */

const props = defineProps<{ open: boolean }>()
const emit = defineEmits<{ 'update:open': [value: boolean] }>()

const store = useSessionStore()
const router = useRouter()

const query = ref('')
const activeIndex = ref(0)
const input = ref<HTMLInputElement | null>(null)

const items = computed<PaletteEntry[]>(() => paletteEntries(routes, store.roles))

const matches = computed(() => {
  const needle = query.value.trim().toLowerCase()
  if (needle === '') return items.value
  return items.value.filter(
    (item) =>
      item.label.toLowerCase().includes(needle) || item.groupLabel.toLowerCase().includes(needle),
  )
})

const activeId = computed(() =>
  matches.value.length === 0 ? undefined : optionId(activeIndex.value),
)

function optionId(index: number): string {
  return `route-palette-option-${index}`
}

function setOpen(value: boolean): void {
  emit('update:open', value)
}

watch(
  () => props.open,
  async (open) => {
    if (!open) return
    query.value = ''
    activeIndex.value = 0
  },
)

// IonModal presents asynchronously and moves focus to itself when it does, so
// focusing on open (or on nextTick) loses the race; the input takes focus once the
// modal says it is presented.
function focusInput(): void {
  input.value?.focus()
}

// Keeps the highlight in range as the filter narrows.
watch(matches, () => {
  if (activeIndex.value >= matches.value.length) activeIndex.value = 0
})

function move(delta: number): void {
  if (matches.value.length === 0) return
  activeIndex.value = (activeIndex.value + delta + matches.value.length) % matches.value.length
}

function choose(item: PaletteEntry | undefined): void {
  if (item === undefined) return
  setOpen(false)
  void router.push(item.path)
}

function onInputKeydown(event: KeyboardEvent): void {
  if (event.key === 'ArrowDown') {
    event.preventDefault()
    move(1)
  } else if (event.key === 'ArrowUp') {
    event.preventDefault()
    move(-1)
  } else if (event.key === 'Enter') {
    event.preventDefault()
    choose(matches.value[activeIndex.value])
  } else if (event.key === 'Escape') {
    event.preventDefault()
    setOpen(false)
  }
}

/**
 * The combo is honoured wherever focus is; everything else is only acted on
 * while the palette is open, so the listener never steals a keystroke from a
 * form elsewhere on the page.
 */
function onWindowKeydown(event: KeyboardEvent): void {
  if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === 'k') {
    event.preventDefault()
    setOpen(true)
    return
  }
  if (props.open && event.key === 'Escape') setOpen(false)
}

onMounted(() => window.addEventListener('keydown', onWindowKeydown))
onBeforeUnmount(() => window.removeEventListener('keydown', onWindowKeydown))
</script>

<template>
  <IonModal
    class="route-palette-modal"
    :is-open="open"
    @did-present="focusInput"
    @did-dismiss="setOpen(false)"
  >
    <div v-if="open" class="palette" role="dialog" aria-modal="true" aria-label="Search pages">
      <input
        ref="input"
        v-model="query"
        class="palette__input"
        type="text"
        role="combobox"
        aria-label="Search pages"
        :aria-expanded="open"
        aria-controls="route-palette-list"
        :aria-activedescendant="activeId"
        autocomplete="off"
        spellcheck="false"
        @keydown="onInputKeydown"
      />

      <ul id="route-palette-list" class="palette__list" role="listbox" aria-label="Pages">
        <li
          v-for="(item, index) in matches"
          :id="optionId(index)"
          :key="item.path"
          class="palette__option"
          :class="{ 'palette__option--active': index === activeIndex }"
          role="option"
          :aria-selected="index === activeIndex"
          @click="choose(item)"
          @mousemove="activeIndex = index"
        >
          <span class="palette__label">{{ item.label }}</span>
          <span class="palette__group">{{ item.groupLabel }}</span>
        </li>
      </ul>

      <p v-if="matches.length === 0" class="palette__empty">No matching pages.</p>
    </div>
  </IonModal>
</template>

<style scoped>
.palette {
  margin: 0 auto;
  max-width: 32rem;
  padding: var(--ds-space-4);
}

.palette__input {
  background: var(--ds-surface);
  border: 1px solid var(--ds-border);
  border-radius: var(--ds-radius-sm);
  color: var(--ds-text);
  font-family: inherit;
  font-size: var(--ds-font-size-md);
  padding: var(--ds-space-3);
  width: 100%;
}

.palette__input:focus-visible {
  border-color: var(--ds-accent);
  outline: 2px solid var(--ds-accent);
  outline-offset: 1px;
}

.palette__list {
  list-style: none;
  margin: var(--ds-space-2) 0 0;
  max-height: 20rem;
  overflow-y: auto;
  padding: 0;
}

.palette__option {
  align-items: center;
  border-radius: var(--ds-radius-sm);
  cursor: pointer;
  display: flex;
  gap: var(--ds-space-3);
  justify-content: space-between;
  padding: var(--ds-space-2) var(--ds-space-3);
}

.palette__option--active {
  background: var(--ds-neutral-soft);
}

.palette__label {
  color: var(--ds-text);
}

.palette__group {
  color: var(--ds-text-muted);
  font-size: var(--ds-font-size-xs);
  text-transform: uppercase;
}

.palette__empty {
  color: var(--ds-text-muted);
  font-size: var(--ds-font-size-sm);
  margin: var(--ds-space-3) 0 0;
  text-align: center;
}
</style>

<style>
/* Unscoped: ion-modal is teleported out of this component. A command palette sits
 * near the top and is as tall as its results, not a full-size dialog. */
ion-modal.route-palette-modal {
  --width: min(560px, calc(100vw - 2 * var(--ds-space-4)));
  --height: auto;
  --max-height: 70vh;
  align-items: flex-start;
  padding-top: 12vh;
}

ion-modal.route-palette-modal .palette {
  width: 100%;
}
</style>
