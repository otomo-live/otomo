/**
 * The runtime configuration, read from /admin/env.json (WEB-2).
 *
 * This file is what lets one image serve dev, staging and live: nothing about
 * where the API lives is baked into the bundle, so a deployment swaps this one
 * file rather than rebuilding. In production `apiBaseUrl` is the empty string,
 * because the SPA and the API are both served by gateway_dev on one origin.
 *
 * Two behaviours to know before changing it:
 *
 *  - Unknown keys are ignored, and a wrongly typed value falls back to its
 *    default, so a newer env.json does not break an older bundle.
 *  - A MISSING or unparseable file is fatal and loud. The defaults below are the
 *    production values, and production is exactly the deployment least likely to
 *    notice a silent fallback: an empty API base URL is right there and wrong
 *    almost everywhere else, so it fails as a mystery 404 instead of as a
 *    misconfigured image.
 *
 * The module holds one configuration and hands it out through `envConfig()`. It
 * starts at the defaults rather than at `null` so that a unit test can exercise
 * the auth client without loading a file first, and so that a boot-time failure
 * leaves something coherent in place.
 */

export interface EnvAuthPaths {
  /** Where the SPA POSTs an email and password. */
  loginPath: string
  /** Where the signed-in account's status is read from. */
  mePath: string
  /** Where it redeems the refresh cookie. */
  refreshPath: string
  logoutPath: string
  /** Where it submits the second-factor code. */
  mfaPath: string
  /** The onboarding pair: lookup then redeem the link token. */
  onboardLookupPath: string
  onboardRedeemPath: string
  /** TOTP enrollment: fetch a secret, then confirm a code. */
  mfaEnrollPath: string
  mfaConfirmPath: string
}

export interface EnvConfig {
  /**
   * Empty means same origin. A cross-origin value would need the gateway's
   * GATEWAY_DEV_CORS_ALLOWED_ORIGINS to be both configured and implemented
   * (services/gateway_dev/.env.example:37 parses it; nothing reads it), so the
   * supported deployment is same-origin and this stays empty there.
   */
  apiBaseUrl: string
  appTitle: string
  environment: string
  auth: EnvAuthPaths
}

const DEFAULTS: EnvConfig = {
  apiBaseUrl: '',
  appTitle: 'Otomo Admin',
  environment: 'live',
  auth: {
    loginPath: '/admin-auth/login',
    mePath: '/admin-auth/me',
    refreshPath: '/admin-auth/refresh',
    logoutPath: '/admin-auth/logout',
    mfaPath: '/admin-auth/mfa/verify',
    onboardLookupPath: '/admin-auth/onboard/lookup',
    onboardRedeemPath: '/admin-auth/onboard',
    mfaEnrollPath: '/admin-auth/mfa/enroll',
    mfaConfirmPath: '/admin-auth/mfa/confirm',
  },
}

/** Where the file lives. Vite's base ends in a slash, so this is '/admin/env.json'. */
export const ENV_CONFIG_PATH = `${import.meta.env.BASE_URL}env.json`

export class EnvConfigError extends Error {
  constructor(message: string, cause?: unknown) {
    super(message, cause === undefined ? undefined : { cause })
    this.name = 'EnvConfigError'
  }
}

let current: EnvConfig = DEFAULTS

export function envConfig(): EnvConfig {
  return current
}

/**
 * An empty string counts as absent, which is why `apiBaseUrl`'s default is the
 * empty string too: the one value that must be expressible is also the one that
 * looks like "not set".
 */
function readString(source: Record<string, unknown>, key: string, fallback: string): string {
  const value = source[key]
  return typeof value === 'string' && value.length > 0 ? value : fallback
}

function readAuthPaths(source: unknown, fallback: EnvAuthPaths): EnvAuthPaths {
  if (typeof source !== 'object' || source === null) return fallback
  const record = source as Record<string, unknown>
  return {
    loginPath: readString(record, 'loginPath', fallback.loginPath),
    mePath: readString(record, 'mePath', fallback.mePath),
    refreshPath: readString(record, 'refreshPath', fallback.refreshPath),
    logoutPath: readString(record, 'logoutPath', fallback.logoutPath),
    mfaPath: readString(record, 'mfaPath', fallback.mfaPath),
    onboardLookupPath: readString(record, 'onboardLookupPath', fallback.onboardLookupPath),
    onboardRedeemPath: readString(record, 'onboardRedeemPath', fallback.onboardRedeemPath),
    mfaEnrollPath: readString(record, 'mfaEnrollPath', fallback.mfaEnrollPath),
    mfaConfirmPath: readString(record, 'mfaConfirmPath', fallback.mfaConfirmPath),
  }
}

export async function loadEnvConfig(): Promise<EnvConfig> {
  let response: Response
  try {
    response = await fetch(ENV_CONFIG_PATH, {
      // WEB-6 has nginx serve this no-cache, and the client does not rely on it:
      // a cached env.json outlives the deployment it described and is very hard
      // to see, so asking for a fresh one costs nothing here.
      cache: 'no-store',
      credentials: 'same-origin',
    })
  } catch (cause) {
    throw new EnvConfigError(`Could not reach ${ENV_CONFIG_PATH}.`, cause)
  }

  if (!response.ok) {
    throw new EnvConfigError(
      `${ENV_CONFIG_PATH} returned HTTP ${response.status}. Every image ships a default at public/env.json, so a 404 here means the path is wrong rather than the file missing: the app is served under /admin/, and env.json lives under it.`,
    )
  }

  let body: unknown
  try {
    body = await response.json()
  } catch (cause) {
    throw new EnvConfigError(
      `${ENV_CONFIG_PATH} is not valid JSON. It is served as a static file and must not be an HTML fallback: a missing env.json that gets index.html instead is exactly the failure this check exists for.`,
      cause,
    )
  }

  if (typeof body !== 'object' || body === null) {
    throw new EnvConfigError(`${ENV_CONFIG_PATH} is not a JSON object.`)
  }

  const record = body as Record<string, unknown>
  current = {
    apiBaseUrl: readString(record, 'apiBaseUrl', DEFAULTS.apiBaseUrl),
    appTitle: readString(record, 'appTitle', DEFAULTS.appTitle),
    environment: readString(record, 'environment', DEFAULTS.environment),
    auth: readAuthPaths(record.auth, DEFAULTS.auth),
  }
  return current
}
