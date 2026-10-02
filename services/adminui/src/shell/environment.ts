/**
 * The environment banner, derived from `/admin/env.json`'s `environment`.
 *
 * One image serves dev, staging and live (see `src/api/env.ts`), so the shell
 * has to say which one is on screen: an operator who cannot tell live from
 * staging is one save away from a mistake that reaches players.
 *
 * This is a plain module rather than a computed inside the shell so the text and
 * role per environment can be asserted without mounting Ionic. The colours are
 * never named here; the kind maps to a `--ds-*` role in the shell's stylesheet.
 */

export type EnvironmentKind = 'danger' | 'warn' | 'info'

export interface EnvironmentBanner {
  kind: EnvironmentKind
  text: string
}

export function environmentBanner(environment: string): EnvironmentBanner {
  if (environment === 'live') {
    return { kind: 'danger', text: 'LIVE — changes reach players' }
  }
  if (environment === 'staging') {
    return { kind: 'warn', text: 'STAGING — changes do not reach players' }
  }
  return { kind: 'info', text: `${environment} — not live` }
}
