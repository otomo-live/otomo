import { HttpResponse, http } from 'msw'
import { beforeEach, describe, expect, it } from 'vitest'

import { ENV_CONFIG_PATH, EnvConfigError, envConfig, loadEnvConfig } from '@/api/env'
import { server } from '@/mocks/server'

/**
 * The runtime configuration's two promises: unknown and wrongly typed values in
 * env.json are survivable, and a missing file is not.
 *
 * The path MSW matches on is a wildcard because `ENV_CONFIG_PATH` comes from
 * Vite's base, which is `/admin/` under `vite` and `/` under Vitest. That is the
 * kind of difference this file exists to make visible rather than to hide.
 */

const ANY_ENV_JSON = '*/env.json'

beforeEach(async () => {
  // The fixture API remembers the last login in module state, which is what makes
  // the refresh path testable at all; a logout is how one test stops deciding for
  // the next. src/mocks/handlers.ts has the detail.
  await fetch('/admin-auth/logout', { method: 'POST' })
})

describe('the runtime configuration', () => {
  it('reads the base URL, the title and the auth paths', async () => {
    server.use(
      http.get(ANY_ENV_JSON, () =>
        HttpResponse.json({
          apiBaseUrl: 'https://api.example',
          appTitle: 'Otomo Admin (staging)',
          environment: 'staging',
          auth: { loginPath: '/custom/login' },
        }),
      ),
    )

    const loaded = await loadEnvConfig()

    expect(loaded.apiBaseUrl).toBe('https://api.example')
    expect(loaded.appTitle).toBe('Otomo Admin (staging)')
    expect(loaded.environment).toBe('staging')
    expect(loaded.auth.loginPath).toBe('/custom/login')
    // The keys the file omits keep the defaults, which are the production paths.
    expect(loaded.auth.refreshPath).toBe('/admin-auth/refresh')
    expect(loaded.auth.mfaPath).toBe('/admin-auth/mfa/verify')
    // And the module hands out the same object the loader returned.
    expect(envConfig()).toBe(loaded)
  })

  it('ignores unknown keys and refuses a wrongly typed value', async () => {
    server.use(
      http.get(ANY_ENV_JSON, () =>
        HttpResponse.json({
          // A number where a string belongs: a newer or hand-edited file, which
          // must not become a base URL of 42.
          apiBaseUrl: 42,
          appTitle: 'Otomo Admin',
          somethingFromTheFuture: { anything: true },
        }),
      ),
    )

    const loaded = await loadEnvConfig()

    expect(loaded.apiBaseUrl).toBe('')
    expect(loaded.appTitle).toBe('Otomo Admin')
  })

  it('fails loudly when the file is missing', async () => {
    server.use(http.get(ANY_ENV_JSON, () => new HttpResponse('not found', { status: 404 })))

    const error = await loadEnvConfig().catch((caught: unknown) => caught)

    expect(error).toBeInstanceOf(EnvConfigError)
    // The path, not the file: a deployment that serves the app at the wrong
    // prefix is the likelier cause than a missing file, and the message says so.
    expect((error as Error).message).toContain(ENV_CONFIG_PATH)
  })

  it('fails loudly when the file is not JSON', async () => {
    // What a static server's SPA fallback answers for a path that does not
    // exist: index.html with a 200. A silent default here would present as a
    // mystery 404 against the API instead.
    server.use(
      http.get(
        ANY_ENV_JSON,
        () =>
          new HttpResponse('<html>index</html>', {
            status: 200,
            headers: { 'Content-Type': 'text/html' },
          }),
      ),
    )

    const error = await loadEnvConfig().catch((caught: unknown) => caught)

    expect(error).toBeInstanceOf(EnvConfigError)
    expect((error as Error).message).toContain('not valid JSON')
  })
})
