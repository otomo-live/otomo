import { HttpResponse, http } from 'msw'
import { describe, expect, it } from 'vitest'

import {
  createNamespace,
  createVersion,
  getChannelHead,
  getDiff,
  getDraft,
  getPreviousRelease,
  getRelease,
  getSchema,
  getVersion,
  isNoChanges,
  isStaleRelease,
  isStaleRevision,
  listNamespaces,
  listReleaseHistory,
  listReleases,
  listVersions,
  promoteRelease,
  publishRelease,
  putSchema,
  rollbackRelease,
  isStaleSchema,
  saveDraft,
  validateDocument,
} from '@/api/config'
import { ApiError } from '@/api/errors'
import configReleasesDev from '@/mocks/fixtures/config-releases-dev.json'
import { server } from '@/mocks/server'

/**
 * The Config client against the fixture API, through a real `fetch`, because the
 * things that break in practice are the path the SPA addresses and the shape it
 * reads a body as. Every response is read rather than cast, so a body that is not
 * the documented shape is an ApiError rather than a page full of `undefined`.
 *
 * The save cases come last in this file: they are the ones that move the fixture's
 * draft, and every other case here is a reader.
 */

describe('listNamespaces', () => {
  it('lists a namespace with its audience and its draft in flight', async () => {
    const namespaces = await listNamespaces()
    const weapons = namespaces.find((candidate) => candidate.name === 'balance.weapons')

    expect(weapons?.audience).toBe('client')
    expect(weapons?.description).not.toBe('')
    expect(weapons?.latestVersion).toBe(11)
    expect(weapons?.draft.revision).toBe(12)
    expect(weapons?.draft.updatedAt).toBe('2026-09-21T09:12:44Z')
    // This fixture's draft is byte for byte what version 11 published, so there
    // is nothing unpublished. The comparison is the service's, not the SPA's:
    // the column is read, never computed here.
    expect(weapons?.draft.hasUnpublishedChanges).toBe(false)
  })

  it('lists the reserved presentation namespace like any other', async () => {
    // It is a Config namespace and not a file, which is the whole point: it
    // travels the same draft, version and audit path as everything else.
    const namespaces = await listNamespaces()
    expect(namespaces.map((candidate) => candidate.name)).toContain('ui.presentation')
  })

  it('says which drafts are ahead of the last version, and which are not', async () => {
    // The other half of the flag: `ui.presentation` has a draft that mentions a
    // render hint version 6 does not, so the two namespaces disagree, which is
    // what makes this a test of the reading rather than of one fixture value.
    const namespaces = await listNamespaces()
    const presentation = namespaces.find((candidate) => candidate.name === 'ui.presentation')

    expect(presentation?.draft.hasUnpublishedChanges).toBe(true)
  })
})

describe('getSchema', () => {
  it('reads the schema, its version and who wrote it', async () => {
    const snapshot = await getSchema('balance.weapons')

    expect(snapshot.namespace).toBe('balance.weapons')
    expect(snapshot.schemaVersion).toBe(3)
    expect(snapshot.createdBy).toBe('admin@example.com')
    expect(snapshot.schema).toMatchObject({ title: 'Weapon', type: 'object' })
  })

  it('refuses an unknown namespace in the COM-5 shape', async () => {
    const error = await getSchema('nope').catch((caught: unknown) => caught)

    expect(error).toBeInstanceOf(ApiError)
    expect((error as ApiError).status).toBe(404)
    expect((error as ApiError).code).toBe('not_found')
    expect((error as ApiError).requestId).toBeTypeOf('string')
  })
})

describe('getDraft', () => {
  it('reads the document, the revision and the version it was taken from', async () => {
    const draft = await getDraft('balance.weapons')

    expect(draft.document).toEqual({ name: 'Arc Rifle', damage: 40, range: 18.5, ammo: 6 })
    expect(draft.revision).toBe(12)
    expect(draft.baseVersion).toBe(11)
    expect(draft.updatedBy).toBe('liveops@example.com')
    expect(draft.updatedAt).toBe('2026-09-21T09:12:44Z')
  })

  it('tells a draft that is ahead of the last version from one that is not', async () => {
    // `baseVersion` is what a version composer needs, and it is also the only
    // thing that says whether the draft has ever been published.
    const draft = await getDraft('balance.weapons')
    expect(draft.baseVersion).toBeLessThan(draft.revision)
  })
})

describe('validateDocument', () => {
  it('passes a document the schema accepts', async () => {
    const result = await validateDocument('balance.weapons', {
      name: 'Arc Rifle',
      damage: 40,
      range: 18.5,
      ammo: 6,
    })
    expect(result.valid).toBe(true)
    expect(result.errors).toEqual([])
  })

  it('reports what the service refuses, with a pointer per issue', async () => {
    const result = await validateDocument('balance.weapons', {
      name: 'Arc Rifle',
      damage: 900,
      range: 18.5,
    })

    expect(result.valid).toBe(false)
    expect(result.errors).toContainEqual({ pointer: '/damage', message: 'must be <= 500' })
  })

  it('stores nothing, which is what makes it usable on save', async () => {
    const before = await getDraft('balance.weapons')

    await validateDocument('balance.weapons', { name: 'x', damage: -1, range: -1 })

    const after = await getDraft('balance.weapons')
    expect(after.revision).toBe(before.revision)
    expect(after.document).toEqual(before.document)
  })
})

describe('versions and diff', () => {
  it('lists versions newest first and pages with before', async () => {
    const page = await listVersions('balance.weapons')

    // Newest first, and two rows fit one page, so there is no cursor.
    expect(page.versions.map((version) => version.version)).toEqual([11, 10])
    expect(page.nextBefore).toBeNull()

    const older = await listVersions('balance.weapons', { before: 11, limit: 1 })
    expect(older.versions.map((version) => version.version)).toEqual([10])
  })

  it('reads one version with its size and full document', async () => {
    const version = await getVersion('balance.weapons', 10)

    expect(version.version).toBe(10)
    expect(version.schemaVersion).toBe(3)
    expect(version.document).toMatchObject({ name: 'Arc Rifle', range: 21 })
    expect(version.size).toBeGreaterThan(0)
  })

  it('diffs two versions and returns both documents', async () => {
    const diff = await getDiff('balance.weapons', '10', '11')

    expect(diff.from.ref).toBe('10')
    expect(diff.to.ref).toBe('11')
    expect(diff.from.document).toMatchObject({ range: 21 })
    expect(diff.to.document).toMatchObject({ range: 18.5 })
    expect(diff.changes).toContainEqual({ op: 'replace', path: '/range', from: 21, to: 18.5 })
  })

  it('refuses a stale revision and a draft with nothing to version', async () => {
    const stale = await createVersion('balance.weapons', { message: 'x', revision: 99 }).catch(
      (caught: unknown) => caught,
    )
    expect(isStaleRevision(stale)).toBe(true)

    const noChanges = await createVersion('balance.weapons', { message: 'x', revision: 12 }).catch(
      (caught: unknown) => caught,
    )
    expect(isNoChanges(noChanges)).toBe(true)
    expect((noChanges as ApiError).message).toContain('version 11')
  })

  it('refuses readers pointed at a namespace that does not exist', async () => {
    const version = await getVersion('nope.namespace', 1).catch((caught: unknown) => caught)
    expect(version).toBeInstanceOf(ApiError)
    expect((version as ApiError).status).toBe(404)

    const versions = await listVersions('nope.namespace').catch((caught: unknown) => caught)
    expect(versions).toBeInstanceOf(ApiError)
    expect((versions as ApiError).code).toBe('not_found')

    const diff = await getDiff('nope.namespace', '1', '2').catch((caught: unknown) => caught)
    expect(diff).toBeInstanceOf(ApiError)
    expect((diff as ApiError).status).toBe(404)
  })
})

describe('saveDraft and the stale revision', () => {
  it('saves against the revision it read and returns the next one', async () => {
    const before = await getDraft('balance.weapons')
    const document = { name: 'Arc Rifle', damage: 42, range: 18.5, ammo: 6 }

    const saved = await saveDraft('balance.weapons', document, before.revision)

    expect(saved.revision).toBe(before.revision + 1)
    expect(saved.updatedAt).not.toBe('')
    expect((await getDraft('balance.weapons')).document).toEqual(document)
  })

  it('refuses a save whose revision has been overtaken, and says so in a way a caller can test', async () => {
    const stale = (await getDraft('balance.weapons')).revision - 1

    const error = await saveDraft('balance.weapons', { name: 'x' }, stale).catch(
      (caught: unknown) => caught,
    )

    expect(error).toBeInstanceOf(ApiError)
    expect((error as ApiError).status).toBe(409)
    expect(isStaleRevision(error)).toBe(true)
  })

  it('does not mistake another refusal for a conflict', () => {
    // The editor's whole 409 path hangs on this predicate, so it has to be false
    // for a 404 and for anything that is not an ApiError at all.
    expect(isStaleRevision(new Error('stale_revision'))).toBe(false)
    expect(isStaleRevision(null)).toBe(false)
    expect(isStaleRevision(undefined)).toBe(false)
    expect(
      isStaleRevision(
        new ApiError({ status: 404, code: 'not_found', message: 'x', requestId: null }),
      ),
    ).toBe(false)
  })
})

describe('createNamespace and putSchema', () => {
  it('creates a namespace in the same shape the list reads back', async () => {
    const created = await createNamespace({
      name: 'unit.api_created',
      audience: 'client',
      description: 'created by a unit test',
    })

    expect(created.name).toBe('unit.api_created')
    expect(created.audience).toBe('client')
    expect(created.latestVersion).toBe(0)
    expect(created.draft.revision).toBe(1)
    expect((await listNamespaces()).some((ns) => ns.name === 'unit.api_created')).toBe(true)
  })

  it('refuses a duplicate name with already_exists', async () => {
    const error = await createNamespace({
      name: 'balance.weapons',
      audience: 'client',
      description: '',
    }).catch((caught: unknown) => caught)

    expect(error).toBeInstanceOf(ApiError)
    expect((error as ApiError).status).toBe(409)
    expect((error as ApiError).code).toBe('already_exists')
  })

  it('sends the schema document as the body and adopts the new version', async () => {
    const before = await getSchema('ui.presentation')

    const saved = await putSchema('ui.presentation', { type: 'object' }, before.schemaVersion)

    expect(saved.schemaVersion).toBe(before.schemaVersion + 1)
    expect(saved.schema).toEqual({ type: 'object' })
    expect((await getSchema('ui.presentation')).schema).toEqual({ type: 'object' })
  })

  it('refuses an invalid schema with validation_failed', async () => {
    const current = await getSchema('ui.presentation')
    const error = await putSchema('ui.presentation', { type: 12 }, current.schemaVersion).catch(
      (caught: unknown) => caught,
    )

    expect(error).toBeInstanceOf(ApiError)
    expect((error as ApiError).status).toBe(400)
    expect((error as ApiError).code).toBe('validation_failed')
    expect((error as ApiError).message).toContain('not a valid JSON Schema')
  })

  it('sends the edited version as If-Match and reports a stale one as stale_schema', async () => {
    const current = await getSchema('ui.presentation')
    const error = await putSchema(
      'ui.presentation',
      { type: 'object' },
      current.schemaVersion - 1,
    ).catch((caught: unknown) => caught)

    expect(error).toBeInstanceOf(ApiError)
    expect((error as ApiError).status).toBe(409)
    expect(isStaleSchema(error)).toBe(true)
    expect((await getSchema('ui.presentation')).schemaVersion).toBe(current.schemaVersion)
  })
})

describe('releases', () => {
  it('reads a release with its manifest parsed into typed namespaces and packs', async () => {
    const release = await getRelease('dev', 3)

    expect(release.releaseId).toBe(3)
    expect(release.channel).toBe('dev')
    expect(release.minClientVersion).toBe('1.1.0')
    expect(release.manifest.releaseId).toBe(3)
    expect(release.manifest.config['balance.weapons']).toEqual({
      version: 11,
      sha256: '3333333333333333333333333333333333333333333333333333333333333333',
      size: 640,
    })
    expect(release.manifest.packs.map((pack) => pack.name).sort()).toEqual([
      'audio.pak',
      'weapons.pak',
    ])
  })

  it('reads a pre-feature release as having no server manifest', async () => {
    const release = await getRelease('dev', 3)

    expect(release.serverManifest).toBeNull()
    expect(release.serverManifestSha256).toBeNull()
  })

  it('parses a server manifest with the same reader as the client one', async () => {
    server.use(
      http.get('/api/admin/config/channels/:channel/releases', () =>
        HttpResponse.json({
          head_release_id: 5,
          next_before: null,
          releases: [
            {
              release_id: 5,
              channel: 'dev',
              manifest_sha256: 'aa'.repeat(32),
              server_manifest_sha256: 'bb'.repeat(32),
              min_client_version: '1.0.0',
              message: 'server pass',
              created_by: 'admin@example.com',
              created_at: '2026-09-20T00:00:00Z',
              is_head: true,
              manifest: {
                format: 1,
                channel: 'dev',
                release_id: 5,
                created_at: '2026-09-20T00:00:00Z',
                min_client_version: '1.0.0',
                config: { 'balance.weapons': { version: 11, sha256: 'cc', size: 10 } },
                packs: [],
              },
              server_manifest: {
                format: 1,
                channel: 'dev',
                release_id: 5,
                created_at: '2026-09-20T00:00:00Z',
                min_client_version: '1.0.0',
                config: { 'server.tuning': { version: 1, sha256: 'dd', size: 20 } },
                packs: [],
              },
            },
          ],
        }),
      ),
    )

    const release = await getRelease('dev', 5)

    expect(release.serverManifest?.config['server.tuning']).toEqual({
      version: 1,
      sha256: 'dd',
      size: 20,
    })
    expect(release.serverManifest?.packs).toEqual([])
    expect(release.serverManifestSha256).toBe('bb'.repeat(32))
  })

  it('reads the server hash on history rows, or null without one', async () => {
    const row = (id: number, serverHash: string | null) => ({
      release_id: id,
      channel: 'dev',
      manifest_sha256: 'aa'.repeat(32),
      server_manifest_sha256: serverHash,
      min_client_version: '1.0.0',
      message: `release ${id}`,
      created_by: 'admin@example.com',
      created_at: '2026-09-20T00:00:00Z',
      is_head: id === 6,
      manifest: {
        format: 1,
        channel: 'dev',
        release_id: id,
        created_at: '2026-09-20T00:00:00Z',
        min_client_version: '1.0.0',
        config: {},
        packs: [],
      },
      server_manifest: null,
    })
    server.use(
      http.get('/api/admin/config/channels/:channel/releases', () =>
        HttpResponse.json({
          head_release_id: 6,
          next_before: null,
          releases: [row(6, 'ee'.repeat(32)), row(5, null)],
        }),
      ),
    )

    const page = await listReleaseHistory('dev')

    expect(page.releases[0].serverManifestSha256).toBe('ee'.repeat(32))
    expect(page.releases[1].serverManifestSha256).toBeNull()
  })

  it('refuses an id the channel does not hold rather than returning a neighbour', async () => {
    const error = await getRelease('dev', 99).catch((caught: unknown) => caught)

    expect(error).toBeInstanceOf(ApiError)
    expect((error as ApiError).status).toBe(404)
    expect((error as ApiError).code).toBe('not_found')
  })

  it('lists releases newest first and walks back with before', async () => {
    const all = await listReleases('dev')
    expect(all.map((release) => release.releaseId)).toEqual([3, 2, 1])
    expect((await listReleases('dev', { before: 2, limit: 1 }))[0].releaseId).toBe(1)
  })

  it('finds the release just below one, and null for the first', async () => {
    expect((await getPreviousRelease('dev', 3))?.releaseId).toBe(2)
    expect(await getPreviousRelease('dev', 1)).toBeNull()
  })

  it('refuses an unknown channel in the COM-5 shape', async () => {
    const error = await getRelease('nightly', 1).catch((caught: unknown) => caught)
    expect(error).toBeInstanceOf(ApiError)
    expect((error as ApiError).code).toBe('validation_failed')
  })

  it('reads the head, the rows and the next cursor from a history page', async () => {
    const page = await listReleaseHistory('dev', { limit: 2 })

    expect(page.headReleaseId).toBe(3)
    expect(page.releases.map((release) => release.releaseId)).toEqual([3, 2])
    expect(page.releases[0].isHead).toBe(true)
    expect(page.releases[1].isHead).toBe(false)
    expect(page.nextBefore).toBe(2)
  })

  it('posts a rollback with both ids to the channel rollback path', async () => {
    let path = ''
    let body: unknown = null
    server.use(
      http.post('/api/admin/config/channels/:channel/rollback', async ({ request }) => {
        path = new URL(request.url).pathname
        body = await request.json()
        return HttpResponse.json(configReleasesDev.releases[1])
      }),
    )

    const release = await rollbackRelease('dev', { release_id: 2, base_release_id: 3 })

    expect(path).toBe('/api/admin/config/channels/dev/rollback')
    expect(body).toEqual({ release_id: 2, base_release_id: 3 })
    expect(release.releaseId).toBe(2)
  })

  it('posts a promote with from in the query and a message in the body', async () => {
    let path = ''
    let from: string | null = null
    let body: unknown = null
    server.use(
      http.post('/api/admin/config/channels/:channel/promote', async ({ request }) => {
        const url = new URL(request.url)
        path = url.pathname
        from = url.searchParams.get('from')
        body = await request.json()
        return HttpResponse.json({ ...configReleasesDev.releases[0], release_id: 4 })
      }),
    )

    const release = await promoteRelease('staging', 'dev', {
      base_release_id: 1,
      message: 'Promote the balance pass',
    })

    expect(path).toBe('/api/admin/config/channels/staging/promote')
    expect(from).toBe('dev')
    expect(body).toEqual({ base_release_id: 1, message: 'Promote the balance pass' })
    expect(release.releaseId).toBe(4)
  })

  it('reads the channel head from the history, id and row together', async () => {
    const head = await getChannelHead('dev')

    expect(head.headReleaseId).toBe(3)
    expect(head.release?.releaseId).toBe(3)
  })

  it('refuses an unknown channel for the history and the head', async () => {
    const history = await listReleaseHistory('nightly').catch((caught: unknown) => caught)
    expect(history).toBeInstanceOf(ApiError)
    expect((history as ApiError).code).toBe('validation_failed')

    const rows = await listReleases('nightly').catch((caught: unknown) => caught)
    expect(rows).toBeInstanceOf(ApiError)

    const previous = await getPreviousRelease('nightly', 1).catch((caught: unknown) => caught)
    expect(previous).toBeInstanceOf(ApiError)

    const head = await getChannelHead('nightly').catch((caught: unknown) => caught)
    expect(head).toBeInstanceOf(ApiError)
    expect((head as ApiError).status).toBe(400)
  })

  it('publishes a release from the versions the composer selected', async () => {
    const release = await publishRelease('dev', {
      base_release_id: 3,
      versions: [{ namespace: 'balance.weapons', version: 10 }],
      packs: [],
      min_client_version: '1.0.0',
      message: 'Publish balance v10 from a unit test',
    })

    expect(release.releaseId).toBe(4)
    expect(release.channel).toBe('dev')
    expect(release.manifest.config['balance.weapons'].version).toBe(10)
    expect((await getChannelHead('dev')).headReleaseId).toBe(4)
  })

  it('refuses a publish whose base is not the head with stale_release', async () => {
    const error = await publishRelease('dev', {
      base_release_id: 999,
      versions: [],
      packs: [],
      min_client_version: '1.0.0',
      message: 'x',
    }).catch((caught: unknown) => caught)

    expect(isStaleRelease(error)).toBe(true)
  })

  it('refuses a rollback and a promote read against the wrong head', async () => {
    // Both are admin-only in gateway_dev, so the caller has to be an admin for
    // the stale check to be the refusal under test rather than a 403.
    await fetch('/admin-auth/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ email: 'admin@example.com', password: 'x' }),
    })

    const rollback = await rollbackRelease('dev', { release_id: 2, base_release_id: 999 }).catch(
      (caught: unknown) => caught,
    )
    expect(isStaleRelease(rollback)).toBe(true)

    const promote = await promoteRelease('staging', 'dev', {
      base_release_id: 999,
      message: 'x',
    }).catch((caught: unknown) => caught)
    expect(isStaleRelease(promote)).toBe(true)
  })
})

describe('createVersion from a saved draft', () => {
  it('cuts the next version and lists it as the newest', async () => {
    const draft = await getDraft('balance.weapons')
    const document = { ...(draft.document as Record<string, unknown>), damage: 77 }
    const saved = await saveDraft('balance.weapons', document, draft.revision)

    const created = await createVersion('balance.weapons', {
      message: 'Cut from a unit test',
      revision: saved.revision,
    })

    expect(created.version).toBe(12)
    expect(created.sha256).toMatch(/^[0-9a-f]{64}$/)
    expect((await listVersions('balance.weapons')).versions[0].version).toBe(12)
  })
})
