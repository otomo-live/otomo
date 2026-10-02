import { describe, expect, it } from 'vitest'

import type { ReleaseManifest } from '@/api/config'
import { diffManifests, diffServerManifests } from '@/config/releaseDiff'

/**
 * The release diff, against literal manifests.
 *
 * A manifest's interesting cases are combinatorial, so each one is built from a
 * one-line namespace/pack literal rather than from the fixture API: the test says
 * what moved, not how a release got that way.
 */

function namespace(version: number, sha256 = `sha-${version}`) {
  return { version, sha256, size: 100 }
}

function manifest(overrides: Partial<ReleaseManifest> = {}): ReleaseManifest {
  return {
    format: 1,
    channel: 'dev',
    releaseId: 2,
    createdAt: '2026-09-18T00:00:00Z',
    minClientVersion: '1.0.0',
    config: {},
    packs: [],
    ...overrides,
  }
}

describe('diffManifests', () => {
  it('reports a namespace added, removed, changed and unchanged', () => {
    const base = manifest({
      config: {
        'changed.ns': namespace(1, 'old'),
        'removed.ns': namespace(2),
        'unchanged.ns': namespace(3, 'same'),
      },
    })
    const next = manifest({
      config: {
        'added.ns': namespace(1),
        'changed.ns': namespace(2, 'new'),
        'unchanged.ns': namespace(3, 'same'),
      },
    })

    const diff = diffManifests(base, next)
    const byName = Object.fromEntries(diff.namespaces.map((entry) => [entry.namespace, entry]))

    expect(byName['added.ns']).toMatchObject({
      status: 'added',
      beforeVersion: null,
      afterVersion: 1,
    })
    expect(byName['removed.ns']).toMatchObject({
      status: 'removed',
      beforeVersion: 2,
      afterVersion: null,
    })
    expect(byName['changed.ns']).toMatchObject({
      status: 'changed',
      beforeVersion: 1,
      afterVersion: 2,
    })
    expect(byName['unchanged.ns']).toMatchObject({
      status: 'unchanged',
      beforeVersion: 3,
      afterVersion: 3,
    })
    // Sorted by name, so a reader's scan does not depend on manifest order.
    expect(diff.namespaces.map((entry) => entry.namespace)).toEqual([
      'added.ns',
      'changed.ns',
      'removed.ns',
      'unchanged.ns',
    ])
  })

  it('calls a same-version republish with different bytes a change', () => {
    const base = manifest({ config: { 'balance.weapons': namespace(11, 'one') } })
    const next = manifest({ config: { 'balance.weapons': namespace(11, 'two') } })

    expect(diffManifests(base, next).namespaces[0].status).toBe('changed')
  })

  it('reports packs added and removed by hash, not by name', () => {
    const base = manifest({
      packs: [
        { name: 'keep.pak', sha256: 'k', size: 1 },
        { name: 'gone.pak', sha256: 'g', size: 2 },
      ],
    })
    const next = manifest({
      packs: [
        { name: 'keep.pak', sha256: 'k', size: 1 },
        { name: 'new.pak', sha256: 'n', size: 3 },
      ],
    })

    const packs = diffManifests(base, next).packs
    expect(packs.find((pack) => pack.sha256 === 'n')).toMatchObject({
      name: 'new.pak',
      size: 3,
      status: 'added',
    })
    expect(packs.find((pack) => pack.sha256 === 'g')).toMatchObject({
      name: 'gone.pak',
      status: 'removed',
    })
    expect(packs.some((pack) => pack.sha256 === 'k')).toBe(false)
  })

  it('treats a missing base as the first release, so everything is added', () => {
    const next = manifest({
      config: { 'balance.weapons': namespace(1) },
      packs: [{ name: 'weapons.pak', sha256: 's', size: 10 }],
      minClientVersion: '2.0.0',
    })

    const diff = diffManifests(null, next)
    expect(diff.namespaces.map((entry) => entry.status)).toEqual(['added'])
    expect(diff.packs.map((pack) => pack.status)).toEqual(['added'])
    expect(diff.minClientVersion).toEqual({ before: null, after: '2.0.0', changed: true })
  })

  it('reports the minimum client version, or says it did not move', () => {
    const base = manifest({ minClientVersion: '1.0.0' })
    expect(diffManifests(base, manifest({ minClientVersion: '1.0.0' })).minClientVersion).toEqual({
      before: '1.0.0',
      after: '1.0.0',
      changed: false,
    })
    expect(diffManifests(base, manifest({ minClientVersion: '1.1.0' })).minClientVersion).toEqual({
      before: '1.0.0',
      after: '1.1.0',
      changed: true,
    })
  })
})

describe('diffManifests pack pairing', () => {
  const manifest = (packs: { name: string; sha256: string; size: number }[]) => ({
    format: 1,
    channel: 'dev',
    releaseId: 1,
    createdAt: '2026-09-26T00:00:00Z',
    minClientVersion: '1.0.0',
    config: {},
    packs,
  })

  it('reports one name leaving with one hash and arriving with another as changed', () => {
    const diff = diffManifests(
      manifest([{ name: 'weapons.pak', sha256: 'a'.repeat(64), size: 1 }]),
      manifest([{ name: 'weapons.pak', sha256: 'b'.repeat(64), size: 2 }]),
    )
    expect(diff.packs).toEqual([
      {
        name: 'weapons.pak',
        sha256: 'b'.repeat(64),
        size: 2,
        status: 'changed',
        previousSha256: 'a'.repeat(64),
      },
    ])
  })

  it('keeps genuinely different packs as added and removed', () => {
    const diff = diffManifests(
      manifest([{ name: 'audio.pak', sha256: 'a'.repeat(64), size: 1 }]),
      manifest([{ name: 'legacy.pak', sha256: 'b'.repeat(64), size: 2 }]),
    )
    expect(diff.packs.map((pack) => pack.status).sort()).toEqual(['added', 'removed'])
  })
})

describe('diffServerManifests', () => {
  it('never reports the mirrored min client version, even against a missing base', () => {
    const next = manifest({ minClientVersion: '2.0.0', config: { 'session.rules': namespace(1) } })

    const fromLegacy = diffServerManifests(null, next)
    expect(fromLegacy.minClientVersion.changed).toBe(false)
    expect(fromLegacy.namespaces).toEqual([
      expect.objectContaining({ namespace: 'session.rules', status: 'added' }),
    ])

    const moved = diffServerManifests(manifest({ minClientVersion: '1.0.0' }), next)
    expect(moved.minClientVersion.changed).toBe(false)
  })
})
