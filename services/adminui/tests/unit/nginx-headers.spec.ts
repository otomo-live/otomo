import { readFileSync } from 'node:fs'
import { join } from 'node:path'

import { describe, expect, it } from 'vitest'

/**
 * The security headers are configuration, not code, so the only way to keep them
 * from regressing is to assert on the files themselves. The two failure modes
 * this guards are silent in a browser: a weakened directive, and nginx's
 * add_header inheritance rule, where a location that declares any add_header of
 * its own drops every header the server block set.
 */

const ROOT = join(__dirname, '..', '..')
const nginxConf = readFileSync(join(ROOT, 'nginx.conf'), 'utf8')
const securityHeaders = readFileSync(join(ROOT, 'security-headers.conf'), 'utf8')
const dockerfile = readFileSync(join(ROOT, 'dockerfile'), 'utf8')

const INCLUDE = 'include /etc/nginx/adminui-security-headers.conf;'

/** Every block whose opening brace follows `pattern`, with its body. */
function blocks(source: string, pattern: RegExp): string[] {
  const out: string[] = []
  const re = new RegExp(pattern.source, pattern.flags.replace('g', '') + 'g')
  let match: RegExpExecArray | null
  while ((match = re.exec(source)) !== null) {
    const open = source.indexOf('{', match.index)
    if (open === -1) break
    let depth = 0
    let end = open
    for (; end < source.length; end++) {
      if (source[end] === '{') depth += 1
      else if (source[end] === '}') {
        depth -= 1
        if (depth === 0) break
      }
    }
    out.push(source.slice(open + 1, end))
    re.lastIndex = end + 1
  }
  return out
}

function headerLine(name: string): RegExp {
  return new RegExp(`add_header\\s+${name}\\s+"([^"]*)"\\s+always;`)
}

describe('security-headers.conf', () => {
  it('sends a CSP that keeps default-src and script-src at self', () => {
    const policy = headerLine('Content-Security-Policy').exec(securityHeaders)?.[1]
    expect(policy).toBeDefined()
    const directives = (policy ?? '').split(';').map((directive) => directive.trim())
    expect(directives).toContain("default-src 'self'")
    expect(directives).toContain("script-src 'self'")
    expect(directives).toContain("object-src 'none'")
    expect(directives).toContain("frame-ancestors 'none'")
  })

  it('never lets a script run from eval or an inline tag', () => {
    const policy = headerLine('Content-Security-Policy').exec(securityHeaders)?.[1] ?? ''
    const scriptSrc = policy
      .split(';')
      .map((directive) => directive.trim())
      .find((directive) => directive.startsWith('script-src'))
    expect(scriptSrc).toBe("script-src 'self'")
    expect(policy).not.toContain("'unsafe-eval'")
  })

  it('allows inline styles because the UI libraries inject them', () => {
    const policy = headerLine('Content-Security-Policy').exec(securityHeaders)?.[1] ?? ''
    expect(policy).toContain("style-src 'self' 'unsafe-inline'")
  })

  it('sends nosniff, a referrer policy and DENY framing, each with always', () => {
    expect(securityHeaders).toMatch(headerLine('X-Content-Type-Options'))
    expect(securityHeaders).toMatch(headerLine('Referrer-Policy'))
    expect(securityHeaders).toMatch(/add_header\s+X-Frame-Options\s+"DENY"\s+always;/)
  })
})

describe('nginx.conf', () => {
  it('includes the header snippet in the server block', () => {
    const servers = blocks(nginxConf, /\bserver\s*\{/)
    expect(servers).toHaveLength(1)
    expect(servers[0]).toContain(INCLUDE)
  })

  it('re-includes the snippet in every location that sets its own header', () => {
    const locations = blocks(nginxConf, /\blocation\b[^{]*\{/)
    const withHeaders = locations.filter((body) => /\badd_header\b/.test(body))
    // If the cache-control locations ever stop setting headers, this test would
    // pass vacuously; the count keeps it honest.
    expect(withHeaders.length).toBeGreaterThan(0)
    for (const body of withHeaders) expect(body).toContain(INCLUDE)
  })
})

describe('dockerfile', () => {
  it('copies the snippet to the path nginx.conf includes', () => {
    expect(dockerfile).toContain(
      'COPY security-headers.conf /etc/nginx/adminui-security-headers.conf',
    )
  })
})
