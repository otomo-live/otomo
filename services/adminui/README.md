# adminui

The Config and Dashboard WebUI: an Ionic + Vue single-page app served at `/admin/`.

It is the frontend for two backends that already exist as skeletons, `services/config`
(authoring: namespaces, drafts, versions, releases, audit) and `services/dashboard`
(observability: overview, time series, log tail, merged audit). It reaches them through
`gateway_dev`, the staff gateway, never through the player gateway.

## Why it is shaped this way

Three constraints drive almost every decision here, and none of them are obvious from the
file tree.

**The `/admin/` prefix is not stripped.** `gateway_dev`'s routes all leave `StripPrefix`
empty, so a request for `/admin/dashboard` reaches this container still carrying
`/admin/`. Vite's `base` is therefore `/admin/`, the router's history base is
`import.meta.env.BASE_URL`, nginx serves the app _at_ `/admin/`, and `env.json` lives at
`/admin/env.json`. Getting any of those four wrong produces a blank page on a deep link.

**Everything is same-origin.** The SPA and its API are both served through `gateway_dev`,
so the API base URL is the empty string in production and CORS is not on the critical
path. Local development uses Vite's `server.proxy` rather than cross-origin fetches, which
keeps it that way. `GATEWAY_DEV_CORS_ALLOWED_ORIGINS` is parsed by the gateway but read by
nothing, so a cross-origin design is not implementable today in any case.

**This app is product and game agnostic, and that is load bearing.** Nothing in the SPA
knows any game's master tables or schemas. A Config namespace is arbitrary JSON plus its
own JSON Schema; the editor is built from the schema, and the optional presentation
metadata is data a project authors, never code in this repo.

## Status

The shell, the build, the transport and the sign-in path. The routes, the app shell and
the fixture API exist; `src/api/client.ts` is the only place the SPA talks HTTP, and it
carries the COM-5 error shape, the in-memory bearer token and the single-flight refresh
after an expiry, all covered by unit tests. `src/stores/session.ts` holds the staff
session and registers that refresh with the client.

`src/auth/guard.ts` is the whole navigation policy, and there is no second copy of it: an
anonymous visitor is sent to `/login` carrying the path they asked for, a signed-in visitor
whose roles do not reach a route is sent to `/denied` rather than somewhere they will be
refused again, and the shell's navigation is derived from the same route table, so the menu
and the guard cannot disagree about who may see what.

Every screen is real. Sign-in, the second factor and `/denied` carry the reference id a failure
shows and the role ladder in `src/auth/roles.ts` that mirrors `gateway_dev`'s; the Config side is a
namespace list and a schema-driven editor with a raw-JSON tab and the 409 conflict dialog; the
Dashboard side is the overview, the log tail and the merged audit. Views live in `src/views/<area>/`
and the modules they share in `src/config/`. See `design/00-common-stack.md` WEB-1 to WEB-8 for the
deliverable set and this service's commit history for the order it landed in.

The editor's form renderer is hand-rolled over the schema subset the namespaces use
(`src/config/controls.ts`), with the layout coming from the schema plus the optional
`ui.presentation` envelope (`src/config/presentation.ts`). `design/02-config.md` names JSON Forms and
jsondiffpatch for CFG-D2 and CFG-D5, the deep module tickets: neither package is a dependency here,
and `src/config/diff.ts` records why this service diffs documents itself. An envelope can never make
a field unreachable: a field it names that the schema lacks is ignored, and a field the schema has
that it names nowhere is appended to a trailing group.

Four things are worth knowing before reading the code:

- **`/admin/env.json` is loaded before the app mounts**, and a missing or malformed file
  renders a readable failure panel rather than a blank page. The shipped `public/env.json`
  is the default a deployment may overwrite.
- **The auth calls are coded against the proposal in `design/06-auth-identity-contract.md`,
  not against a service that exists.** PHP Admin Auth is out of this repository. If the
  real endpoints disagree, `src/api/auth.ts` changes and nothing else does.
- **Mock mode remembers a session across a page reload** (in `sessionStorage`), because
  without that a reload is indistinguishable from being signed out and the guard's restore
  path cannot be tested at all. The real server keeps the refresh token in an httpOnly
  cookie that a service worker cannot set; the note in `src/mocks/handlers.ts` explains the
  substitution.
- **The tests are unit tests over the pure modules, plus one Playwright smoke of five
  cases.** Nothing renders a component in isolation, so `ConflictDialog.vue` and
  `SchemaField.vue` are covered only through what drives them.

## Running it

Requires Node 24 (`.nvmrc` pins it).

```
npm ci
npm run dev          # http://localhost:5173/admin/
npm run dev:mock     # the same, against the fixture API instead of a gateway
npm run build        # vite build, emitting dist/
npm run lint
npm run typecheck
npm run test         # Vitest
npm run e2e          # Playwright, against its own dev:mock on port 5199
```

`npx playwright install chromium` is needed once before the first `npm run e2e`; the
browsers are not a dependency and are not installed by `npm ci`.

`npm run dev` proxies `/api` and `/admin-auth` to `VITE_DEV_PROXY_TARGET`, defaulting to
`http://127.0.0.1:8090`, which is where `services/gateway_dev` listens in the manual test
setup (`services/gateway_dev/testdata/manual/TESTING.md`).

## Image

`dockerfile` builds `dist/` on `node:24-bookworm-slim` and serves it from
`nginxinc/nginx-unprivileged:1.29-alpine`: the non-root variant, which is why it listens on
**8080**, the same container port `ci/services/Jenkinsfile` maps `${PORT}:8080` to for every
service, so this one needs no port parameter of its own. There is no Node in the final
image.

```
docker build -t otomo-adminui:staging services/adminui
docker run --rm -p 5010:8080 otomo-adminui:staging  # http://localhost:5010/admin/
```

`nginx.conf` is a server block, not a whole config. It is dropped into
`/etc/nginx/conf.d/` so the base image's own non-root settings (its user, its pid file
under `/tmp`) survive, and the base image's `default.conf` is deleted in the same step,
because it listens on 8080 as well and which of the two nginx treats as the default server
would otherwise decide whether a request reaches this app or the welcome page.

Three things in it follow from `gateway_dev` not stripping the `/admin/` prefix
(`internal/router/dev.go` sets no `StripPrefix`, and `internal/proxy/proxy.go` only
rewrites a non-empty one), so the app really is served _at_ `/admin/`, exactly matching
Vite's `base`:

- WEB-6's literal `try_files $uri /index.html` is written as `/admin/index.html`; at the
  origin root that fallback would not resolve;
- `/admin/assets/` is cached for a year and marked `immutable` (every filename in it is
  content-hashed), while `/admin/index.html` and `/admin/env.json` are `no-cache`: their
  names never change, and a cached `env.json` is how a deployment's override gets ignored;
- the origin root answers 404 rather than serving the app, so the bundle is reachable at
  exactly one URL.

A deployment may replace `/usr/share/nginx/html/admin/env.json` to point the same image at
a different API, which is what makes one image serve dev, staging and live (WEB-2).

**What has been run.** There is no Docker on the machine this service was written on, so the
image was built and run on a second one (Docker 29.8.1, 2026-09-22). Everything above was
checked there:

- `docker build` succeeds. The image is built from `nginxinc/nginx-unprivileged`, declares
  `User=101` and the base image's `docker-entrypoint.sh`, and contains no Node:
  `command -v node` in the runtime image finds nothing;
- the container runs as uid 101, and `/etc/nginx/conf.d` holds `adminui.conf` alone, so the
  base image's `default.conf` is not a second server block answering 8080;
- `/admin/` is 200 with `no-cache`, a deep link (`/admin/dashboard`, `/admin/config/namespaces`)
  is 200 with `index.html` and that same `no-cache`, `/admin` redirects, and the origin root
  is 404;
- `/admin/env.json` is 200 with `no-cache` and the shipped default body, the hashed assets are
  200 with exactly one `Cache-Control: public, max-age=31536000, immutable`, and a missing
  path under `/admin/assets/` is 404 rather than a fallback;
- a JS asset comes back gzipped, the `Server` header carries no version, and `SIGTERM` exits 0.

The same checks were then run with the container as the `adminui` upstream of a real
`gateway_dev`: the bundle, `env.json` and both cache headers survive the proxy hop, a staff
route with no token returns the COM-5 `missing_token` envelope with a request id, and a
`Set-Cookie` from an `/admin-auth/` upstream arrives intact with `HttpOnly`, `Secure`,
`SameSite=Strict` and `Path` unchanged (the cookie is now `__Host-otomo_refresh`, `Path=/`;
see doc 06 §13.2). That last one is the assumption the refresh design in
`src/api/client.ts` rests on.

The run also turned up one thing worth knowing, with no change needed. **A bare `/admin` never
reaches nginx in production.** `nginx.conf`'s `return 301 /admin/` is absolute and names
nginx's own listen port, so a direct request gets `Location: http://<host>:8080/admin/`, which
would be the wrong scheme and port behind the gateway. The browser never sees it: the
gateway's own `ServeMux` answers the prefix redirect first, with a relative `Location:
/admin/`. The directive is therefore a correct guard for anyone reaching the container
directly, and dead code in the deployed topology.

## Security headers

`security-headers.conf` is included at the server block and again in every location that sets an
`add_header` of its own, because nginx drops inherited headers in exactly that case; the policy test
in `tests/unit/nginx-headers.spec.ts` fails if a future location forgets it. Every header is sent
with `always`, so 404s and redirects carry them too.

The Content-Security-Policy is `default-src 'self'` with `script-src 'self'` and no `'unsafe-eval'`,
which is why local schema validation uses an interpreter rather than a compiler (see
`src/config/validation.ts`). `style-src` additionally allows `'unsafe-inline'`: Ionic and CodeMirror
inject `<style>` elements at runtime, inline styles cannot run code, and `script-src` stays strict.
`img-src` allows `data:` for the TOTP enrollment QR code, which is a PNG data URL, and for Ionic's
inline-SVG icons. `connect-src` is `'self'` because the API is same-origin through `gateway_dev`; a
deployment that sets `env.json`'s `apiBaseUrl` to another origin must add that origin to
`connect-src`. The snippet also sets `X-Frame-Options: DENY` and `frame-ancestors 'none'`,
`X-Content-Type-Options: nosniff`, and `Referrer-Policy: same-origin`.

Check them on a running container (or through `gateway_dev`) with:

```
curl -I http://localhost:5010/admin/
curl -I http://localhost:5010/admin/assets/<hashed-asset>.js
```

## Notes for the reader

**`release` is overloaded.** A Config release is an immutable built artefact of a
namespace. Patch also has a release concept. They are different things sharing a word;
this app only ever means the Config one.

**Role gating in the SPA is UX only.** `meta.minRole` on a route hides a control a role
cannot use. It is not an authorization check and must never be treated as one: the gateway
refuses the request regardless of what the browser renders.

**A 401 and a 403 mean different things.** `missing_token`, `invalid_signature`,
`expired`, `aud_mismatch`, `iss_mismatch` and `invalid_token` are 401 and mean the token is
unusable, so replacing it may help. `insufficient_role` is 403 and means the token is fine
and under-privileged, so refreshing it is pointless. The two must not be handled the same
way.
