import { describe, expect, it } from 'vitest'

import type { NamespaceSummary, PackSummary, Release, ReleaseManifest } from '@/api/config'
import {
  buildDraftManifests,
  defaultVersionSelection,
  manifestHasChanges,
  manifestsHaveChanges,
} from '@/config/releaseCompose'
import { diffManifests } from '@/config/releaseDiff'

/**
 * The composer's pure state: the default selection, the two draft manifests and
 * the "is there anything to publish" question. Each case is a literal so the test
 * says which namespace moved, not how a release got that way.
 */

function namespace(name: string, audience: string, latestVersion = 1): NamespaceSummary {
  return {
    name,
    audience,
    description: '',
    latestVersion,
    draft: { revision: 1, updatedAt: '2026-09-20T00:00:00Z', hasUnpublishedChanges: false },
  }
}

function manifest(overrides: Partial<ReleaseManifest> = {}): ReleaseManifest {
  return {
    format: 1,
    channel: 'dev',
    releaseId: 1,
    createdAt: '2026-09-20T00:00:00Z',
    minClientVersion: '1.0.0',
    config: {},
    packs: [],
    ...overrides,
  }
}

function release(client: ReleaseManifest, server: ReleaseManifest | null): Release {
  return {
    releaseId: 1,
    channel: 'dev',
    manifestSha256: 'ff'.repeat(32),
    serverManifestSha256: server === null ? null : 'ee'.repeat(32),
    minClientVersion: '1.0.0',
    message: '',
    createdBy: 'admin@example.com',
    createdAt: '2026-09-20T00:00:00Z',
    manifest: client,
    serverManifest: server,
  }
}

function pack(sha256: string, name = 'content.pak'): PackSummary {
  return {
    packId: sha256,
    name,
    sha256,
    size: 5,
    uploadedBy: 'admin@example.com',
    uploadedAt: '2026-09-20T00:00:00Z',
  }
}

describe('defaultVersionSelection', () => {
  it('takes every namespace, client and server, at its latest version', () => {
    const selection = defaultVersionSelection(
      [namespace('client.a', 'client'), namespace('server.x', 'server')],
      { 'client.a': [3, 2], 'server.x': [7, 6] },
    )
    expect(selection).toEqual({ 'client.a': 3, 'server.x': 7 })
  })

  it('selects null for a namespace with no versions', () => {
    const selection = defaultVersionSelection(
      [namespace('client.a', 'client'), namespace('empty.ns', 'client')],
      { 'client.a': [1] },
    )
    expect(selection).toEqual({ 'client.a': 1, 'empty.ns': null })
  })
})

describe('buildDraftManifests', () => {
  const namespaces = [namespace('client.a', 'client'), namespace('server.x', 'server')]
  const head = release(
    manifest({
      config: { 'client.a': { version: 3, sha256: 'c3', size: 10 } },
      packs: [{ name: 'content.pak', sha256: 'p1', size: 5 }],
    }),
    manifest({ config: { 'server.x': { version: 7, sha256: 's7', size: 20 } } }),
  )

  it('splits namespaces by audience and puts the packs only in the client manifest', () => {
    const drafts = buildDraftManifests(
      'dev',
      head,
      {
        versions: { 'client.a': 3, 'server.x': 7 },
        packShas: ['p1'],
        minClientVersion: '1.0.0',
      },
      [pack('p1')],
      namespaces,
    )

    expect(Object.keys(drafts.client.config)).toEqual(['client.a'])
    expect(Object.keys(drafts.server.config)).toEqual(['server.x'])
    expect(drafts.client.packs).toEqual([{ name: 'content.pak', sha256: 'p1', size: 5 }])
    expect(drafts.server.packs).toEqual([])
  })

  it('keeps the head hash for an unchanged version on either side', () => {
    const drafts = buildDraftManifests(
      'dev',
      head,
      {
        versions: { 'client.a': 3, 'server.x': 7 },
        packShas: [],
        minClientVersion: '1.0.0',
      },
      [],
      namespaces,
    )

    expect(drafts.client.config['client.a']).toEqual({ version: 3, sha256: 'c3', size: 10 })
    expect(drafts.server.config['server.x']).toEqual({ version: 7, sha256: 's7', size: 20 })
  })

  it('uses a placeholder hash for a moved version, and treats a null server head as empty', () => {
    const moved = buildDraftManifests(
      'dev',
      head,
      {
        versions: { 'server.x': 8 },
        packShas: [],
        minClientVersion: '1.0.0',
      },
      [],
      namespaces,
    )
    expect(moved.server.config['server.x']).toEqual({ version: 8, sha256: '', size: 0 })

    const noServerHead = buildDraftManifests(
      'dev',
      release(manifest(), null),
      { versions: { 'server.x': 7 }, packShas: [], minClientVersion: '1.0.0' },
      [],
      namespaces,
    )
    expect(noServerHead.server.config['server.x']).toEqual({ version: 7, sha256: '', size: 0 })
  })
})

describe('change detection', () => {
  it('sees a server-only change even when the client manifest is untouched', () => {
    const namespaces = [namespace('client.a', 'client'), namespace('server.x', 'server')]
    const head = release(
      manifest({ config: { 'client.a': { version: 1, sha256: 'c1', size: 1 } } }),
      manifest({ config: { 'server.x': { version: 7, sha256: 's7', size: 20 } } }),
    )
    const drafts = buildDraftManifests(
      'dev',
      head,
      {
        versions: { 'client.a': 1, 'server.x': 8 },
        packShas: [],
        minClientVersion: '1.0.0',
      },
      [],
      namespaces,
    )

    const clientDiff = diffManifests(head.manifest, drafts.client)
    const serverDiff = diffManifests(head.serverManifest, drafts.server)

    expect(manifestHasChanges(clientDiff)).toBe(false)
    expect(manifestsHaveChanges(clientDiff, serverDiff)).toBe(true)
  })
})
