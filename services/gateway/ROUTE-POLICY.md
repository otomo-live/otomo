# Route policy (GATE-5): player gateway

This sheet records the route policy and this service's route table as
implemented. `route_policy_test.go` keeps the table matching the policy. The
admin and dev table is `services/gateway_dev` and is not in this
binary: every admin path is a 404 here.

## Policy (techspec §9)

The policy is per route, not per service, so the full list is kept here.

| Group | Prefixes |
|---|---|
| Public | `/auth/*`, `/admin-auth/*`, `/admin/*` (static assets only), `/patch/v1/live/*`, `/patch/v1/blob/*` |
| Player | `/api/player/session/*` |
| Staff | `/api/admin/*`, `/patch/v1/dev/*`, `/patch/v1/staging/*` |
| Never routed | `/.well-known/jwks.json` (Auth and PHP JWKS are fetched east-west, never proxied) |

## Routes as implemented

`services/gateway` (`internal/router/player.go`, `BuildRoutes`):

| Method | Pattern | Upstream | Group | MinRole | Stream | Rate limit |
|---|---|---|---|---|---|---|
| * | `/auth/` | auth | Public | - | | login (5 rps / burst 10) |
| GET | `/patch/v1/live/manifest` | patch | Public | - | | general |
| GET | `/patch/v1/blob/` | patch | Public | - | yes | general |
| GET | `/patch/v1/dev/manifest` | patch | Staff | any staff | | general |
| GET | `/patch/v1/staging/manifest` | patch | Staff | any staff | | general |
| * | `/api/player/session/events` | session | Player | - | yes | general |
| * | `/api/player/session/` | session | Player | - | | general |

The general limit is 20 rps / burst 40 per client IP (`GATEWAY_RATE_LIMIT_*`).
The login limit is `GATEWAY_LOGIN_RATE_LIMIT_*` (techspec §6.4).

Why two staff routes are here: the dev and staging manifests carry unshipped
builds, so they need a staff token, but they are Patch traffic and stay on
the player gateway. That is why this service loads the staff JWKS as well.

## What the tests hold

- Every route matches exactly one §9 prefix and has that prefix's group.
- No public route under `/api/`, and no public or player route with a
  `MinRole`.
- Every route, tried with no token, a player token and a staff token, is
  either served (right domain or public) or a COM-5 401 that never reaches
  the upstream.
- A player token never reaches `/api/admin/*` or `/patch/v1/{dev,staging}/*`,
  and a staff token never reaches `/api/player/*`, including unregistered
  paths under those prefixes.
- Every admin path and every `/.well-known/` path is a COM-5 404.

## CORS

Not on this service. Techspec §3 applies `GATEWAY_CORS_ALLOWED_ORIGINS` to
`gateway_dev` only, because the Godot client is not a browser.

## Open points

1. `/patch/v1/{dev,staging}` carry manifest routes only, no blob route.
   Common-stack §1 says prefix, techspec §5.2 lists two exact paths. Awaiting
   the tech lead; no route added.
