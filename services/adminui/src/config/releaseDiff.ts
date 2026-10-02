import type { ReleaseManifest } from '@/api/config'

/**
 * The diff between two release manifests, as data rather than as a component.
 *
 * A manifest names one version per namespace plus its content packs, so a diff is
 * a comparison of two maps and a bag of pack hashes. It lives in a pure module
 * because the interesting cases are combinatorial — a namespace added, removed,
 * changed, or untouched, packs swapped — and each one is easier to pin down with
 * a literal manifest than through a mounted page.
 *
 * A missing base is the first release on a channel, and everything is new: there
 * is nothing to compare against, and calling that "unchanged" would hide the
 * whole manifest a reader opened the page to see.
 */

export type NamespaceChangeStatus = 'added' | 'removed' | 'changed' | 'unchanged'

export interface NamespaceDiff {
  namespace: string
  status: NamespaceChangeStatus
  beforeVersion: number | null
  afterVersion: number | null
  beforeSha256: string | null
  afterSha256: string | null
}

/**
 * A pack's identity is its hash, never its name: the same name re-uploaded with
 * different bytes is new content. For the reader, though, one name leaving with
 * one hash and arriving with another is the same pack updated, so exactly one
 * removed and one added pack of the same name are reported together as
 * `changed` (previousSha256 → sha256). Unchanged packs are not listed.
 */
export interface PackDiff {
  name: string
  sha256: string
  size: number
  status: 'added' | 'removed' | 'changed'
  /** The hash it replaced, for `changed` only. */
  previousSha256?: string
}

export interface MinClientVersionDiff {
  before: string | null
  after: string
  changed: boolean
}

export interface ManifestDiff {
  /** Every namespace in either manifest, sorted by name, unchanged ones included. */
  namespaces: NamespaceDiff[]
  /** Packs added or removed between the two, in manifest order. */
  packs: PackDiff[]
  minClientVersion: MinClientVersionDiff
}

export function diffManifests(base: ReleaseManifest | null, next: ReleaseManifest): ManifestDiff {
  const beforeConfig = base?.config ?? {}
  const names = [...new Set([...Object.keys(beforeConfig), ...Object.keys(next.config)])].sort()
  const namespaces: NamespaceDiff[] = names.map((namespace) => {
    const before = beforeConfig[namespace]
    const after = next.config[namespace]
    if (before === undefined) {
      return {
        namespace,
        status: 'added',
        beforeVersion: null,
        afterVersion: after.version,
        beforeSha256: null,
        afterSha256: after.sha256,
      }
    }
    if (after === undefined) {
      return {
        namespace,
        status: 'removed',
        beforeVersion: before.version,
        afterVersion: null,
        beforeSha256: before.sha256,
        afterSha256: null,
      }
    }
    // The version is what a reader scans, but the hash is the truth: the same
    // version republished with different bytes is a change, and reporting it as
    // unchanged would say the document did not move when it did.
    const changed = before.version !== after.version || before.sha256 !== after.sha256
    return {
      namespace,
      status: changed ? 'changed' : 'unchanged',
      beforeVersion: before.version,
      afterVersion: after.version,
      beforeSha256: before.sha256,
      afterSha256: after.sha256,
    }
  })

  const beforePacks = new Map((base?.packs ?? []).map((pack) => [pack.sha256, pack]))
  const afterPacks = new Map(next.packs.map((pack) => [pack.sha256, pack]))
  const packs: PackDiff[] = []
  for (const [sha256, pack] of afterPacks) {
    if (!beforePacks.has(sha256)) {
      packs.push({ name: pack.name, sha256, size: pack.size, status: 'added' })
    }
  }
  for (const [sha256, pack] of beforePacks) {
    if (!afterPacks.has(sha256)) {
      packs.push({ name: pack.name, sha256, size: pack.size, status: 'removed' })
    }
  }

  // Pair a single removal and a single addition under one name into `changed`.
  // Several packs sharing a name are ambiguous and stay as separate rows.
  const byName = new Map<string, PackDiff[]>()
  for (const pack of packs) byName.set(pack.name, [...(byName.get(pack.name) ?? []), pack])
  for (const group of byName.values()) {
    if (group.length !== 2) continue
    const added = group.find((pack) => pack.status === 'added')
    const removed = group.find((pack) => pack.status === 'removed')
    if (added === undefined || removed === undefined) continue
    added.status = 'changed'
    added.previousSha256 = removed.sha256
    packs.splice(packs.indexOf(removed), 1)
  }

  const minClientVersionBefore = base?.minClientVersion ?? null
  return {
    namespaces,
    packs,
    minClientVersion: {
      before: minClientVersionBefore,
      after: next.minClientVersion,
      changed: minClientVersionBefore !== next.minClientVersion,
    },
  }
}

/**
 * The diff of two server manifests. A server manifest repeats the release's
 * min_client_version only because it shares the client manifest's format; game
 * servers and Session never read it, and the client diff already reports it. So
 * it is never a change here. Without this, a head released before server
 * manifests existed (base null) would read as a min-version change on every
 * compose and enable Publish with nothing to publish.
 */
export function diffServerManifests(
  base: ReleaseManifest | null,
  next: ReleaseManifest,
): ManifestDiff {
  const diff = diffManifests(base, next)
  return {
    ...diff,
    minClientVersion: {
      before: next.minClientVersion,
      after: next.minClientVersion,
      changed: false,
    },
  }
}
