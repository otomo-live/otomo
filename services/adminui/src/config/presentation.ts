import { getDraft } from '@/api/config'
import { isRecord } from '@/api/shape'

/**
 * The presentation envelope: how a project says what its Config namespaces should
 * look like, without this app knowing anything about them.
 *
 * The envelope lives in one reserved Config namespace, `ui.presentation`, whose
 * document maps a namespace name to its metadata. It is a Config namespace rather
 * than a file or a table because it then travels the same draft, version, release
 * and audit path as everything else, and because nothing new has to exist for a
 * project to author it. Reading it needs `live_ops` on `/api/admin/config/*`,
 * which is the role that may open the editor at all, so it grants nothing new.
 *
 * The safety property is DEGRADATION, and it is one-way:
 *
 *  - a namespace with no envelope renders in schema property order, with property
 *    names as labels;
 *  - a field the envelope names that the schema does not define is ignored;
 *  - a field the schema defines that the envelope does not name is appended to a
 *    trailing section.
 *
 * So no envelope can make a field unreachable. That is what lets a game add a
 * column without every presentation document having to be rewritten, and it is
 * why the render hints are not part of this type: `render` exists in the contract
 * and is a hint the shell does not act on, and a field modelled only to be
 * ignored reads as a feature that is not there. A view that uses it adds it here.
 */

export const PRESENTATION_NAMESPACE = 'ui.presentation'

export interface PresentationGroup {
  label: string
  fields: string[]
}

export interface PresentationEnvelope {
  title?: string
  order?: string[]
  groups?: PresentationGroup[]
}

function stringList(value: unknown): string[] | undefined {
  if (!Array.isArray(value)) return undefined
  const entries = value.filter((entry): entry is string => typeof entry === 'string')
  return entries.length === 0 ? undefined : entries
}

function parseGroups(value: unknown): PresentationGroup[] | undefined {
  if (!Array.isArray(value)) return undefined
  const groups: PresentationGroup[] = []
  for (const entry of value) {
    if (!isRecord(entry)) continue
    const fields = stringList(entry.fields)
    // A group with no usable field list says nothing. Dropping it is the same
    // degradation as a field the schema does not define: ignore what cannot be
    // honoured rather than rendering an empty box.
    if (fields === undefined) continue
    groups.push({ label: typeof entry.label === 'string' ? entry.label : '', fields })
  }
  return groups.length === 0 ? undefined : groups
}

/**
 * The envelope for one namespace, or null. Null and an empty envelope mean the
 * same thing to a caller, which is the point: a project that has authored
 * nothing, a document that mentions some other namespace, and a document whose
 * entry is malformed all degrade to "render the schema".
 */
export function parseEnvelope(document: unknown, namespace: string): PresentationEnvelope | null {
  if (!isRecord(document)) return null
  const entry = document[namespace]
  if (!isRecord(entry)) return null

  const envelope: PresentationEnvelope = {}
  // An empty title is not a title: it would silently replace the schema's own,
  // which is the more useful of the two.
  if (typeof entry.title === 'string' && entry.title !== '') envelope.title = entry.title
  const order = stringList(entry.order)
  if (order !== undefined) envelope.order = order
  const groups = parseGroups(entry.groups)
  if (groups !== undefined) envelope.groups = groups

  return Object.keys(envelope).length === 0 ? null : envelope
}

/**
 * Reads the envelope for a namespace from Config.
 *
 * Every failure is "no envelope". An unreachable Config, a project that has never
 * authored presentation metadata (the namespace does not exist yet, which is a
 * 404 rather than an empty document), and a refusal all have the same answer for
 * the editor: show the schema-driven form. Surfacing an error here would mean an
 * editor that cannot open because a cosmetic document is missing, which is the
 * wrong way round.
 */
export async function loadEnvelope(namespace: string): Promise<PresentationEnvelope | null> {
  try {
    const draft = await getDraft(PRESENTATION_NAMESPACE)
    return parseEnvelope(draft.document, namespace)
  } catch {
    return null
  }
}

// ---------------------------------------------------------------------------
// The form model
// ---------------------------------------------------------------------------

export interface FormField {
  name: string
  /** The schema's `title`, else the property name. */
  label: string
  required: boolean
  /** The property's own schema. Untyped on purpose: it is data. */
  schema: Record<string, unknown>
}

export interface FormSection {
  /** Null for the trailing section of fields no group claimed. */
  label: string | null
  fields: FormField[]
}

export interface FormModel {
  /** The envelope's title, else the schema's, else null. */
  title: string | null
  sections: FormSection[]
  /**
   * Whether an envelope was applied at all, so a reader can be told which they
   * are seeing. True for a title-only envelope as well as one that grouped or
   * ordered the fields: the question this answers is where the form came from,
   * not how much of it the envelope had an opinion about.
   */
  fromEnvelope: boolean
}

function propertiesOf(schema: unknown): Record<string, unknown> {
  if (!isRecord(schema)) return {}
  const properties = schema.properties
  return isRecord(properties) ? properties : {}
}

function requiredOf(schema: unknown): Set<string> {
  if (!isRecord(schema) || !Array.isArray(schema.required)) return new Set()
  return new Set(schema.required.filter((name): name is string => typeof name === 'string'))
}

function fieldFor(name: string, property: unknown, required: Set<string>): FormField {
  const definition = isRecord(property) ? property : {}
  return {
    name,
    label: typeof definition.title === 'string' ? definition.title : name,
    required: required.has(name),
    schema: definition,
  }
}

/**
 * Applies an envelope to a schema, if there is one.
 *
 * Sections are built first from `groups` when the envelope has them, because a
 * group is the stronger statement: it says which fields belong together, and
 * `order` only says which comes first. Within a section, fields keep the order
 * they were named in. A field is claimed by the first section that names it, and
 * everything the schema defines that no section claimed is appended to a trailing
 * unnamed section, in schema order.
 */
export function buildForm(schema: unknown, envelope: PresentationEnvelope | null): FormModel {
  const properties = propertiesOf(schema)
  const names = Object.keys(properties)
  const required = requiredOf(schema)

  const schemaTitle = isRecord(schema) && typeof schema.title === 'string' ? schema.title : null
  const title = envelope?.title ?? schemaTitle

  // Every field, in schema order, keyed by name. Names the schema does not define
  // are simply not here, which is how an envelope's unknown names are ignored.
  const unclaimed = new Map(names.map((name) => [name, fieldFor(name, properties[name], required)]))
  const sections: FormSection[] = []

  function claim(fieldNames: string[]): FormField[] {
    const claimed: FormField[] = []
    for (const name of fieldNames) {
      const field = unclaimed.get(name)
      if (field === undefined) continue
      unclaimed.delete(name)
      claimed.push(field)
    }
    return claimed
  }

  if (envelope?.groups !== undefined) {
    for (const group of envelope.groups) {
      const fields = claim(group.fields)
      // A group the schema does not back at all is dropped rather than drawn
      // empty. See the header: an envelope can never make a field unreachable,
      // and it should not be able to add an empty box either.
      if (fields.length > 0) sections.push({ label: group.label, fields })
    }
  } else if (envelope?.order !== undefined) {
    const fields = claim(envelope.order)
    if (fields.length > 0) sections.push({ label: null, fields })
  }

  if (unclaimed.size > 0) {
    sections.push({ label: null, fields: [...unclaimed.values()] })
  }

  return { title, sections, fromEnvelope: envelope !== null }
}
