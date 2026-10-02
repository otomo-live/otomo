# gateway_dev testing guide

## 1. Automated tests

From `services/gateway_dev`:

```powershell
go test -v ./...
go test -race ./...
go vet ./...
gofmt -l .
```

No test count is written down here on purpose. The suite is shared with
`services/gateway` and grows in both places; a number in this file goes stale the
first time someone adds a case.

The suite is not a subset of `services/gateway`'s. Both services carry the
full middleware and config suites, because the packages are copies rather than
imports (see the README), and the tests are what hold the two copies honest.
`services/gateway_dev/internal/authn/middleware_test.go` is where the
cross-domain cases live: they build their own two-domain JWKS registries to
prove a **player** token is rejected here even though this service is only ever
configured with the staff domain.

## 2. Manual testing

### Prerequisites

- Go 1.27.1
- A staff token, signed by a key whose public half is in the JWKS the service is
  pointed at. `run-dev.ps1` points at `http://localhost:9999/.well-known/staff-jwks.json`;
  serve a JWKS there, or edit the three `GATEWAY_STAFF_*` values to match whatever
  issuer you have. Without a valid token every protected route returns `401`, which
  is itself a useful thing to see.

### Step 1: Start the fake upstreams

In `services/gateway_dev/testdata/manual`:

```powershell
go run fake_upstream.go
```

This starts seven echo servers on 9001-9007. This service reaches five of them:
`session` 9003, `adminauth` 9004, `adminui` 9005, `config` 9006, `dashboard` 9007.

### Step 2: Start the gateway

Second terminal, same directory:

```powershell
.\run-dev.ps1
```

### Step 3: Check the routes

Third terminal. Public routes need no token:

```powershell
curl http://127.0.0.1:8090/admin-auth/login      # -> adminauth
curl http://127.0.0.1:8090/admin/dashboard       # -> adminui
```

Protected routes need a staff token. With `$T` set to one:

```powershell
curl -H "Authorization: Bearer $T" http://127.0.0.1:8090/api/admin/session/players
curl -H "Authorization: Bearer $T" http://127.0.0.1:8090/api/admin/dashboard/logs
```

What the role bar should do, with the same token:

| Request | `viewer` | `live_ops` | `admin` |
|---|---|---|---|
| `GET /api/admin/session/players` | 200 | 200 | 200 |
| `GET /api/admin/dashboard/logs` | 200 | 200 | 200 |
| `GET /api/admin/config/namespaces` | 200 | 200 | 200 |
| `POST /api/admin/config/channels/live/releases` | 403 | 403 | 200 |
| `GET /api/admin/users` | 403 | 403 | 200 |

`insufficient_role` (`403`) means the token was valid and correctly issued, just
under-privileged. Every `401` means the token itself is unusable, and the
`code` says why: `missing_token`, `invalid_signature`, `expired`, `aud_mismatch`,
`iss_mismatch`, `invalid_token`.

### Step 4: The negative checks, which are the point

A **player** token must not be accepted anywhere here. If you have one from
`services/gateway`'s harness:

```powershell
curl -i -H "Authorization: Bearer $PLAYER_TOKEN" http://127.0.0.1:8090/api/admin/session/players
```

Expect `401 invalid_signature`, not `403`: the player issuer's key is not in this
service's registry at all, so the signature fails before issuer or audience is ever
compared.

A **player path** must not exist here:

```powershell
curl -i http://127.0.0.1:8090/api/player/session/info   # COM-5 404
curl -i http://127.0.0.1:8090/auth/login                # COM-5 404
curl -i http://127.0.0.1:8090/patch/v1/live/manifest    # COM-5 404
```

That 404 is the de-conflation working. The instance-flag design could not produce
it: the admin table was in the same binary, so a misconfiguration served the player
paths instead of refusing them.

### Step 5: Listener isolation

```powershell
curl http://127.0.0.1:8091/healthz    # 200
curl http://127.0.0.1:8091/readyz     # 200 once the JWKS fetch has landed
curl http://127.0.0.1:8091/metrics
```

All of these must be `404` on 8090. `/readyz` stays `503` while the staff JWKS is
unreachable, which is the correct answer: without it the service cannot validate a
single protected request.

### What to look for

1. **Routing:** each response body names the upstream that served it.
2. **Request ID:** every response carries `X-Request-Id`, and the upstream echoes the
   same value.
3. **Access logs:** one JSON line per request with `method`, `route`, `status`,
   `duration` and `request_id`.
4. **Port isolation:** `/healthz`, `/readyz`, `/metrics` and `/debug/pprof/` are on
   the metrics port only.
5. **COM-5 shape:** unmatched paths return `{"error":{"code":"not_found",...}}`.
6. **Metrics:** `curl http://127.0.0.1:8091/metrics` shows
   `gateway_http_request_duration_seconds` and, after a rejection,
   `gateway_token_rejected_total{reason,group="staff"}`.

## 3. Smoke test (no Docker)

```bash
bash services/gateway_dev/testdata/smoke/smoke.sh
```

`testdata/smoke/smoke.sh` is the whole edge in one run: it builds this service from
the working tree, generates its own keys and mints its own tokens, starts a JWKS
endpoint, starts the echo upstreams, the streaming dashboard upstream and the
gateway, prints one line per check with the observed status and COM-5 code, and
stops everything again. It needs Go on `PATH` and nothing else: no Docker, no
database, no admin-auth. The ports are its own (public `18090`, metrics `18091`,
JWKS `18092`, streaming dashboard `18093`) so it does not collide with the manual
runner in §2; the echo upstreams are the same `testdata/manual/fake_upstream.go`, on
9001-9007.

What it holds, in order:

9. no token on `/api/admin/config/namespaces` → `401 missing_token`
10. a player token there → `401 invalid_signature`
11. a player token on `/api/admin/session/players` → `401`, explicitly **not** `403`
12. a staff token with no `roles` claim → `403 insufficient_role`, on the config
    prefix and on `/api/admin/dashboard/logs/tail` (`viewer` minimum)
13. the config prefix (`viewer` minimum; Config re-checks writes per route): `viewer`
    → 200, `live_ops` → 200, `admin` → 200
14. the ServeMux specificity case: `POST /api/admin/config/channels/live/releases`
    (its own `admin` minimum) → `viewer` 403, `live_ops` 403, `admin` 200, so it has
    not fallen through to the `viewer` prefix rule
    14b. `/api/admin/users` (`admin` minimum) → `viewer` 403, `live_ops` 403, `admin`
    200 on both the collection root and a path below it, with no redirect on the root
15. roles are ordinal: an `admin` token also passes a `viewer`-minimum route
16. every player path 404s, and both public routes still reach their upstreams
17. `/healthz`, `/readyz`, `/metrics`: 404 on the public port, 200 on the metrics port
18. the streaming route outlives `GATEWAY_DEV_WRITE_TIMEOUT` (`2s` here): the
    upstream emits one SSE chunk, holds 3s, emits a second, and **both** arrive
19. that no request id, player subject or staff subject appears in any `/metrics`
    label, with the label names printed so they can be read
20. that the series are `gateway_*` and no `gateway_dev_*` exists
21. one COM-5 body and one structured access log line, for eyeballing
    21b. the default `1 MiB` body cap: a `2 MiB` `POST /api/admin/session/players`
    with an admin token → `413 body_too_large`, before the upstream
    21c. the content-pack route: a `3 MiB` `POST /api/admin/config/packs` as
    `live_ops` → `200 upstream=config` (its own `512 MiB` cap), as `viewer` →
    `403 insufficient_role`
    21d. `SetForwarded`: `POST /admin-auth/login` with `X-Forwarded-For: 6.6.6.6`
    reports the peer only (`127.0.0.1`) and `X-Forwarded-Proto: http` from the
    echo upstream, so the client chain never reaches admin-auth

The echo upstreams now report `x_forwarded_for` and `x_forwarded_proto` in their
JSON alongside `upstream` and `request_id`, which is what check 21d reads.

Two helpers, both under `testdata/`, so `go build ./...` and `go vet ./...` neither
see them nor need them. `testdata/smoke/keyserver/` generates both Ed25519 keypairs
at startup, serves the two JWKS documents, and writes the five tokens the script
presents; it imports this service's own `internal/authn.Claims`, so a token it mints
is the exact production payload shape rather than a lookalike. It serves the player
document too, so the script can prove the point this service exists for: that a
player token is rejected here. `testdata/smoke/sseupstream/` is the dashboard
upstream for check 18; every path but the streaming one echoes the same JSON as
`fake_upstream.go`.
