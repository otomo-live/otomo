/// <reference types="vite/client" />

declare module '*.vue' {
  import type { DefineComponent } from 'vue'

  const component: DefineComponent<Record<string, unknown>, Record<string, unknown>, unknown>
  export default component
}

interface ImportMetaEnv {
  /** Where `npm run dev` proxies /api and /admin-auth. Empty is not valid here. */
  readonly VITE_DEV_PROXY_TARGET?: string
  /** '1' serves the fixture API from MSW instead of proxying to a gateway. */
  readonly VITE_ENABLE_MOCK?: string
}

interface ImportMeta {
  readonly env: ImportMetaEnv
}
