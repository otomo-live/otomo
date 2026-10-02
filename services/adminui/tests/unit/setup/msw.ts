import { afterAll, afterEach, beforeAll } from 'vitest'

import { server } from '@/mocks/server'

/**
 * Started for every unit suite, not just the mock's own: the API client, the
 * refresh coordinator and the stores are all written against this fixture API,
 * and a suite that wants different answers registers its own handler on top
 * (server.use) rather than reaching for the network.
 */
beforeAll(() => {
  // 'error' rather than the default 'warn': an unmocked request is a test that
  // believes it is exercising a fixture and is not. It fails the test instead of
  // quietly going out to localhost or the internet.
  server.listen({ onUnhandledRequest: 'error' })
})

afterEach(() => {
  // Handlers added inside one test (server.use) must not leak into the next.
  server.resetHandlers()
})

afterAll(() => {
  server.close()
})
