# Admin-auth

Staff identity service. Owns staff accounts, the Ed25519 staff signing keys,
access-token issuance, and the JWKS that Gateway and every other service verify staff
tokens against. Tokens carry issuer `https://admin-auth.otomo.internal` and audience
`otomo:staff`.

**What exists today:** config, the two HTTP listeners, structured logging, metrics,
graceful shutdown, the COM-5 error shape, request IDs, panic recovery, a cached
Postgres readiness probe, the AA-2 staff schema in the embedded migration
`00002_staff_schema.sql` (`00001` is the AA-1 no-op), the AA-3 signing material:
Ed25519 key generation (`genkey`), a cached JWKS published at
`GET /.well-known/jwks.json`, and staff access-token issuance. It also owns the
break-glass root account: `bootstrap-root` creates or rotates the single password-only
staff row that exists before any admin and whose only job is creating personal admin
accounts. The staff session endpoints (`/admin-auth/login`, `/admin-auth/refresh`,
`/admin-auth/logout`, `/admin-auth/me`, `/admin-auth/audit`), the public onboarding endpoints
(`/admin-auth/onboard/lookup`, `/admin-auth/onboard`), the MFA endpoints
(`/admin-auth/mfa/verify`, `/admin-auth/mfa/enroll`, `/admin-auth/mfa/confirm`; MFA
schema in `00003_mfa.sql`) and the user-management API
(`/api/admin/users*`) are live too. See "Scope" at the bottom.

---

## Running it

Key generation and migrations are separate one-shot steps. The service never generates
a key at startup: doing so would invalidate every outstanding token on every restart.

```sh
export ADMIN_AUTH_DATABASE_URL='postgres://admin_auth_rw:pw@localhost:5432/admin_auth'
export ADMIN_AUTH_SIGNING_KEY_PATH=./admin_auth_signing_key.pem
export ADMIN_AUTH_TOTP_KEY_PATH=./admin_auth_totp.key

go run . migrate                             # applies the staff schema
go run . genkey                              # writes the key, inserts the signing_key row
go run . genkey --totp "$ADMIN_AUTH_TOTP_KEY_PATH"   # writes the MFA sealing key (0600)
go run . bootstrap-root                      # creates the break-glass root from the password secret
go run . serve                               # or just: go run .
```

`bootstrap-root` needs a root password file (see "Root account" below) mounted at
`ADMIN_AUTH_ROOT_PASSWORD_FILE`. It needs only the database URL, not the signing key.

`genkey` refuses to overwrite an existing `-out` file. If the database insert fails it
deletes the file it just wrote, so you can rerun with the same `-kid` — but it never
touches a pre-existing file. With `--totp <path>` it does no database work at all: it
writes 32 random bytes as a base64 line with mode `0600` and refuses to overwrite.

The same commands work in the container, because the entrypoint is the binary:

```sh
mkdir -p keys && chown 65532 keys        # the image runs as uid 65532 and must be able to write here
docker run --rm --env-file admin_auth.env -v "$PWD/keys:/keys" otomo-admin-auth:staging genkey -out /keys/admin_auth.pem
docker run --rm --env-file admin_auth.env otomo-admin-auth:staging migrate
docker run --rm --env-file admin_auth.env -p 8080:8080 otomo-admin-auth:staging serve
```

### Commands

| Command | Does |
|---|---|
| `serve` (default) | Runs both listeners until `SIGTERM`/`SIGINT`, then shuts down gracefully. |
| `migrate` | Applies the embedded goose migrations. Idempotent. |
| `genkey` | Generates an Ed25519 key, writes it as PKCS#8 PEM (`0600`), inserts the `signing_key` row. Flags: `-kid`, `-out`, `-totp`. `-totp <path>` instead generates the 32-byte base64 MFA key (`0600`) and exits without touching the database. |
| `bootstrap-root` | Creates, or with `-rotate` rotates, the single break-glass root account. Flags: `-email` (default `root@otomo.internal`), `-name` (default `Root`), `-rotate`. Needs only `ADMIN_AUTH_DATABASE_URL`. |

Unknown commands print usage and exit `2`. A configuration error prints **every**
problem at once and exits `1`.

---

## Root account

There is exactly one root staff row (`staff_user.is_root = true`): a
password-only break-glass account with role `admin` and `created_by = NULL`. It has no
TOTP, is never challenged for one, and may not enroll (`403` on the MFA endpoints). Its
only job is creating personal admin accounts; day-to-day work happens in
those accounts, so the root password can stay in a host secret that admins read over
SSH.

```sh
printf '%s\n' "$(openssl rand -base64 32)" > /run/secrets/admin_root_password
chmod 600 /run/secrets/admin_root_password
ADMIN_AUTH_ROOT_PASSWORD_FILE=/run/secrets/admin_root_password go run . bootstrap-root
```

The password is read from `ADMIN_AUTH_ROOT_PASSWORD_FILE` (default
`/run/secrets/admin_root_password`), one trailing `\n`/`\r\n` is trimmed, and it must be
at least 16 characters. A group- or world-readable file prints a warning to stderr but
still works. The password is hashed with argon2id before it reaches the database and is
never printed.

- **No root yet:** the row is inserted and `root.bootstrap` is audited in one
  transaction; the command prints `root created: <email>`.
- **Root exists without `-rotate`:** nothing changes and it prints
  `root already exists: <email>`. The existing email is kept even if `-email` differs.
- **Root exists with `-rotate`:** the hash, `password_changed_at`, `failed_logins = 0`
  and `locked_until = NULL` are updated, every live `refresh_session` of the root is
  revoked, and `root.rotate` is audited in the same transaction. It prints
  `root password rotated; <n> sessions revoked`.
- **No root with `-rotate`:** it behaves as a create and says nothing existed to rotate.

The single-root unique index is the concurrency guard: two simultaneous
`bootstrap-root` runs cannot both insert, because the loser's `23505` reloads the row
and reports "already exists".

---

## Configuration

Every variable is read once at startup. `ADMIN_AUTH_DATABASE_URL` is required by all
four commands; `ADMIN_AUTH_SIGNING_KEY_PATH` and `ADMIN_AUTH_TOTP_KEY_PATH` are
required by `serve` only (for `genkey` the former is just the default for `-out` and
the latter is supplied with `-totp`, and `bootstrap-root` does not need either). The
process exits `1` at boot rather than starting in a half-working state — including when
Postgres is unreachable. Compose's restart policy is the retry loop; a service that
cannot reach its database has nothing useful to serve.

See `.env.example` for a copy-pasteable template.

| Variable | Required | Default | Notes |
|---|---|---|---|
| `ADMIN_AUTH_LISTEN_ADDR` | no | `:8080` | Public listener. |
| `ADMIN_AUTH_METRICS_ADDR` | no | `:9090` | `/healthz`, `/readyz`, `/metrics`, `/debug/pprof/`. Never exposed publicly. |
| `ADMIN_AUTH_DATABASE_URL` | **yes** | — | `postgres://admin_auth_rw:...@postgres:5432/admin_auth` |
| `ADMIN_AUTH_DB_MAX_CONNS` | no | `8` | See the connection budget below. |
| `ADMIN_AUTH_SIGNING_KEY_PATH` | **yes** (`serve`) | — | PKCS#8 PEM, mounted read-only. |
| `ADMIN_AUTH_TOTP_KEY_PATH` | **yes** (`serve`) | — | Base64 (std) one-liner holding exactly 32 bytes; seals staff TOTP secrets. Mounted read-only, `0600`. Generate with `genkey --totp`. |
| `ADMIN_AUTH_ROOT_PASSWORD_FILE` | no | `/run/secrets/admin_root_password` | Break-glass root password for `bootstrap-root`; read-only, `0600`. |
| `ADMIN_AUTH_ISSUER` | no | `https://admin-auth.otomo.internal` | Must equal Gateway's staff issuer exactly. |
| `ADMIN_AUTH_AUDIENCE` | no | `otomo:staff` | Must equal Gateway's staff audience. |
| `ADMIN_AUTH_ACCESS_TOKEN_TTL` | no | `15m` | Must be positive and at most `1h`. |
| `ADMIN_AUTH_INVITE_TTL` | no | `72h` | Invite and password-reset link lifetime, between `1h` and `168h`. |
| `ADMIN_AUTH_PUBLIC_URL` | no | `http://localhost:8090` | Base URL for `/admin/onboard#token=…` invite and reset links. |
| `ADMIN_AUTH_READ_TIMEOUT` | no | `10s` | `ReadHeaderTimeout` is fixed at `5s`. |
| `ADMIN_AUTH_WRITE_TIMEOUT` | no | `30s` | |
| `ADMIN_AUTH_IDLE_TIMEOUT` | no | `120s` | |
| `ADMIN_AUTH_SHUTDOWN_TIMEOUT` | no | `15s` | Bound on `http.Server.Shutdown`, shared by both listeners. |
| `ADMIN_AUTH_LOG_LEVEL` | no | `info` | `debug`, `info`, `warn`, `error`. |
| `ADMIN_AUTH_TEST_DATABASE_URL` | no | — | Read only by the `internal/store`, `internal/bootstrap` and `internal/server` tests. Never set in a deployment. |

### Connection budget (COM-14)

`ADMIN_AUTH_DB_MAX_CONNS` defaults to `8`. Keep the sum of every service's `MaxConns`
below Postgres's `max_connections`, minus a margin for admin sessions and the one-shot
`migrate` container.

---

## HTTP surface

### Public listener (`ADMIN_AUTH_LISTEN_ADDR`)

| Route | Status |
|---|---|
| `GET /.well-known/jwks.json` | Live. Pre-serialized bytes behind an `atomic.Pointer`, `Cache-Control: no-store`. |
| `POST /admin-auth/login` | Live. Staff password login; sets the refresh cookie, or answers `mfa_required` / `mfa_enrollment_required` with an `mfa_ticket`. |
| `POST /admin-auth/mfa/verify` | Live. `{mfa_ticket, code}` finishes a challenged login and returns the login body plus cookie. |
| `POST /admin-auth/mfa/enroll` | Live. `{mfa_ticket}` or bearer; returns `{secret, otpauth_url}` for an unconfirmed factor. |
| `POST /admin-auth/mfa/confirm` | Live. `{mfa_ticket?, code}` activates the pending factor and returns 10 recovery codes; with a ticket it also signs in. |
| `POST /admin-auth/onboard/lookup` | Live. Public. Describes a pending invite or reset link (`purpose`, `email`, `name`, `role`, `expires_at`); anything else is `404 invalid_link`. |
| `POST /admin-auth/onboard` | Live. Public. Redeems an invite or reset link, applies the password policy, then signs the account in exactly like `/login`. |
| `POST /admin-auth/refresh` | Live. Rotates the refresh cookie and issues a new access token. |
| `POST /admin-auth/logout` | Live. Revokes the refresh family. |
| `GET /admin-auth/me` | Live. Verifies the bearer in-process and reads the user from the database: `{id, name, roles, is_root, mfa_enabled}`. |
| `POST /admin-auth/account/password` | Live. Bearer of an active account. `{current_password, new_password}`: the current one is argon2-verified (a wrong one counts toward the login lockout like login does), the new one must pass the password policy and differ from the old, then the hash is replaced and every other live refresh session is revoked; `204`, audited `account.password_changed`. |
| `GET /admin-auth/account/sessions` | Live. Bearer. Lists the caller's live refresh sessions newest-last-used first as `{id, created_at, last_used_at, ip, user_agent, current}`; `current` marks the one named by the refresh cookie. `refresh_session` has no last-used column, so `last_used_at` is the head row's creation time (the newest rotation) and no migration was needed. |
| `POST /admin-auth/account/sessions/revoke-others` | Live. Bearer. Revokes every live session except the refresh cookie's rotation family and answers `{"revoked":n,"current_kept":bool}`; without a cookie it revokes all; audited `account.sessions_revoked`. |
| `POST /admin-auth/account/mfa/recovery-codes` | Live. Bearer of an account with a confirmed factor. `{code}` is a current TOTP (the replay guard applies); replaces every recovery code with ten new ones and audits `mfa.recovery_regenerated`; `409 mfa_not_enrolled` without a factor. |
| `POST /admin-auth/account/mfa/disable` | Live. Bearer. `{code}` is a TOTP or recovery code; decision D5 allows it only for an account without the `admin` role and never for root (`403 mfa_required` / `403 root_protected`). Clears the factor, pending secret, step and recovery codes, burns open MFA tickets and audits `mfa.disabled`; `204`. |
| `GET /admin-auth/audit` | Live. Bearer-gated at the **viewer** minimum. Newest-first audit feed in the same per-entry shape Config's `GET /api/admin/config/audit` uses (`source:"admin-auth"`); filters `action`, `actor` (actor_id, exact), `from`/`to` (RFC3339, inclusive), opaque `cursor`, and `limit` (1–200, default 50). |
| `GET /api/admin/users` | Live. Lists staff accounts (`mfa_enrolled` = TOTP confirmed). Never returns a hash or a TOTP secret. |
| `POST /api/admin/users` | Live. Creates an invite link. `admin` role is root-only; a live pending invite or existing user is `409`. |
| `GET /api/admin/users/invites` | Live. Lists pending invites and resets, never their tokens. |
| `DELETE /api/admin/users/invites/{id}` | Live. Revokes one pending link; unknown/used/revoked is `404`. |
| `PATCH /api/admin/users/{id}` | Live. Changes roles and/or status; root-only for anything that touches `admin`. |
| `POST /api/admin/users/{id}/reset` | Live. Creates a password-reset link, revoking the target's older pending resets. |
| `POST /api/admin/users/{id}/mfa/reset` | Live. Root-only for admins. Clears the target's second factor, recovery codes and open MFA tickets, revokes its live refresh sessions and audits `mfa.reset`, all in one transaction; `409 mfa_not_enrolled` when there is nothing to clear. |
| anything else | `404` or `405`, in the error shape below. |

`/api/admin/users*` is proxied by Gateway unstripped and sits behind its admin gate;
every route re-checks the bearer and the caller's role against the database. Invite and
reset tokens are 32 random bytes, only their sha256 is stored, and the link puts the
token in the URL **fragment** so a browser never sends it to Gateway or nginx.

### Onboarding links

Invite and reset links are `…/admin/onboard#token=<t>`. The token is in the URL
**fragment**, which a browser never transmits to a server, so it cannot appear in
Gateway, nginx or any access log and cannot leak through a `Referer` header. The
admin UI's onboarding page reads the fragment and POSTs the token, so it travels only
in a request body. There is deliberately **no route that carries the token in its
path**. Both routes are public — they are how a person without an account gets one —
and Gateway puts `/admin-auth/*` on its strict login rate-limit bucket.

```json
POST /admin-auth/onboard/lookup   {"token":"<t>"}
→ 200 {"purpose":"invite","email":"...","name":"...","role":"viewer","expires_at":"..."}
→ 200 {"purpose":"reset","email":"...","name":"...","role":null,"expires_at":"..."}
→ 404 invalid_link "this link is invalid or has expired"
```

```json
POST /admin-auth/onboard          {"token":"<t>","password":"<new password>"}
→ 200 (the login body: access_token, expires_in, user)
→ 200 {"mfa_required":true,"mfa_ticket":"…"}            // confirmed factor
→ 200 {"mfa_enrollment_required":true,"mfa_ticket":"…"} // admin without TOTP
```

Every unusable link — unknown, used, revoked or expired — returns the same
`404 invalid_link`, so the endpoint is not an oracle. The password policy is 12–128
runes, must not equal (case-insensitively) the account email or its local part, and
must not appear in the embedded common-password denylist
(`internal/password/common.txt`, compared case-insensitively). A refusal is
`400 validation_failed` naming the rule, and it does not consume the link.

Redemption is one transaction that locks the link `FOR UPDATE`: an invite creates the
`staff_user` with `created_by` from the link and audits `user.onboard`; a reset refuses
a missing or disabled target, replaces the password, clears any lockout, revokes every
live refresh session and audits `user.password_reset`. Both stamp `used_at` in the same
transaction. An invite whose address became a user in the meantime is `409
already_exists`, and the link is burned so it cannot be replayed. A successful
redemption then applies exactly the login MFA policy below: root and a non-`admin`
without a confirmed factor get the login body and cookie; an account with a confirmed
factor gets `200 {"mfa_required":true,"mfa_ticket":…}`, and an `admin` without one gets
`200 {"mfa_enrollment_required":true,"mfa_ticket":…}`, both with **no token and no
cookie**. The link is consumed and the password set in every case; the challenge is
finished on `/admin-auth/mfa/verify` (confirmed factor) or `/admin-auth/mfa/enroll` +
`/admin-auth/mfa/confirm` (enrollment), which are what issue the session.

### Multi-factor authentication (MFA)

TOTP (RFC 6238, SHA1, 6 digits, 30 s) is **required for `admin` accounts** and
optional for `viewer` and `live_ops`. The break-glass root account (`is_root`) is
exempt: it is never challenged and may not enroll (`403 mfa_not_allowed`). The login
outcome depends only on the account:

- root → session issued as before.
- any non-root account with a confirmed factor → `200 {"mfa_required":true,"mfa_ticket":…}`,
  **no access token and no cookie**.
- `admin` without a confirmed factor → `200 {"mfa_enrollment_required":true,"mfa_ticket":…}`.
- otherwise → the normal login body and cookie.

`mfa_ticket` is 32 random bytes as base64url, stored only as its sha256, valid for five
minutes and single use. Five wrong codes burn it.

`POST /admin-auth/mfa/verify` accepts a 6-digit TOTP or a recovery code
(`xxxx-xxxx`, case-insensitive, hyphen optional). The previous, current and next 30 s
steps are accepted; the step is recorded in `staff_user.totp_last_step` with an atomic
conditional update, so the same code cannot be replayed. A success returns exactly the
login body and cookie, and audits `login.success` with `{"mfa":"totp"}` or
`{"mfa":"recovery_code"}`; a recovery code also writes `mfa.recovery_used` with the
remaining count. Every unusable ticket is `401 invalid_ticket` and every wrong code
`401 invalid_code`; an account disabled or locked between the password and code steps
is `401 invalid_ticket`.

`POST /admin-auth/mfa/enroll` authenticates either with the enroll ticket from login or
with a bearer access token (the account page). It generates a 20-byte seed, seals it
with AES-256-GCM under `ADMIN_AUTH_TOTP_KEY_PATH` (the user id is the additional
authenticated data, so a ciphertext cannot move between rows) and returns the base32
secret and an `otpauth://` URL. It does not consume the ticket and does not enable the
factor; an account that already has one is `409 mfa_already_enabled`.

`POST /admin-auth/mfa/confirm` takes the same two auth modes and a code matching the
pending seed, enables the factor and replaces all recovery codes with ten new ones.
With a ticket it consumes the ticket and ends signed in (`access_token`, `expires_in`,
`user` and the cookie); with a bearer it returns only `{"recovery_codes":[…10]}`.
Confirming with no pending seed is `409 mfa_not_enrolling`.

`/.well-known/jwks.json` is **not** proxied by Gateway. Gateway and the backends fetch
it east-west at `http://admin-auth:8080/.well-known/jwks.json` before they have any
credential to present, which is why it must be public.

Middleware, outermost first: request ID → access log → metrics → `recover` → mux.
`recover` sits innermost so a panicking handler still produces its access-log line, its
metrics sample and a `500` body carrying the request ID, instead of unwinding past them.

### Internal listener (`ADMIN_AUTH_METRICS_ADDR`)

| Route | Behaviour |
|---|---|
| `GET /healthz` | Always `200 ok` once the process is listening. |
| `GET /readyz` | `200` once startup finished and **both** the cached Postgres ping and the signing-key check pass; `503` otherwise. |
| `GET /metrics` | Prometheus. |
| `/debug/pprof/*` | Runtime profiles. |

The signing-key check passes while at least one key is published in the JWKS cache;
`serve` refuses to start without an active `signing_key` row, so a healthy process is
ready as soon as it is listening. The Postgres ping behind `/readyz` is cached for 10s,
so probes do not become database load. None of these routes exist on the public
listener — they return the `404` body below.

### Error shape (COM-5)

Every 4xx and 5xx from the public listener:

```json
{ "error": { "code": "not_found", "message": "no such route", "request_id": "..." } }
```

`request_id` echoes the inbound `X-Request-Id`, or a fresh UUIDv4 if Gateway did not
supply one; the same value is returned in the `X-Request-Id` response header.
`api.WriteError` is the only thing that writes an error status.

### Metrics and logs (COM-10)

Metric names are `admin_auth_<thing>_<unit>`, labels are limited to `route`, `method`
and `status`:

- `admin_auth_http_requests_total{route,method,status}`
- `admin_auth_http_request_duration_seconds{route,method}`
- `admin_auth_jwks_keys_active`
- `admin_auth_build_info{version}`
- `staff_login_total{result}` — closed set `success`, `invalid_credentials`, `locked`, `bad_request`, `mfa_required`, `mfa_enrollment_required`, `error`; every series pre-initialised to 0.
- `staff_refresh_total{result}` — closed set `success`, `invalid`, `reuse_detected`, `error`; every series pre-initialised to 0.
- `staff_mfa_total{result}` — closed set `verified`, `recovery_code`, `invalid_code`, `invalid_ticket`, `enrolled`, `error`; every series pre-initialised to 0.

`route` is the path part of the **ServeMux pattern** that matched, never the raw URL,
so it stays bounded in cardinality and cannot leak identifiers. The catch-all is
`route="/"`. One JSON access-log line per request carries `method`, `route`, `status`,
`duration_ms` and `request_id`. Staff IDs, raw URLs and request IDs are never metric
labels.

---

## Tests

```sh
go test -race ./...
```

`internal/store`, `internal/bootstrap` and the DB-backed `internal/server` login tests
need a throwaway Postgres; without it they skip with
`ADMIN_AUTH_TEST_DATABASE_URL not set`, so CI stays green without one. The three
packages serialise themselves with one Postgres advisory lock, because the package
test binaries run in parallel and `internal/store`'s migration round-trip drops the
staff tables. To run them:

```sh
docker run --rm -d -p 5433:5432 -e POSTGRES_PASSWORD=pw -e POSTGRES_USER=auth_rw \
  -e POSTGRES_DB=admin_auth_test postgres:17
ADMIN_AUTH_TEST_DATABASE_URL='postgres://auth_rw:pw@localhost:5433/admin_auth_test?sslmode=disable' \
  go test -race ./...
```

They create the schema themselves and leave rows behind; point them at a database you
do not care about.

The `internal/token` tests generate a key in-process, build the JWKS and verify a
freshly issued staff token with Gateway's exact parser options
(`WithValidMethods(["EdDSA"])`, `WithIssuer`, `WithAudience`,
`WithExpirationRequired`). `internal/token/contract_test.go` is the contract test that
matters: it decodes a token's payload into the struct Gateway and the config service
consume and asserts the exact `iss`/`aud`/`roles`/`name` shape.

---

## Scope

**In:** config, HTTP server and middleware, error shape, metrics, request IDs,
graceful shutdown, readiness plumbing, the AA-2 staff schema and its embedded
migration set, signing-key management, JWKS publication and staff access-token
issuance, the break-glass root account (`bootstrap-root`), the staff session endpoints,
the public onboarding endpoints, TOTP MFA (enroll, confirm, verify and recovery codes),
and the staff user-management API under
`/api/admin/users`.

**Out (do not assume it exists):** other staff business endpoints (AA-4+), key-rotation
automation, JWKS rebuild on `LISTEN/NOTIFY`, and multi-key signing. There are also no
imports outside this service's own directory, because Jenkins checks out
`services/admin_auth` alone.