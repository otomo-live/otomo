<script setup lang="ts">
import { HighlightStyle, syntaxHighlighting } from '@codemirror/language'
import { json } from '@codemirror/lang-json'
import { Compartment, Prec } from '@codemirror/state'
import { basicSetup, EditorView } from 'codemirror'
import { tags } from '@lezer/highlight'
import { onBeforeUnmount, onMounted, ref, watch } from 'vue'

/**
 * The raw-JSON escape hatch.
 *
 * The form is generated from a schema, so it can only offer controls for the
 * subset the form model understands: nested objects, arrays of objects and
 * anything a schema invents are not laid out. This is what covers them.
 *
 * It edits TEXT and never a parsed value. Parsing belongs to whoever owns the
 * document, because a component that parsed its own input would have to decide
 * what an unparsable buffer means, and the answer depends on the caller: the
 * editor keeps the text and refuses to switch tabs, a sub-editor for one field
 * keeps the text and marks the field bad. So this emits the string it has and
 * holds no opinion about whether it is JSON.
 *
 * CodeMirror rather than a textarea: a Config document is a few hundred lines of
 * nested braces, and bracket matching, indentation and a line-number gutter are
 * the difference between editing it and guessing at it. `basicSetup` pulls in
 * the standard set; what it deliberately does NOT include is a Tab binding. That
 * omission is CodeMirror's, and it is right: binding Tab to indent traps a
 * keyboard user inside the editor with no way out, which is why the Escape
 * hatch into `indentWithTab` is documented as needing thought before use.
 */

const props = withDefaults(
  defineProps<{
    modelValue: string
    readonly?: boolean
    ariaLabel?: string
  }>(),
  { readonly: false, ariaLabel: 'JSON document' },
)

const emit = defineEmits<{ 'update:modelValue': [value: string] }>()

const host = ref<HTMLDivElement | null>(null)
let view: EditorView | null = null

/**
 * The editable/read-only toggle lives in a compartment because the schema page
 * flips it without remounting the editor: the admin presses Edit and the same
 * CodeMirror instance has to start accepting input.
 */
const editable = new Compartment()

/**
 * CodeMirror's syntax palette, expressed with the design system's roles.
 *
 * The default highlight style is a light palette baked into the editor, which
 * would be wrong under the dark theme. This one reads the same `--ds-*` tokens
 * every other surface does, so a theme change moves the editor with it.
 */
const dsHighlight = HighlightStyle.define([
  { tag: tags.propertyName, color: 'var(--ds-info)' },
  { tag: [tags.string, tags.special(tags.string)], color: 'var(--ds-ok)' },
  { tag: [tags.number, tags.bool, tags.null], color: 'var(--ds-warn)' },
  { tag: tags.keyword, color: 'var(--ds-accent)' },
  { tag: tags.comment, color: 'var(--ds-text-muted)', fontStyle: 'italic' },
  { tag: [tags.punctuation, tags.bracket], color: 'var(--ds-text-muted)' },
])

onMounted(() => {
  if (host.value === null) return

  view = new EditorView({
    parent: host.value,
    doc: props.modelValue,
    extensions: [
      basicSetup,
      json(),
      EditorView.lineWrapping,
      Prec.highest(syntaxHighlighting(dsHighlight)),
      editable.of(EditorView.editable.of(!props.readonly)),
      // The accessible name goes on CodeMirror's own content element, which is
      // the one carrying role="textbox". Naming a wrapper instead would leave a
      // textbox inside a textbox, which is worse for a screen reader than an
      // unnamed one.
      EditorView.contentAttributes.of({ 'aria-label': props.ariaLabel }),
      EditorView.updateListener.of((update) => {
        if (update.docChanged) emit('update:modelValue', update.state.doc.toString())
      }),
      // Sizing in the theme rather than in CSS: the scroller is a div
      // CodeMirror builds inside the element this component owns, so a rule from
      // outside would have to reach through .cm-editor to land.
      EditorView.theme({
        '&': {
          backgroundColor: 'var(--ds-surface)',
          color: 'var(--ds-text)',
          fontSize: '0.85rem',
          height: '22rem',
        },
        '.cm-scroller': { fontFamily: 'var(--ds-font-mono)' },
        '.cm-gutters': {
          backgroundColor: 'var(--ds-surface-raised)',
          borderRight: '1px solid var(--ds-border)',
          color: 'var(--ds-text-muted)',
        },
        '.cm-activeLine': { backgroundColor: 'var(--ds-neutral-soft)' },
        '.cm-activeLineGutter': { backgroundColor: 'var(--ds-neutral-soft)' },
        '.cm-cursor, .cm-dropCursor': { borderLeftColor: 'var(--ds-text)' },
        '&.cm-focused .cm-selectionBackground, .cm-selectionBackground': {
          backgroundColor: 'var(--ds-info-soft)',
        },
      }),
    ],
  })
})

/**
 * Follows the model, which the parent can change without this component being
 * remounted: opening a different namespace, reloading after a conflict.
 *
 * The guard is not an optimisation. Replacing the document on every keystroke's
 * round trip would put the cursor back at the start of the file after every
 * character, and echoing a change this editor just made is exactly that.
 */
watch(
  () => props.modelValue,
  (value) => {
    if (view === null) return
    const current = view.state.doc.toString()
    if (current === value) return
    view.dispatch({
      changes: { from: 0, to: current.length, insert: value },
      // The selection is not restored: whatever was selected referred to the old
      // text, and keeping the same offsets would land it somewhere arbitrary.
      selection: { anchor: 0 },
    })
  },
)

watch(
  () => props.readonly,
  (readonly) => {
    view?.dispatch({ effects: editable.reconfigure(EditorView.editable.of(!readonly)) })
  },
)

onBeforeUnmount(() => {
  view?.destroy()
  view = null
})
</script>

<template>
  <div ref="host" class="raw-editor" />
</template>

<style scoped>
/* The editor chrome follows the design system; the syntax palette is set in the
   CodeMirror theme, because that styling is generated inside the component's own
   scroller and a rule from out here could not reach it. */
.raw-editor {
  background: var(--ds-surface);
  border: 1px solid var(--ds-border);
  border-radius: var(--ds-radius-sm);
  color: var(--ds-text);
  overflow: hidden;
}

.raw-editor:focus-within {
  border-color: var(--ds-accent);
}
</style>
