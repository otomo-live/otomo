import { setupServer } from 'msw/node'

import { handlers } from '@/mocks/handlers'

/**
 * The Node half of the same handler set, used by Vitest through
 * tests/unit/setup/msw.ts. It is the browser worker's twin on purpose: a handler
 * that works in a test and not in the browser would be worse than no mock.
 */
export const server = setupServer(...handlers)
