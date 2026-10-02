# Route policy: admin/dev gateway

This is the admin-edge counterpart of `services/gateway/ROUTE-POLICY.md`
(GATE-5). The policy is the admin half of techspec §9; the player
half lives on the other binary, which is the whole reason this service exists. `route_policy_test.go` keeps the table in
`internal/router/dev.go` matching this sheet.

## Policy (techspec §9)

The policy is per route, not per service. On this edge the full list is:

| Group | Prefixes |
|---|---|
| Public | `/admin-auth/*` (the staff login/refresh/logout surface), `/admin/*` (static SPA assets only) |
| Staff | `/api/admin/*` |
| Never routed | `/api/player/*`, `/auth/*`, `/patch/v1/live/*` (player paths, answered 404 here), `/.well-known/jwks.json` (Auth and PHP JWKS are fetched east-west, never proxied) |

Every protected route here is `GroupStaff`: verified against admin-auth's JWKS,
issuer and audience, never against the player Auth service. `GroupPlayer`
survives only as a constant for the cross-domain middleware tests; no route
uses it.

Role bars are the coarse edge check; every upstream re-checks per route.
`/api/admin/config/` is gated at `viewer` because Config's own table is the
finer one (reads viewer, writes live_ops or admin); a stricter edge would 403 a
viewer before Config ever saw the read. The one Config route the edge holds
higher is the live publish, the boundary between rehearsal and what players
receive.

## Routes as implemented

`services/gateway_dev` (`internal/router/dev.go`, `BuildRoutes`):

| Method | Pattern | Upstream | Group | MinRole | Stream | Rate limit | Body cap | Upload | SetForwarded | ForwardCookies |
|---|---|---|---|---|---|---|---|---|---|---|
| * | `/admin-auth/` | adminauth | Public | - | | login | 1 MiB | | yes | yes |
| * | `/admin/` | adminui | Public | - | | none | 1 MiB | | | |
| * | `/api/admin/config/` | config | Staff | viewer | | general | 1 MiB | | | |
| POST | `/api/admin/config/channels/live/releases` | config | Staff | admin | | general | 1 MiB | | | |
| POST | `/api/admin/config/packs` | config | Staff | live_ops | | general | 512 MiB | yes | | |
| * | `/api/admin/dashboard/logs/tail` | dashboard | Staff | viewer | yes | general | 1 MiB | | | |
| * | `/api/admin/dashboard/` | dashboard | Staff | viewer | | general | 1 MiB | | | |
| * | `/api/admin/session/` | session | Staff | viewer | | general | 1 MiB | | | |
| * | `/api/admin/users` | adminauth | Staff | admin | | general | 1 MiB | | yes | yes |
| * | `/api/admin/users/` | adminauth | Staff | admin | | general | 1 MiB | | yes | yes |

Rate limits (techspec §6.4): the general limit is 20 rps / burst 40 per client
IP (`GATEWAY_DEV_RATE_LIMIT_*`); the login limit is 5 rps / burst 10
(`GATEWAY_DEV_LOGIN_RATE_LIMIT_*`). `/admin/` is unlimited because one SPA page
load fetches dozens of hashed assets at once, and a per-IP bucket there breaks
the UI rather than stopping abuse; the catch-all 404 is unlimited too.

The body cap defaults to 1 MiB (`router.DefaultMaxBody`) and is 512 MiB on the
content-pack upload only. `Upload` clears both server-wide deadlines on the
packs route because 512 MiB cannot fit in the read or write timeout.

`SetForwarded` is set on exactly the admin-auth-served routes, because
admin-auth rate-limits and logs per client and must not trust a client-built
`X-Forwarded-For` chain. It drops `X-Forwarded-For`, `Forwarded` and
`X-Real-Ip` and records the peer IP and scheme instead.

`ForwardCookies` is also set on exactly the admin-auth-served routes.
Every other route drops the `Cookie` header before proxying. admin-auth's refresh
cookie is `__Host-otomo_refresh` with `Path=/`, so browsers send it on every staff
request, and it has to reach admin-auth alone. The parent domain is shared with
other teams, so foreign parent-domain cookies arrive too, and they reach no upstream.

The `/api/admin/users` collection root is registered alongside the subtree: with
the subtree alone, ServeMux would answer the collection itself with a 307 before
any auth ran, confirming the route to an anonymous caller.

## What the tests hold

`route_policy_test.go`, package `main`:

- `TestRoutePolicy_MatchesSpec`: every route's Method, Pattern, Upstream, Group,
  MinRole, Stream, SetForwarded, MaxBody and Upload equals the literal table in
  the test. Any table change must edit that test.
- `TestRoutePolicy_FieldsConsistentWithGroup`: public routes have `MinRole` 0,
  staff routes are at least `viewer`, and no `GroupPlayer` route exists.
- `TestRoutePolicy_FailClosedMatrix`: every staff route × {none, garbage,
  expired staff, player-domain token, staff with no roles, viewer, live_ops,
  admin} through `buildPublicMux` and a stub upstream. The first four are 401
  (never 403, never 2xx); no-roles is 403; role tokens are 2xx iff role ≥
  MinRole, else 403. A rejection never reaches the upstream.
- `TestRoutePolicy_PlayerPathsNotServed`: `/api/player/session/me`,
  `/auth/anonymous` and `/patch/v1/live/manifest` are COM-5 404s.
- `TestRoutePolicy_JWKSEndpointsNeverProxied`: no route mentions `.well-known`,
  and `/.well-known/jwks.json` is a COM-5 404 that never reaches an upstream.

`ratelimit_test.go` holds the rate-limit guards (login pattern exists, `/admin/`
is unlimited, login bucket is stricter than general). `gateway_dev_test.go`
holds the body-cap, forwarding and ServeMux-specificity cases.

## Open points

1. `/api/admin/config/packs` is the only upload route; if Config ever adds a
   second content-pack path, the edge must add a matching route or that upload
   will be cut off at 1 MiB.
