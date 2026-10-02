/**
 * The client-side rules for a content-pack upload.
 *
 * Config owns the authority (services/config/internal/api/packs.go): it checks
 * the name grammar, the `GDPC` magic and the 512 MiB cap before a byte reaches
 * the blob store. Everything here mirrors those rules so the upload panel can
 * refuse an obviously wrong file before spending a slow connection on it, and so
 * the mock handler can enforce the same rules in the browser. Mirrored, not
 * invented: the constants below are the ones Config uses.
 */

/** The name grammar from design/02-config.md §5 and `packNameRE` in packs.go. */
export const PACK_NAME_RE = /^[a-z][a-z0-9_]{0,63}$/

/** Config's `maxPackBytesDefault`: 512 MiB. */
export const MAX_PACK_BYTES = 536870912

/** The four-byte header every Godot pack begins with (`packMagic` in packs.go). */
export const PACK_MAGIC = 'GDPC'

export function isValidPackName(name: string): boolean {
  return PACK_NAME_RE.test(name)
}

/**
 * A candidate name from a chosen file: lowercased, the `.pck` extension dropped,
 * and every character the grammar does not allow folded to `_`. It is only a
 * prefill; the field is validated live and the user can correct it.
 */
export function derivePackName(fileName: string): string {
  return fileName
    .replace(/\.pck$/i, '')
    .toLowerCase()
    .replace(/[^a-z0-9_]/g, '_')
    .slice(0, 64)
}

/**
 * Whether the first four bytes are the Godot header. A file shorter than four
 * bytes is not one.
 */
export function isPckHeader(bytes: Uint8Array): boolean {
  if (bytes.length < 4) return false
  return bytes[0] === 0x47 && bytes[1] === 0x44 && bytes[2] === 0x50 && bytes[3] === 0x43
}

/** A sentence naming the limit, or null when the file is within it. */
export function packSizeProblem(size: number): string | null {
  if (size <= MAX_PACK_BYTES) return null
  return `That file is ${formatBytes(size)}. The upload limit is ${formatBytes(
    MAX_PACK_BYTES,
  )} (512 MiB).`
}

/** The result line the upload panel shows, shared so a test can pin both cases. */
export function uploadResultText(created: boolean, name: string): string {
  return created ? 'Uploaded' : `Already uploaded — same bytes as ${name}`
}

const BYTE_UNITS = ['B', 'KiB', 'MiB', 'GiB', 'TiB'] as const

/** Binary units, because the limit the backend names is 512 MiB and not 512 MB. */
export function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return '—'
  let value = bytes
  let unit = 0
  while (value >= 1024 && unit < BYTE_UNITS.length - 1) {
    value /= 1024
    unit += 1
  }
  const decimals = unit === 0 || value >= 100 ? 0 : value >= 10 ? 1 : 2
  return `${value.toFixed(decimals)} ${BYTE_UNITS[unit]}`
}
