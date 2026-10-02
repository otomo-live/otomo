import { readdirSync, readFileSync } from 'node:fs'
import { join, relative } from 'node:path'

import { describe, expect, it } from 'vitest'

/**
 * Colour lives in one file, and only one file.
 *
 * A component that names a colour is a component that cannot follow a theme and
 * a component that duplicates a value the design system already owns. This scans
 * every `.vue`, `.css` and `.ts` under `src/` (the JSON fixtures and the mock
 * service worker are not source we paint with) and fails on a literal outside
 * `tokens.css`.
 *
 * The second check is the other half of the same contract: a `var(--ds-…)` that
 * is not defined is a silently empty value.
 */

const SRC = join(__dirname, '..', '..', 'src')
const TOKENS = join(SRC, 'theme', 'tokens.css')

function sourceFiles(dir: string): string[] {
  const found: string[] = []
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const path = join(dir, entry.name)
    if (entry.isDirectory()) found.push(...sourceFiles(path))
    else if (/\.(vue|css|ts)$/.test(entry.name)) found.push(path)
  }
  return found
}

/**
 * Named colours are only a literal when they are a property's value. `white-space`
 * and a comment about "green ticks" are not colours, and a regex that did not
 * know the difference would fail on both.
 */
const COLOUR_PROPERTY =
  /(?:^|[\s;{])(?:color|background|background-color|border|border-color|border-top|border-right|border-bottom|border-left|outline|outline-color|fill|stroke|box-shadow|text-shadow)\s*:\s*[^;{}]*\b(white|black|red|blue|green|gray|grey|silver|maroon|olive|lime|aqua|teal|navy|fuchsia|purple|orange|yellow)\b/i

describe('design tokens', () => {
  const files = sourceFiles(SRC)

  it('has source files to scan', () => {
    expect(files.length).toBeGreaterThan(0)
  })

  it('has no colour literals outside tokens.css', () => {
    const offenders: string[] = []
    for (const file of files) {
      if (file === TOKENS) continue
      const text = readFileSync(file, 'utf8')
      const lines = text.split('\n')
      lines.forEach((line, index) => {
        const functional = /\brgba?\(|\bhsla?\(|#[0-9a-fA-F]{3,8}\b/.test(line)
        const named = COLOUR_PROPERTY.test(line)
        if (functional || named) {
          offenders.push(`${relative(SRC, file)}:${index + 1}: ${line.trim()}`)
        }
      })
    }
    expect(offenders).toEqual([])
  })

  it('defines every --ds-* token that is used', () => {
    const defined = new Set<string>()
    const definitionPattern = /^\s*(--ds-[a-z0-9-]+)\s*:/gm
    const tokenText = readFileSync(TOKENS, 'utf8')
    for (const match of tokenText.matchAll(definitionPattern)) {
      defined.add(match[1])
    }

    const used = new Set<string>()
    const usagePattern = /var\(\s*(--ds-[a-z0-9-]+)\s*\)/g
    for (const file of files) {
      const text = readFileSync(file, 'utf8')
      for (const match of text.matchAll(usagePattern)) {
        used.add(match[1])
      }
    }

    const missing = [...used].filter((token) => !defined.has(token)).sort()
    expect(missing).toEqual([])
  })
})
