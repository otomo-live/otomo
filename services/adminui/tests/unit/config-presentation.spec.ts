import { describe, expect, it } from 'vitest'

import balanceWeapons from '@/mocks/fixtures/namespace-balance-weapons.json'
import uiPresentation from '@/mocks/fixtures/namespace-ui-presentation.json'
import {
  buildForm,
  parseEnvelope,
  PRESENTATION_NAMESPACE,
  type FormModel,
  type PresentationEnvelope,
} from '@/config/presentation'

/**
 * The envelope is what lets a project say how its own namespaces should look
 * without this app knowing anything about them, so the property under test is
 * DEGRADATION: whatever the envelope says, a field the schema defines stays
 * reachable. Half of these cases are therefore envelopes that are wrong in some
 * way and have to be survived rather than rejected.
 */

function envelopeOf(value: PresentationEnvelope | null): PresentationEnvelope {
  if (value === null) throw new Error('expected an envelope')
  return value
}

function fieldNames(model: FormModel): string[][] {
  return model.sections.map((section) => section.fields.map((field) => field.name))
}

/** A schema with three properties, for the cases the two fixtures cannot express. */
const synthetic = {
  type: 'object',
  title: 'Synthetic',
  required: ['a'],
  properties: {
    a: { type: 'string', title: 'A' },
    b: { type: 'integer' },
    c: { type: 'boolean' },
  },
}

describe('parseEnvelope', () => {
  it('names the reserved namespace, which is a namespace and not a file', () => {
    expect(PRESENTATION_NAMESPACE).toBe('ui.presentation')
  })

  it('reads the entry for the namespace it was asked about', () => {
    const envelope = envelopeOf(parseEnvelope(uiPresentation.draft.document, 'balance.weapons'))
    expect(envelope.title).toBe('Weapons')
    expect(envelope.order).toContain('rarity')
    expect(envelope.groups).toEqual([{ label: 'Impact', fields: ['damage'] }])
  })

  it('does not model the render hint, which is a hint the shell does not act on', () => {
    // The fixture carries `render` on purpose. A field modelled only to be
    // ignored reads as a feature that is not there.
    const envelope = envelopeOf(parseEnvelope(uiPresentation.draft.document, 'balance.weapons'))
    expect(envelope).not.toHaveProperty('render')
    expect(uiPresentation.draft.document['balance.weapons']).toHaveProperty('render')
  })

  it('has nothing to say about a namespace the document does not mention', () => {
    expect(parseEnvelope(uiPresentation.draft.document, 'balance.armour')).toBeNull()
  })

  it('degrades to no envelope for a document that is not one', () => {
    for (const document of [null, undefined, 'text', 5, [], true]) {
      expect(parseEnvelope(document, 'balance.weapons')).toBeNull()
    }
  })

  it('degrades to no envelope for an entry that is not an object', () => {
    expect(parseEnvelope({ 'balance.weapons': 'list' }, 'balance.weapons')).toBeNull()
    expect(parseEnvelope({ 'balance.weapons': [] }, 'balance.weapons')).toBeNull()
  })

  it('degrades to no envelope for an entry that says nothing usable', () => {
    // An empty title is not a title: it would replace the schema's own, which is
    // the more useful of the two. An empty order and a group with no fields say
    // nothing either.
    expect(parseEnvelope({ ns: { title: '' } }, 'ns')).toBeNull()
    expect(parseEnvelope({ ns: { order: [] } }, 'ns')).toBeNull()
    expect(parseEnvelope({ ns: { order: [1, 2] } }, 'ns')).toBeNull()
    expect(parseEnvelope({ ns: { groups: [{ label: 'x' }] } }, 'ns')).toBeNull()
    expect(parseEnvelope({ ns: { render: { style: 'list' } } }, 'ns')).toBeNull()
  })

  it('drops a group with no usable field list rather than drawing an empty box', () => {
    const envelope = envelopeOf(
      parseEnvelope(
        {
          ns: {
            groups: [
              { label: 'No fields' },
              { label: 'Not strings', fields: [1, 2] },
              { label: 'Real', fields: ['a'] },
            ],
          },
        },
        'ns',
      ),
    )
    expect(envelope.groups).toEqual([{ label: 'Real', fields: ['a'] }])
  })

  it('keeps the parts of an entry that are usable', () => {
    const envelope = envelopeOf(parseEnvelope({ ns: { title: 'T', order: ['a'] } }, 'ns'))
    expect(envelope).toEqual({ title: 'T', order: ['a'] })
  })
})

describe('buildForm without an envelope', () => {
  const model = buildForm(balanceWeapons.schema.body, null)

  it('renders every property in the schema, in the schema order', () => {
    expect(fieldNames(model)).toEqual([['name', 'damage', 'range', 'ammo']])
    expect(model.sections[0].label).toBeNull()
  })

  it('labels a field with the schema title and falls back to the property name', () => {
    const labels = model.sections[0].fields.map((field) => field.label)
    expect(labels).toEqual(['Name', 'Damage', 'Range (m)', 'Ammo'])
    expect(buildForm(synthetic, null).sections[0].fields[1].label).toBe('b')
  })

  it('marks the fields the schema requires', () => {
    const required = model.sections[0].fields
      .filter((field) => field.required)
      .map((field) => field.name)
    expect(required).toEqual(['name', 'damage', 'range'])
  })

  it('carries the schema of each property, which is what its control is chosen from', () => {
    const damage = model.sections[0].fields.find((field) => field.name === 'damage')
    expect(damage?.schema).toEqual({ type: 'integer', title: 'Damage', minimum: 0, maximum: 500 })
  })

  it('takes the title from the schema', () => {
    expect(model.title).toBe('Weapon')
    expect(buildForm({ type: 'object' }, null).title).toBeNull()
  })

  it('says the form did not come from an envelope', () => {
    expect(model.fromEnvelope).toBe(false)
  })

  it('renders nothing, rather than failing, for a schema with no properties', () => {
    expect(buildForm({ type: 'object' }, null).sections).toEqual([])
    expect(buildForm(null, null).sections).toEqual([])
  })
})

describe('buildForm with an envelope', () => {
  const envelope = envelopeOf(parseEnvelope(uiPresentation.draft.document, 'balance.weapons'))
  const model = buildForm(balanceWeapons.schema.body, envelope)

  it('takes the title from the envelope rather than from the schema', () => {
    expect(model.title).toBe('Weapons')
  })

  it('says the form came from an envelope, including a title-only one', () => {
    expect(model.fromEnvelope).toBe(true)
    expect(buildForm(synthetic, { title: 'T' }).fromEnvelope).toBe(true)
  })

  it('drops a group the schema does not back at all', () => {
    const ghost = buildForm(synthetic, { groups: [{ label: 'Ghost', fields: ['nope'] }] })
    expect(fieldNames(ghost)).toEqual([['a', 'b', 'c']])
    expect(ghost.sections[0].label).toBeNull()
  })

  it('ignores a name the schema does not define, in a group as well as in order', () => {
    const withGhosts = buildForm(synthetic, {
      order: ['ghost', 'b'],
      groups: [{ label: 'G', fields: ['ghost', 'a'] }],
    })
    expect(fieldNames(withGhosts)).toEqual([['a'], ['b', 'c']])
  })

  it('appends everything no section claimed to one trailing section, in schema order', () => {
    // The fixture envelope names four fields in `order` and groups one of them,
    // so what is left is appended rather than dropped: this is the property that
    // means a game can add a column without rewriting a presentation document.
    expect(fieldNames(model)).toEqual([['damage'], ['name', 'range', 'ammo']])
    expect(model.sections[0].label).toBe('Impact')
    expect(model.sections[1].label).toBeNull()
  })

  it('lets groups decide, so order does not survive alongside them', () => {
    // A group is the stronger statement: it says which fields belong together,
    // and order only says which comes first. Where both exist the grouping wins,
    // and every field lands in schema order within its section.
    const both = buildForm(synthetic, {
      order: ['c', 'b'],
      groups: [{ label: 'G', fields: ['a'] }],
    })
    expect(fieldNames(both)).toEqual([['a'], ['b', 'c']])
  })

  it('orders the fields an envelope names, when it only names an order', () => {
    const ordered = buildForm(synthetic, { order: ['c', 'a'] })
    expect(fieldNames(ordered)).toEqual([['c', 'a'], ['b']])
  })

  it('never loses a field, whatever the envelope says', () => {
    // The safety property, stated once as a property rather than as cases.
    const envelopes: PresentationEnvelope[] = [
      {},
      { title: 'T' },
      { order: [] },
      { order: ['a', 'nope'] },
      { groups: [] },
      { groups: [{ label: 'G', fields: ['nope'] }] },
      { order: ['c'], groups: [{ label: 'G', fields: ['a'] }] },
    ]
    for (const candidate of envelopes) {
      const names = fieldNames(buildForm(synthetic, candidate)).flat()
      expect([...names].sort()).toEqual(['a', 'b', 'c'])
    }
  })
})
