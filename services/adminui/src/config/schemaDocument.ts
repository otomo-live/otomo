/**
 * Reading the schema editor's text.
 *
 * The schema page edits a schema document, and unlike the draft editor it has no
 * form: the text IS the document. The only decision it makes is whether the text
 * is JSON yet, and where the mistake is when it is not. The line number is
 * derived from JSON.parse's own `position` because every engine this runs in
 * reports one, and a syntax error without a line is a search through a hundred
 * lines of braces.
 */

export type SchemaParseOutcome =
  { kind: 'document'; document: unknown } | { kind: 'error'; message: string; line: number | null }

/** A 1-based line number for a byte position, or null when it cannot be found. */
export function lineAt(text: string, position: number): number | null {
  if (!Number.isInteger(position) || position < 0) return null
  let line = 1
  const end = Math.min(position, text.length)
  for (let index = 0; index < end; index += 1) {
    if (text[index] === '\n') line += 1
  }
  return line
}

export function parseSchemaText(text: string): SchemaParseOutcome {
  try {
    return { kind: 'document', document: JSON.parse(text) as unknown }
  } catch (error) {
    const message = error instanceof Error ? error.message : 'The schema is not valid JSON.'
    // V8 reports a byte position for most syntax errors and a resolved
    // `(line N column M)` for some; the position is preferred because it is
    // exact, and the resolved line is the fallback when only that is present.
    const position = /position (\d+)/.exec(message)
    const resolved = /line (\d+)/.exec(message)
    const line =
      position !== null
        ? lineAt(text, Number(position[1]))
        : resolved !== null
          ? Number(resolved[1])
          : null
    return { kind: 'error', message, line }
  }
}
