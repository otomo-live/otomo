/**
 * The ten one-time recovery codes, as a file to keep.
 *
 * The text is built here rather than in the view so a unit test can assert what
 * the download would contain without a browser download existing. `Download .txt`
 * is the only copy of these codes the server will ever give; once the recovery
 * screen is left they are hashed and unrecoverable.
 */

const FILE_NAME = 'otomo-admin-recovery-codes.txt'

/** One code per line, plain, which is what a password manager imports. */
export function recoveryCodesFile(codes: readonly string[]): string {
  const lines = [
    'Otomo Admin recovery codes',
    '',
    'Each code signs you in once. Keep this file somewhere safe and offline.',
    '',
    ...codes,
    '',
  ]
  return lines.join('\n')
}

/** Saves `codes` as a text file. A no-op where the download APIs do not exist. */
export function downloadRecoveryCodes(codes: readonly string[]): void {
  if (typeof document === 'undefined' || typeof URL.createObjectURL !== 'function') return

  const blob = new Blob([recoveryCodesFile(codes)], { type: 'text/plain' })
  const url = URL.createObjectURL(blob)
  const anchor = document.createElement('a')
  anchor.href = url
  anchor.download = FILE_NAME
  anchor.click()
  URL.revokeObjectURL(url)
}
