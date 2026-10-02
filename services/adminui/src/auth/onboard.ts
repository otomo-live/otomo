/**
 * The onboarding link's token, which arrives in the URL fragment.
 *
 * The fragment is the one part of a URL a browser does not send to a server, so
 * neither a proxy log nor a `Referer` header can capture it. That is the whole
 * reason the link is `…/admin/onboard#token=…` rather than a query parameter, and
 * why this module is separate from the view: the parsing and the erasing are the
 * part worth testing on their own.
 */

/**
 * The value of `key` in a location fragment, or null when it is absent or empty.
 *
 * `URLSearchParams` does the decoding, so a percent-encoded token and a plain one
 * read the same. A fragment with no `token` at all is null rather than an empty
 * string: the view has to show the invalid-link state, and an empty token is not
 * one to send to the server.
 */
export function readFragmentToken(hash: string, key = 'token'): string | null {
  const raw = hash.startsWith('#') ? hash.slice(1) : hash
  if (raw === '') return null
  const value = new URLSearchParams(raw).get(key)
  return value === null || value === '' ? null : value
}

/**
 * Erases the fragment from the address bar without a navigation and without
 * adding a history entry, so the back button cannot return to a URL that carries
 * the token. Called before the token is sent anywhere.
 */
export function stripFragment(
  target: Pick<Location, 'pathname' | 'search'>,
  history: Pick<History, 'state' | 'replaceState'>,
): void {
  history.replaceState(history.state, '', `${target.pathname}${target.search}`)
}
