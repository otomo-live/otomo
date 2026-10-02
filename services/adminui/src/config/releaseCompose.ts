import type {
  NamespaceSummary,
  PackSummary,
  Release,
  ReleaseManifest,
  ReleaseNamespace,
} from '@/api/config'
import type { ManifestDiff } from '@/config/releaseDiff'

/**
 * The composer's state as data rather than as a component.
 *
 * A composed release is one version per namespace plus a set of packs and a
 * minimum client version. The interesting part is the would-be manifests that
 * the preview diffs against the channel head, and that is pure arithmetic over
 * the selections: a namespace dropped, a version moved, a pack unchecked. It
 * lives here so each combination can be pinned down with a literal selection
 * instead of through a mounted page.
 *
 * The hashes in a draft are the server's job. For a namespace whose selected
 * version is the head's, the head's hash is kept so an unchanged namespace reads
 * as unchanged; for anything else the version already differs, so a placeholder
 * hash changes nothing and is never sent. Packs carry their real hash because a
 * pack's identity IS the hash and the diff compares nothing else.
 */

/** One namespace's placement in a draft: a version number, or null for "not included". */
export type VersionSelection = Record<string, number | null>

export interface ComposeSelection {
  versions: VersionSelection
  /** The sha256 of every checked pack. */
  packShas: string[]
  minClientVersion: string
}

/**
 * Every namespace at its latest version, client and server alike.
 *
 * The composer hands the server service the whole selection and it sorts the
 * list by audience, so a server namespace is a candidate exactly like a client
 * one. A namespace with no versions is null, which the page renders as "not
 * included" and a disabled select.
 */
export function defaultVersionSelection(
  namespaces: readonly NamespaceSummary[],
  versionsByNamespace: Readonly<Record<string, readonly number[]>>,
): VersionSelection {
  const selection: VersionSelection = {}
  for (const namespace of namespaces) {
    selection[namespace.name] = versionsByNamespace[namespace.name]?.[0] ?? null
  }
  return selection
}

/**
 * The head's packs that this deployment still holds, so the checklist can
 * pre-check them. A head pack that is not in the list cannot be re-selected and
 * the diff will report it removed, which is the honest outcome.
 */
export function selectedHeadPacks(head: Release | null, packs: readonly PackSummary[]): string[] {
  const available = new Set(packs.map((pack) => pack.sha256))
  return (head?.manifest.packs ?? [])
    .map((pack) => pack.sha256)
    .filter((sha256) => available.has(sha256))
}

/** The two draft manifests a selection composes: client namespaces + packs, server namespaces alone. */
export interface DraftManifests {
  client: ReleaseManifest
  server: ReleaseManifest
}

/**
 * The draft manifests the preview diffs, split by audience.
 *
 * The client document carries the client namespaces and the selected packs; the
 * server document carries the server namespaces and always an empty pack list.
 * The hashes are the server's job. For a namespace whose selected version is its
 * audience's head, the head's hash is kept so an unchanged namespace reads as
 * unchanged; for anything else the version already differs, so a placeholder hash
 * changes nothing and is never sent. Packs carry their real hash because a pack's
 * identity IS the hash and the diff compares nothing else.
 */
export function buildDraftManifests(
  channel: string,
  head: Release | null,
  selection: ComposeSelection,
  packs: readonly PackSummary[],
  namespaces: readonly NamespaceSummary[],
): DraftManifests {
  const audienceByName = new Map(namespaces.map((entry) => [entry.name, entry.audience]))
  const clientConfig: Record<string, ReleaseNamespace> = {}
  const serverConfig: Record<string, ReleaseNamespace> = {}

  for (const [namespace, version] of Object.entries(selection.versions)) {
    if (version === null) continue
    const server = audienceByName.get(namespace) === 'server'
    const before = server
      ? head?.serverManifest?.config[namespace]
      : head?.manifest.config[namespace]
    const entry =
      before !== undefined && before.version === version
        ? { version, sha256: before.sha256, size: before.size }
        : { version, sha256: '', size: 0 }
    if (server) serverConfig[namespace] = entry
    else clientConfig[namespace] = entry
  }

  const bySha = new Map(packs.map((pack) => [pack.sha256, pack]))
  const selectedPacks = selection.packShas
    .map((sha256) => bySha.get(sha256))
    .filter((pack): pack is PackSummary => pack !== undefined)
    .map((pack) => ({ name: pack.name, sha256: pack.sha256, size: pack.size }))

  const base = {
    format: 1,
    channel,
    releaseId: 0,
    createdAt: '',
    minClientVersion: selection.minClientVersion,
  }
  return {
    client: { ...base, config: clientConfig, packs: selectedPacks },
    server: { ...base, config: serverConfig, packs: [] },
  }
}

/** Whether a manifest diff carries anything at all. */
export function manifestHasChanges(diff: ManifestDiff): boolean {
  return (
    diff.namespaces.some((entry) => entry.status !== 'unchanged') ||
    diff.packs.length > 0 ||
    diff.minClientVersion.changed
  )
}

/** Whether either composed manifest carries anything: the publish gate asks about both. */
export function manifestsHaveChanges(client: ManifestDiff, server: ManifestDiff): boolean {
  return manifestHasChanges(client) || manifestHasChanges(server)
}

/** The §5 grammar for a minimum client version. */
export const MIN_CLIENT_VERSION_PATTERN = /^\d+\.\d+\.\d+$/

export function isValidMinClientVersion(value: string): boolean {
  return MIN_CLIENT_VERSION_PATTERN.test(value.trim())
}
