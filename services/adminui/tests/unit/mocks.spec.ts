import { describe, expect, it } from 'vitest'

import balanceWeapons from '@/mocks/fixtures/namespace-balance-weapons.json'
import uiPresentation from '@/mocks/fixtures/namespace-ui-presentation.json'

/**
 * The fixture API is checked through a real `fetch` rather than by calling the
 * handlers directly, because the thing that breaks in practice is not the
 * handler body but the path the SPA addresses, the content type it gets back, or
 * the shape of an error body. `server` comes from the setup file.
 */

interface TokenResponse {
  access_token?: string
  expires_in?: number
  mfa_required?: boolean
  mfa_ticket?: string
  user?: { id: string; name: string; roles: string[] }
}

interface DraftResponse {
  document?: unknown
  revision?: number
  updated_at?: string
}

interface ErrorResponse {
  error: { code: string; message: string; request_id: string }
}

async function postJson(path: string, body: unknown): Promise<Response> {
  return fetch(path, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  })
}

describe('staff auth fixtures', () => {
  it('issues a token with the roles for the email', async () => {
    const response = await postJson('/admin-auth/login', {
      email: 'admin@example.com',
      password: 'correct horse battery staple',
    })

    expect(response.status).toBe(200)
    const body = (await response.json()) as TokenResponse
    expect(body.access_token).toBeTypeOf('string')
    expect(body.access_token).not.toBe('')
    expect(body.expires_in).toBeTypeOf('number')
    expect(body.user?.roles).toEqual(['admin'])
  })

  it('maps liveops to live_ops and viewer to viewer', async () => {
    const liveops = await postJson('/admin-auth/login', {
      email: 'liveops@example.com',
      password: 'x',
    })
    expect(((await liveops.json()) as TokenResponse).user?.roles).toEqual(['live_ops'])

    const viewer = await postJson('/admin-auth/login', {
      email: 'viewer@example.com',
      password: 'x',
    })
    expect(((await viewer.json()) as TokenResponse).user?.roles).toEqual(['viewer'])
  })

  it('refuses a sign-in whose local part starts with denied, in the COM-5 shape', async () => {
    const response = await postJson('/admin-auth/login', {
      email: 'denied@example.com',
      password: 'x',
    })

    expect(response.status).toBe(401)
    const body = (await response.json()) as { error?: { code?: string } }
    expect(body.error?.code).toBe('invalid_credentials')
  })

  it('asks for a second factor and issues no token when the local part starts with mfa', async () => {
    const response = await postJson('/admin-auth/login', {
      email: 'mfa.ops@example.com',
      password: 'x',
    })

    expect(response.status).toBe(200)
    const body = (await response.json()) as TokenResponse
    expect(body.mfa_required).toBe(true)
    expect(body.mfa_ticket).toBeTypeOf('string')
    expect(body.access_token).toBeUndefined()

    const verified = await postJson('/admin-auth/mfa/verify', {
      mfa_ticket: body.mfa_ticket,
      code: '123456',
    })
    expect(verified.status).toBe(200)
    expect(((await verified.json()) as TokenResponse).access_token).toBeTypeOf('string')
  })
})

describe('config draft fixtures', () => {
  it('refuses a save whose revision is not the current one', async () => {
    const read = await fetch('/api/admin/config/namespaces/balance.weapons/draft')
    expect(read.status).toBe(200)
    const before = (await read.json()) as DraftResponse
    expect(before.revision).toBeTypeOf('number')

    const saved = await fetch('/api/admin/config/namespaces/balance.weapons/draft', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ document: before.document, revision: before.revision }),
    })
    expect(saved.status).toBe(200)
    const after = (await saved.json()) as DraftResponse
    expect(after.revision).toBe((before.revision ?? 0) + 1)

    // Same revision as the first save, which the first save has already moved
    // past: exactly the second-writer case CFG-B3 returns 409 for.
    const conflict = await fetch('/api/admin/config/namespaces/balance.weapons/draft', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ document: before.document, revision: before.revision }),
    })

    expect(conflict.status).toBe(409)
    const body = (await conflict.json()) as ErrorResponse
    expect(body.error.code).toBe('stale_revision')
    expect(Object.keys(body.error).sort()).toEqual(['code', 'message', 'request_id'])
    expect(body.error.request_id).toBeTypeOf('string')
  })

  it('serves the schema and the draft as two separate reads', async () => {
    const schema = await fetch('/api/admin/config/namespaces/balance.weapons/schema')
    expect(schema.status).toBe(200)
    const schemaBody = (await schema.json()) as { namespace?: string; schema_version?: number }
    expect(schemaBody.namespace).toBe('balance.weapons')
    expect(schemaBody.schema_version).toBeTypeOf('number')

    const draft = await fetch('/api/admin/config/namespaces/balance.weapons/draft')
    expect(((await draft.json()) as DraftResponse).revision).toBeTypeOf('number')

    const missing = await fetch('/api/admin/config/namespaces/nope/draft')
    expect(missing.status).toBe(404)
    expect(((await missing.json()) as ErrorResponse).error.code).toBe('not_found')
  })
})

/**
 * The presentation envelope is only useful if it degrades, and it only degrades
 * in a way a later test can check if the fixture actually contains the two
 * mismatches. Asserting them here means the degradation test cannot be written
 * against a fixture that has quietly become consistent.
 */
describe('ui.presentation fixture', () => {
  const envelope = uiPresentation.draft.document['balance.weapons']
  const schemaProperties = Object.keys(balanceWeapons.schema.body.properties)

  it('names a field in order that the namespace schema does not define', () => {
    const unknown = envelope.order.filter((field) => !schemaProperties.includes(field))
    expect(unknown).toHaveLength(1)
  })

  it('leaves a schema field out of order, so it has to be appended to a trailing group', () => {
    const unmentioned = schemaProperties.filter((field) => !envelope.order.includes(field))
    expect(unmentioned).toHaveLength(1)
  })

  it('is describing the same namespace, not two unrelated fixtures', () => {
    const shared = envelope.order.filter((field) => schemaProperties.includes(field))
    expect(shared.length).toBeGreaterThan(0)
  })
})
