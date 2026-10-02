import { setupWorker } from 'msw/browser'

import { handlers } from '@/mocks/handlers'

/**
 * The browser half of the fixture API, started from src/main.ts only in mock
 * mode. It is imported dynamically there so neither MSW nor the fixtures can
 * reach a production bundle.
 */
export const worker = setupWorker(...handlers)
