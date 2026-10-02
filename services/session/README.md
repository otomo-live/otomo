# Session

The social state of a player outside a match — profile, presence, friends, blocks and party
— and the event stream that tells a client when any of it changes.

**What exists today is the foundation, not the features:** config, HTTP server, middleware,
the COM-5 error shape, COM-10 metrics, graceful shutdown, the two-domain token guard
(SES-A1), the schema and migrations (SES-A2), the Valkey event producer (SES-A4) and the
audit writer (SES-A5). Profiles are built (SE-2: `POST /me/init`, `GET /me`, `PATCH /me`),
with the name rules in `internal/rules`, and so are the event long-poll (SE-4: `GET /events`)
and parties (SE-6: every `/party` route).
**Every other route handler is a `501` stub**. See
"Scope" at the bottom for what that means in practice.

Sources of truth: `design/04-session-minimal.md` (§3.1 the data model, §3.2 the event stream,
§4 the producer, §5 the route table), `design/06-auth-identity-contract.md` §3 (the staff
identity domain), `design/00-common-stack.md` (the COM-* conventions).

---

## Running it

Two commands, one binary. Unlike Auth there is nothing to generate: this service holds no
signing key, so a fresh deployment needs only a database and a Valkey.

```sh
export SESSION_DATABASE_URL='postgres://session_rw:pw@localhost:5432/session'
export SESSION_VALKEY_URL='valkey://default:pw@localhost:6379/0'
export SESSION_PLAYER_JWKS_URL='http://localhost:8081/.well-known/jwks.json'
export SESSION_STAFF_JWKS_URL='http://localhost:8082/.well-known/jwks.json'

go run . migrate          # apply the embedded goose migrations
go run . serve            # or just: go run .
```

The same commands work in the container, because the entrypoint is the binary:

```sh
docker run --rm --env-file session.env otomo-session:staging migrate
docker run --rm --env-file session.env -p 8080:8080 otomo-session:staging serve
```

### Commands

| Command | Does |
|---|---|
| `serve` (default) | Runs both listeners until `SIGTERM`/`SIGINT`, then shuts down gracefully. |
| `migrate` | Applies the embedded goose migrations. Idempotent. |

Unknown commands print usage and exit `2`. A configuration error prints **every** problem at
once and exits `1`.

Migrations are a separate command rather than startup work: several replicas starting at once
would each try to migrate, and a schema change should be a deliberate, observable act with
its own exit code rather than a side effect of a restart.

---

## Configuration

Every variable is read once at startup. `SESSION_DATABASE_URL`, `SESSION_VALKEY_URL` and the
six `SESSION_{PLAYER,STAFF}_{JWKS_URL,ISSUER,AUDIENCE}` values are required by `serve`;
`migrate` needs only `SESSION_DATABASE_URL`. The process exits `1` at boot rather than
starting in a half-working state — including when Postgres or Valkey is unreachable.

See `.env.example` for a copy-pasteable template.

| Variable | Required | Default | Notes |
|---|---|---|---|
| `SESSION_LISTEN_ADDR` | no | `:8080` | Public listener. Gateway proxies the two `/api/*/session/*` prefixes here. |
| `SESSION_METRICS_ADDR` | no | `:9090` | `/healthz`, `/readyz`, `/metrics`, `/debug/pprof/`. Never exposed publicly. |
| `SESSION_INTERNAL_ADDR` | no | `:8081` | The Allocator's end-of-match callback (LB-4). No gateway points at it. Must differ from the other two listeners. |
| `SESSION_CALLBACK_KEY_PATH` | no | none | The D4 key the Allocator presents on the internal listener (`session_allocator.key`). Unset: every call there answers `401`, and only the repair poll returns a party from its match. Set but unreadable: start-up fails. |
| `SESSION_DATABASE_URL` | **yes** | — | `postgres://session_rw:...@postgres:5432/session` |
| `SESSION_DB_MAX_CONNS` | no | `8` | See the connection budget below. |
| `SESSION_VALKEY_URL` | **yes** (`serve`) | — | Presence, pending-event lists, and the wake-up channel. `valkey://` and `redis://` both parse. |
| `SESSION_PLAYER_JWKS_URL` | **yes** (`serve`) | — | Auth's JWKS — not PHP Admin Auth's. |
| `SESSION_PLAYER_ISSUER` | **yes** (`serve`) | — | Must differ from the staff issuer. |
| `SESSION_PLAYER_AUDIENCE` | **yes** (`serve`) | — | Must differ from the staff audience. |
| `SESSION_STAFF_JWKS_URL` | **yes** (`serve`) | — | PHP Admin Auth's JWKS (contract §3). |
| `SESSION_STAFF_ISSUER` | **yes** (`serve`) | — | |
| `SESSION_STAFF_AUDIENCE` | **yes** (`serve`) | — | |
| `SESSION_JWKS_REFRESH` | no | `30s` | One ticker per domain. Must be positive — `0` is a startup error, not a way to switch fetching off. |
| `SESSION_JWT_CLOCK_SKEW` | no | `30s` | Leeway on `exp`/`nbf`. `0` is accepted for containers sharing a clock source. |
| `SESSION_EVENT_HOLD` | no | `25s` | How long `GET /events` will hold a request open (§4). Validated at startup. The value must already be below `SESSION_READ_TIMEOUT`, which must be below Gateway's 35 s read timeout for that route (§5a), because a hold longer than the proxy's timeout turns every idle poll into a 504. |
| `SESSION_PATCH_URL` | no | `http://patch:8081` | Patch's internal listener, where the rules loader (LB-1) reads the server manifest. |
| `SESSION_PATCH_KEY_PATH` | no | none | The D4 key Session presents to Patch (`patch_session.key`). Unset: the loader is off and the compiled-in rules apply. Set but unreadable: start-up fails. |
| `SESSION_RULES_CHANNEL` | no | `live` | Whose `session.rules` to follow: `dev`, `staging` or `live`. |
| `SESSION_RULES_POLL` | no | `60s` | How often the loader asks Patch, with `If-None-Match`. |
| `SESSION_REQUIRE_RELEASE_HEADER` | no | `false` | SE-8: refuse a player request without `X-Otomo-Release` (`409 release_outdated`). Off until the SDK sends the header; a request with a release other than the live one is refused either way. |
| `SESSION_ALLOCATOR_URL` | no | `http://allocator:8080` | Where lobby launches (LB-3) ask for a game server, and where the repair poll (LB-4) reads an allocation back. |
| `SESSION_ALLOCATOR_KEY_PATH` | no | none | The D4 key Session presents to the Allocator (`allocator_session.key`). Unset: `POST /party/launch` and `/party/launch/ticket` answer `503 launch_unavailable`, and the repair poll is off. Set but unreadable: start-up fails. |
| `SESSION_READ_TIMEOUT` | no | `35s` | `ReadHeaderTimeout` is fixed at `5s`. |
| `SESSION_WRITE_TIMEOUT` | no | `40s` | Above `ReadTimeout` on purpose: it is the budget for writing one answer, and the longest answer is a long-poll's, which cannot start until the hold is over. |
| `SESSION_IDLE_TIMEOUT` | no | `120s` | |
| `SESSION_SHUTDOWN_TIMEOUT` | no | `15s` | Bound on `http.Server.Shutdown`, shared by all three listeners. |
| `SESSION_LOG_LEVEL` | no | `info` | `debug`, `info`, `warn`, `error`. |
| `SESSION_TEST_DATABASE_URL` | no | — | Read only by the `internal/store` tests. Never set in a deployment. |
| `SESSION_TEST_VALKEY_URL` | no | — | Read only by the `internal/events` tests. Never set in a deployment. |

An empty value is treated as unset, so it falls back to the default rather than failing —
which is what makes `docker run --env-file` with blank lines behave.

### The two issuers must be distinguishable

Startup refuses a configuration where both domains share an issuer *and* an audience. The
keys already differ in any real deployment, so this is a second line of defence: it stops a
copy-pasted env file from silently making the staff domain accept player tokens, which would
put every `/api/admin/session/*` route behind the wrong identity.

### Connection budget (COM-14)

`SESSION_DB_MAX_CONNS` defaults to `8`. Keep the sum of every service's `MaxConns` below
Postgres's `max_connections`, minus a margin for admin sessions and the one-shot `migrate`
container. Raise it only alongside the Postgres setting.

---

## HTTP surface

### Public listener (`SESSION_LISTEN_ADDR`)

Routes are registered at their **external** paths — Gateway proxies `/api/player/session/*`
and `/api/admin/session/*` as wildcard prefixes without stripping them.

| Route | Domain | Min role | Status |
|---|---|---|---|
| `POST /api/player/session/me/init` | player | none | `200` profile, created on the first call (SE-2) |
| `GET /api/player/session/me` | player | none | `200` profile, or `404 profile_not_found` (SE-2) |
| `PATCH /api/player/session/me` | player | none | `200` profile; `400 invalid_display_name`; `409 name_unavailable`; `429 rate_limit_exceeded` with `Retry-After` inside the rename cooldown (SE-2) |
| `POST /api/player/session/presence/heartbeat` | player | none | `204`; `{"status"}` is `online`, `in_menus` or `away`, else `400 invalid_status`; `429 rate_limit_exceeded` with `Retry-After` under 10 s after the last beat (SE-3) |
| `GET /api/player/session/friends` | player | none | `200 {friends, incoming, outgoing}`; each entry `{player_id, display_name, discriminator, since}`, friends also `status` (presence). One SQL query and one Valkey pipeline (SE-5) |
| `POST /api/player/session/friends/requests` | player | none | `{"display_name","discriminator"}` (a number). `200 {player_id, display_name, discriminator, state}`, `state` is `pending`, or `accepted` when they had already asked you. `400 invalid_player_tag`, `403 blocked`, `404 player_not_found` / `profile_not_found`, `409 already_friends` / `friend_limit`, `429 rate_limit_exceeded` past 20 an hour (SE-5) |
| `POST /api/player/session/friends/requests/{player_id}/accept` | player | none | `200` the new friend; `404 request_not_found`, `409 friend_limit` (SE-5) |
| `POST /api/player/session/friends/requests/{player_id}/decline` | player | none | `204`; `404 request_not_found` (SE-5) |
| `DELETE /api/player/session/friends/{player_id}` | player | none | `204`: ends a friendship, or withdraws or refuses a request; `404 not_friends` (SE-5) |
| `POST /api/player/session/blocks/{player_id}` | player | none | `204`, also when already blocked; ends the friendship and every party invite between the two in the same transaction (SE-5) |
| `DELETE /api/player/session/blocks/{player_id}` | player | none | `204`, also when not blocked; does not restore the friendship (SE-5) |
| `POST /api/player/session/party` | player | none | `201` party; `409 already_in_party` (SE-6) |
| `GET /api/player/session/party` | player | none | `200` party with `state`, `settings`, `members[].ready`, `members[].status` (presence), and `match {allocation_id, address, port}` while `in_game` (never a ticket); `404 not_in_party` (SE-6, LB-2, LB-3) |
| `POST /api/player/session/party/invites` | player | none | `201` invite; any member may invite (SE-6) |
| `POST /api/player/session/party/invites/{invite_id}/accept` | player | none | `200` party; `409 party_full`, `410 invite_expired` (SE-6) |
| `POST /api/player/session/party/invites/{invite_id}/decline` | player | none | `204` (SE-6) |
| `POST /api/player/session/party/leave` | player | none | `204`; a leader leaving promotes the longest-standing member (SE-6) |
| `POST /api/player/session/party/kick/{player_id}` | player | none | `200` party; leader only, `{"revision"}` required (SE-6) |
| `POST /api/player/session/party/promote/{player_id}` | player | none | `200` party; leader only, `{"revision"}` required (SE-6) |
| `PATCH /api/player/session/party/settings` | player | none | `200` party; leader only, `{"settings","revision"}`; `400 invalid_settings`, `409 party_locked`; clears every ready (LB-2) |
| `POST /api/player/session/party/ready` | player | none | `200` party; any member, `{"ready"}`; `409 party_locked` outside `forming` (LB-2) |
| `POST /api/player/session/party/launch` | player | none | `202` party in `launching`; leader only, `{"revision"}`, every member ready; `409 not_ready`, `409 invalid_settings`, `409 party_locked` while `in_game`; a press while `launching` answers `202` again and starts nothing; `503 launch_unavailable` without an Allocator key (LB-3) |
| `POST /api/player/session/party/launch/ticket` | player | none | `200 {address, port, ticket, ticket_expires_at}`, a fresh join ticket for the caller while `in_game`; `409 not_in_game`, `409 match_ended`, `404 not_in_match`, `503 allocator_unavailable` (LB-3) |
| `GET /api/player/session/events` | player | none | `200` event list, `[]` when the hold ends or a newer poll replaces this one, or `{"resync":true}`; `400 invalid_after` (SE-4) |
| `GET /api/admin/session/players?name=&discriminator=` | staff | viewer | `200 {players: [{player_id, display_name, discriminator}]}`, name without case, discriminator optional, at most 50; `400 validation_failed` without a name (SE-7) |
| `GET /api/admin/session/players/{id}` | staff | viewer | `200 {player_id, display_name, discriminator, status, party}`, `party` is the party as `GET /party` shows it, or `null`; `404 player_not_found` (SE-7) |
| `POST /api/admin/session/parties/{party_id}/disband` | staff | live_ops | `204`; optional `{"reason"}` (at most 500 characters); in any state, audited in the same transaction (`party.disband_forced`), every member gets `party.disbanded`; `404 party_not_found` (SE-7) |
| `GET /api/admin/session/audit?limit=&cursor=&actor=&action=&from=&to=` | staff | viewer | `200 {entries, next_cursor}` in Config's shape with `source: "session"`, newest first; the Dashboard merges it (SE-7) |
| anything else | — | — | `404`, or `405` with `Allow` when the path exists under another method |

The route table is data (`internal/api/routes.go`), and `routes_test.go` holds a hand-written
second copy of §5's table so that a route added or dropped in one place fails the suite.

**Every path on the public listener is authenticated, including the fallback.** An
unauthenticated request to `/metrics`, `/nope` or `/` gets `401 missing_token` — not `404` —
so an anonymous caller cannot even learn which paths exist. A valid player token on those
paths gets the `404`.

### The two identity domains (SES-A1)

Which verifier runs is a property of the route's group, so the guard is per-route middleware
rather than one wrapper around the mux. A player route can only be satisfied by a token
signed by Auth's key and carrying Auth's issuer and audience; a staff route only by PHP Admin
Auth's. Nothing in the request can move a call from one domain to the other.

| Situation | Answer |
|---|---|
| No token, or a token with no `Bearer` | `401 missing_token` |
| Malformed token | `401 invalid_token` |
| Signature not from this domain's key | `401 invalid_signature` |
| Expired (beyond the clock skew) | `401 expired` |
| Right key, wrong issuer or audience | `401 iss_mismatch` / `401 aud_mismatch` |
| Valid token, role below the route's minimum | `403 insufficient_role` |
| No verifier configured for the group | `500 internal_error` |

401 and 403 are not interchangeable: a token the caller can fix by presenting a different one
is 401; a correctly-issued token that simply is not allowed is 403, and the client should stop
retrying. **The reason string is both the COM-5 `code` and the metric label**, so what a
client sees and what a dashboard spikes on are the same word.

In a real two-issuer deployment the cross-domain reason is `invalid_signature`, not
`iss_mismatch`: the two domains publish different keys, so the other domain's `kid` is not in
this key set at all and verification fails before any claim is compared. `iss_mismatch` is
what a *single-key-set* deployment would produce, which is why the two-issuer check above
exists as a separate guard.

### Internal listener (`SESSION_INTERNAL_ADDR`)

The Allocator's end-of-match callback (LB-4, doc 14 §4.1, §4.4). Compose sets
`ALLOCATOR_SESSION_URL=http://session:8081`, publishes no port for it, and no gateway points
at it.

| Route | Behaviour |
|---|---|
| `POST /internal/session/allocations/{allocation_id}/ended` `{party_id, reason}` | `204` whether or not the party still exists or is still in a match on that allocation (idempotent); `400 invalid_request` / `invalid_body` for a body outside the contract; `500` when the store fails, so the Allocator retries |
| anything else | `404` |

Every path needs `Authorization: Bearer <session_allocator.key>`, compared in constant time
(`internal/servicekey`). A missing, malformed or unknown key is `401 unauthorized` on every
path, the fallback included, so a caller without the key cannot learn which paths exist.
Without `SESSION_CALLBACK_KEY_PATH` every call is `401`. The listener has the same request
ID, access log and metrics as the public one.

### Metrics listener (`SESSION_METRICS_ADDR`)

| Route | Behaviour |
|---|---|
| `GET /healthz` | Always `200 ok` once the process is listening. |
| `GET /readyz` | `200` once startup finished **and** Postgres, Valkey and both JWKS are healthy; `503` otherwise, with **every** failing reason in the body. |
| `GET /metrics` | Prometheus. |
| `/debug/pprof/*` | Runtime profiles. |

None of these exist on the public listener. The Postgres probe behind `/readyz` is cached for
10 s so that probes do not become database load; the Valkey probe is a single `PING` on a
multiplexed connection; the JWKS checks read in-memory maps.

The checks run on their own context with a 2 s deadline, not on the request's. The deploy
probe (`deploy/scripts/lib.sh`) sends its request through `nc`, which half-closes the
connection, and Go cancels the request context when it sees that. A probe that gives up is
never cached as a Postgres failure.

### Error shape (COM-5)

Every 4xx and 5xx from the public listener, including `404` and `405`:

```json
{ "error": { "code": "invalid_signature", "message": "...", "request_id": "..." } }
```

`request_id` echoes the inbound `X-Request-Id`, or mints a UUIDv7 if Gateway did not supply
one; the same value is returned in the `X-Request-Id` response header. `api.WriteError` is
the only thing that writes an error status.

### Metrics and logs (COM-10)

Metric names are `session_<thing>_<unit>`; labels are limited to `route`, `method`, `status`,
`domain`, `reason`, `result` and `source`, each with a fixed set of values:

- `session_http_requests_total{route,method,status}`
- `session_http_request_duration_seconds{route,method}`
- `session_token_rejected_total{domain,reason}`
- `session_build_info{version}`
- `session_rules_polls_total{result}` and `session_rules_release_id` (LB-1)
- `session_release_checks_total{result}`: `current`, `outdated`, `missing`, `invalid`,
  `unchecked` (SE-8)
- `session_launch_total{result}`: `in_game`, `no_capacity`, `allocator_unavailable`,
  `superseded` (LB-3)
- `session_return_total{source}`: lobbies returned from a match, by `callback` or `repair`
  (LB-4)
- `otomo_online_players`: players whose last heartbeat is under 60 s old, set every 15 s
  (SE-3). The one metric without the `session_` prefix, because the Dashboard overview
  queries `sum(otomo_online_players)`. Every Session instance exports the same count, so
  with more than one replica that sum over-counts; compose runs one.

`route` is the path part of the **ServeMux pattern** that matched (`/api/player/session/me`,
or `/api/player/session/friends/requests/{player_id}/accept`), never the raw URL, so it stays
bounded and cannot leak identifiers; the method is its own label, and the catch-all is
`route="unmatched"`. One JSON access-log line per request carries `method`, `route`, `status`,
`duration_ms` and `request_id`. Player ids, staff ids, raw URLs and request IDs are never
metric labels.

---

## Data model (SES-A2)

Seven tables in `migrations/00001_init.sql`. The constraints are the point: each one is an
invariant a handler would otherwise have to remember.

| Table | Key | Holds |
|---|---|---|
| `player_profile` | `player_id` | `display_name`, `discriminator`. Unique on `(lower(display_name), discriminator)`, so "add by the name you see" is unambiguous; name non-empty, discriminator `1..9999`. |
| `friendship` | `(player_lo, player_hi)` | `state` in `('pending','accepted')`. **Stored with the lower uuid first and `CHECK (player_lo < player_hi)`**, so A→B and B→A cannot become two rows. |
| `block` | `(blocker, blocked)` | `CHECK (blocker <> blocked)`. No extra index: the primary key is the only lookup. |
| `party` | `party_id` | `leader_id`. |
| `party_member` | `player_id` | `party_id` with `ON DELETE CASCADE`. **The primary key is the rule "one party per player"** — joining a second party fails rather than needing a check. |
| `party_invite` | `invite_id` | `UNIQUE (party_id, to_player)`: re-inviting refreshes `expires_at` instead of creating a second live row. Indexed on `expires_at` for SES-D5's per-minute cleanup. |
| `audit_log` | `id` | Staff actions (SES-A5). |

`audit_log` uses **text**, not `uuid`, for `actor_id`: the actor is a staff account id from
PHP Admin Auth, which this service does not own and must not assume has any particular shape.
Nothing in the log is a foreign key, because the log is read long after the rows it names may
be gone.

`audit_log` rows are written **in the caller's transaction** — `store.WriteAudit` takes a
`pgx.Tx` — so an action and its record commit or roll back together. There is no queue and no
background writer, because a staff action that happened without a log line is worse than one
that failed.

---

## The event stream (SES-A3, SES-A4)

Per-player, in Valkey:

| Key | Holds |
|---|---|
| `events:{player}` | The last `EventCap` events, as a list of JSON documents. |
| `events:seq:{player}` | The sequence counter. |
| `notify:{player}` | The pub/sub channel a long-poll waits on. |

`Publish` is **one Lua script** (`internal/events/publish.lua`): `INCR` the counter → build
the document → `RPUSH` → `LTRIM` to the cap → `EXPIRE` both keys → `PUBLISH`. That atomicity
is the whole reason the script exists: a read-modify-write built from separate commands would
let two producers take the same sequence number, and the test that proves it races 8 producers
against one stream and asserts the exact set of numbers rather than a count.

The oldest retained sequence is **derived, not stored**: `seq - LLEN + 1`. `Read` gets the
counter and the list from one script (`internal/events/read.lua`), so a publish cannot land
between the two reads. `events.NeedsResync` asks for a resync when `after < seq - length`
(events were trimmed away) or when `after > seq` (the stream expired and started again from 1,
so the client's cursor belongs to a stream that no longer exists).

`Read` tolerates a missing counter (a stream nothing has been sent to yet, or one whose keys
have expired) and reads it as 0. The counter's absence is the ordinary state of a brand-new
player, not a failure.

### The long-poll (SE-4)

`GET /events?after=` is served by `internal/api/events.go` with an `events.Hub`:

- Each instance holds **one** pattern subscription, `PSUBSCRIBE notify:*`
  (`events.Client.Subscribe`), and passes each wake-up to the poll waiting for that player on
  this instance. The hand-off never blocks: each poll has a wake channel with room for one.
- A poll registers with the hub **before** its first read, so a publish between the read and
  the wait is not missed. It answers at once when events wait, and otherwise when woken, when
  `SESSION_EVENT_HOLD` ends (`[]`), or when the client leaves.
- One poll per player per instance: a newer poll ends the older one, which answers `[]`.
- Every (re)subscription wakes every waiting poll, since wake-ups sent while unsubscribed are
  lost.
- On shutdown the hub is closed, so held polls answer `[]` at once and the server's shutdown
  does not wait out a hold.

### Game rules (LB-1)

Party size, name rules, the rename cooldown and lobby settings come from the `session.rules`
Config namespace (`internal/rules`). Handlers read them from a `rules.Source` on every
request, so a new release changes behaviour on the next request.

- `rules.Defaults()` equals the seeded document (`TestDefaultsEqualTheSeed`), and is what
  applies until a release carries `session.rules`, or when the loader is off.
- `rules.Loader` polls `GET {SESSION_PATCH_URL}/internal/patch/server-manifest/{channel}`
  every `SESSION_RULES_POLL` with `If-None-Match`. On a new manifest it fetches the
  `session.rules` blob from `/internal/patch/blob/{sha256}`, checks its SHA-256, and parses
  it over the defaults: a field the document leaves out keeps its default, and
  `lobby.settings` replaces the default settings as a whole.
- `Rules.Validate` applies the schema's ranges and the two checks the schema cannot state:
  `names.max_length >= names.min_length`, and each lobby setting's `default` is one of its
  `allowed` values.
- Anything that fails (Patch unreachable, a hash mismatch, a bad document) keeps the last
  good rules. `session_rules_polls_total{result}` counts `not_modified`, `loaded`,
  `rejected` and `error`; `session_rules_release_id` is the release in force (0 on the
  defaults). The loader never affects readiness.

### Content release enforcement (SE-8)

Every player route, after the token check, compares the client's `X-Otomo-Release` with the
`release_id` of the live head of `SESSION_RULES_CHANNEL` (decision D5 in doc 13). Staff routes
are exempt. Implemented in `internal/server/release.go`.

| Request | Answer |
|---|---|
| `X-Otomo-Release` equals the head | passes |
| any other release, older or newer (a rollback moves the head down) | `409 release_outdated`; the client re-runs Patch and retries once |
| not a positive integer | `400 invalid_release` |
| no header | passes while `SESSION_REQUIRE_RELEASE_HEADER=false`; `409 release_outdated` once it is `true` |
| the head is not known (no Patch key, or Patch has not answered since start-up) | passes |

The head comes from the rules loader's poll of the server manifest, which Patch answers
with `X-Otomo-Release` (CF-3); it does not add a second poll. It is updated on every `200`,
even when the release carries the same rules. Because the poll runs every 60 s, a client
that patched to a release published since the last poll would otherwise be refused, so a
mismatch first asks Patch again (`Loader.Refresh`): at most one poll per 5 s, never waiting
for a poll already running, and giving up after 2 s. `session_release_checks_total{result}`
counts `current`, `outdated`, `missing`, `invalid` and `unchecked`.

### Launch (LB-3)

`POST /party/launch {"revision"}` is the leader's call from `forming` with every member
ready (doc 14 §2, §3). It runs in three steps, and the Allocator is never called inside a
transaction:

1. `store.StartLaunch` locks the party, checks leader, revision, ready flags and that the
   settings still fit the rules in force, and commits `launching`. The handler answers
   `202` with the party and sends `party.updated`. A second press finds `launching` and
   answers `202` without starting anything, so two presses start one allocation.
2. `launch.Launcher` asks the Allocator (`POST /internal/allocations`, 5 s deadline,
   `allocator_session.key`) in the background.
3. On success `store.FinishLaunch` commits `in_game` with the allocation's id, address and
   port, and each member gets their own `party.launching` carrying only their own ticket.
   On failure `store.FailLaunch` commits `forming` again, ready flags kept so the leader can
   retry, and every member gets `party.launch_failed{reason}`: `no_capacity` when the
   Allocator has no free game server, `allocator_unavailable` for anything else.

Both finishers act only while the party is still `launching`, so a duplicate finisher
changes nothing. Every 10 s a sweep repeats the Allocator call for parties `launching` for
more than 15 s (a restart mid-launch); the Allocator is idempotent per party, so this never
reserves a second server. `session_launch_total{result}` counts the outcomes, with
`superseded` for a finisher that found the work already done.

`POST /party/launch/ticket` asks the Allocator for a fresh ticket for the caller while the
party is `in_game`, for a client that missed `party.launching`, resynced, crashed or held
an expired ticket. Session never stores a ticket, and `GET /party` never shows one.

### Return from a match (LB-4)

A match ends when the Allocator calls back (above), or, if that callback is lost, when the
repair poll finds out:

- **Callback.** `store.ReturnFromGame` runs one conditional update,
  `WHERE party_id = $1 AND state = 'in_game' AND allocation_id = $2`, to `forming`, clears
  the allocation, address and port and every `ready`, and bumps `revision`. Every member
  gets `party.updated` and `party.returned{party_id, revision, reason}`. A party that is
  gone (disbanded mid-match) or already back, or one on another allocation, matches no row,
  and the callback still answers `204`.
- **Repair poll** (`launch.Returner`, doc 14 §6). Every 30 s, for each party `in_game` for
  more than 30 s, Session reads the allocation back (`GET /internal/allocations/{id}`). If
  it is not live (`reserved` or `active`), the party is returned the same way with the
  allocation's `end_reason`; an allocation the Allocator does not know is returned as
  `ended`. Any other error stops the round, and the next round asks again. The poll needs
  `SESSION_ALLOCATOR_KEY_PATH`.

Both paths go through the same update, so a callback racing the poll returns the party
once. `session_return_total{source}` counts returns by `callback` and `repair`.

During `launching` and `in_game` only leaving is allowed (doc 14 §2): a leaver does not
cancel the match for the others, a leader who leaves hands over as usual, and the last one
out disbands the party. Kick and promote answer `409 party_locked`.

### The Valkey instance

SES-A3's settings, as run locally:

```conf
bind 127.0.0.1
port 6379
maxmemory 256mb
maxmemory-policy noeviction     # evicting from the stream drops events a client has not read
save ""                         # it is a rolling window with its own TTL; not a source of truth
appendonly no

user default off                       # the service must authenticate
user session on >CHANGE_ME ~events:* ~notify:* ~presence:* ~ratelimit:* &notify:* +@all
```

The ACL is least-privilege: the service may touch only the four key families and the one
channel it uses. `SESSION_VALKEY_URL` carries that username and password, so it is a secret
and belongs in the credential-backed env file (COM-9), not in a compose file.

The deployed Valkey (`deploy/valkey/valkey.conf.example`, rendered by
`generate-secrets.sh`) sets `requirepass` on the default user instead, which Session uses.

### Presence (SE-3)

| Key | Holds |
|---|---|
| `presence:{player}` | Hash: `status`, `updated_at` (unix ms of the last beat). Expires 60 s after the last beat. |
| `presence:online` | Sorted set: every player, scored by their last beat (unix ms). |

- `POST /presence/heartbeat` is **one Lua script** (`internal/presence/heartbeat.lua`): if
  the last beat was under 10 s ago it answers with the wait (`429` and `Retry-After`) and
  changes nothing; otherwise `HSET`, `EXPIRE 60`, `ZADD`, and it returns the previous
  status. Two beats racing from one player cannot both pass the limit.
- A status change, including coming online, sends `presence.changed{player_id, status}` to
  each accepted friend who is online (doc 04 §4: friends only).
- Every 15 s each instance trims the set (`trim.lua`: `ZRANGEBYSCORE` then
  `ZREMRANGEBYSCORE` below now minus 60 s, in one script, so each player who went offline
  is returned to exactly one instance), tells their online friends they are `offline`, and
  sets `otomo_online_players` from `ZCOUNT presence:online (now-60s +inf`.
- A player is offline 60 s after the last beat: the hash has expired, so `GET /party`
  shows `offline`, and the count no longer includes them whether or not the trim has run.
- `GET /party` reads every member's status in one pipeline of `HGETALL`s. If Valkey cannot
  answer, the party is returned without statuses rather than failing.
- Presence shares the event stream's Valkey connection (`events.Client.Conn`).

### Friends and blocks (SE-5)

A player is found by `display_name` (without case, as the unique index compares) and
`discriminator`, a JSON number. A friendship is one `friendship` row, lower id first, in
`pending` or `accepted`; `requested_by` says who asked.

- **One lock order.** Every friend change first locks both players' `player_profile` rows,
  lower id first. So two players asking each other at the same moment end as one accepted
  friendship (the second request finds the first and accepts it), and two accepts cannot
  take a player past `session.rules` `friends.max_friends`.
- **Events**, after commit: `friend.request{player_id, display_name, discriminator}` to the
  one asked, `friend.accepted{...}` to the one who asked (also on an auto-accept), and
  `friend.removed{player_id}` on a decline, a removal, or a block that ended a friendship.
- **Block** (`store.Block`) is one transaction: insert the block, delete the friendship or
  request, and delete every party invite between the two, in either direction. After it,
  neither player can send the other a friend request or a party invite (`403 blocked`), and
  `AcceptInvite` refuses an invite between them that raced the block. Nothing tells the
  blocked player they were blocked.
- **`GET /friends`** is one SQL query (friendships joined to profiles), then one Valkey
  pipeline of `HGETALL presence:{friend}` for every accepted friend (SES-C4). If Valkey
  cannot answer, friends are listed without a status.
- **Rate limit** (SES-C5): at most 20 `POST /friends/requests` per player per hour, a
  fixed window in Valkey (`internal/ratelimit`, key `ratelimit:friend_request:{player}`),
  so it holds across instances. Past it: `429 rate_limit_exceeded` with `Retry-After`.

---

## Tests

```sh
go test -race ./...          # everything that needs no infrastructure
```

Three suites are gated on the environment and **skip** with a message when it is absent, so a
machine without Postgres or Valkey still runs the rest:

| Suite | Gate | Covers |
|---|---|---|
| `internal/store` | `SESSION_TEST_DATABASE_URL` | The migrations (twice, cleanly), every constraint above, and that `WriteAudit` lives in the caller's transaction — proved by rolling back and counting rows. |
| `internal/events` | `SESSION_TEST_VALKEY_URL` | The producer script: contiguous sequence numbers, the cap, the derived resync, both TTLs, and 8 concurrent producers never sharing a number. The subscription: an event produced through one client wakes a poll held on another instance's hub within 100 ms, only the addressed player is woken, and shutdown closes the hub. The hub, the resync rule and the long-poll handler are also tested without Valkey. |
| `internal/presence` | `SESSION_TEST_VALKEY_URL` | The heartbeat script: the lease and its 60 s TTL, the online score, one beat per 10 s (and one of ten racing beats written), offline without a lease, and a player trimmed once, 60 s after the last beat, and no longer counted. The announcements and the gauge are also tested without Valkey. |

The store tests refuse a database whose name does not contain `test`, as insurance against a
typo pointing destructive tests at a real one. They create the schema themselves and leave
rows behind; point them at a database you do not care about.

Everything else — the verification reasons, the route table against §5, the HTTP boundary,
the metrics — needs no infrastructure and runs anywhere.

### CI levels, gateway smoke and load (SE-9)

`ci/services/otomo-session/unit_test.sh <sanity|functional|integration|security|scaling>`
runs a selection of these Go tests per Jenkins stage, and fails any level but sanity when
either test URL is missing. `smoke/session.hurl` walks two new players through every player
feature through the player gateway, and `load/presence-longpoll.js` is the k6 run of 2,000
clients heartbeating and long-polling. Both need the deployed stack; how to run them is in
[`load/README.md`](load/README.md).

### End-to-end smoke test

`smoke.sh` builds the binaries from the working tree and runs a real service against real
Postgres, real Valkey and two real JWKS endpoints, then checks the two-domain boundary in
every direction, the role bar on force-disband, `405`/`Allow`, the internal listener being
unreachable from the public one, request-id echo, the metric series, and a clean `SIGTERM`
exit. `smoke/keygen` is the stand-in for Auth and PHP Admin Auth: it generates one Ed25519
key pair per domain, serves their JWKS, and mints tokens.

It needs `go`, `curl`, a Postgres it may migrate and a Valkey — **not** Docker, so it runs on
a workstation:

```sh
go run ./smoke/keygen init /tmp/keys          # one key pair per domain
SESSION_DATABASE_URL='postgres://user:pw@127.0.0.1:5432/session_smoke' \
SESSION_VALKEY_URL='valkey://session:pw@127.0.0.1:6379/0' \
  sh smoke.sh
```

A local Postgres and Valkey, no Docker:

```sh
initdb -D /tmp/pgdata && pg_ctl -D /tmp/pgdata -o '-p 5433' start
createdb -h 127.0.0.1 -p 5433 session_smoke

valkey-server --port 6379 --maxmemory 256mb --maxmemory-policy noeviction \
  --user default off --user 'session on >pw ~events:* ~notify:* ~presence:* ~ratelimit:* &notify:* +@all' &
```

---

## Deploying

- The image runs on distroless `nonroot` (uid `65532`) and writes nothing to disk — its state
  is in Postgres and Valkey — so there is no volume to create or own.
- All three ports are `EXPOSE`d (8080 public, 8081 internal, 9090 metrics); none is
  published in compose, and only 8080 is ever a gateway upstream.
- An unavailable JWKS does **not** stop startup, unlike Postgres and Valkey. The service
  starts, rejects every token on that domain, and reports itself unready until keys arrive.
  That keeps a Session deploy decoupled from a deploy of Auth or PHP Admin Auth; the
  alternative would make every Session restart depend on both being up first.
- **Jenkins note:** `ci/services/Jenkinsfile` runs the container with no environment. With
  fail-fast config it exits immediately on the missing `SESSION_DATABASE_URL`, which is
  correct and intentional — wiring env and secrets into the deploy is COM-9. Do not add
  placeholder defaults to make the staging container stay up; it would come up "healthy"
  without a database.

---

## Decisions worth not re-litigating

1. **Two verifiers, chosen by the route's group, applied as per-route middleware** — not one
   wrapper around the mux. A single outer middleware would have to re-derive the group from
   the path, duplicating what ServeMux already resolved, or check only the lowest bar and
   leave every per-route boundary to the handlers.
2. **The guard re-verifies issuer and audience even though Gateway already did**
   (00-common-stack.md §1a). Gateway's check protects the network; this one protects the data,
   and it is the only one that holds if the service is ever called from inside the compose
   network or a Gateway route is misconfigured to a lower group.
3. **A route with no verifier answers `500`, never "no authentication required."** The only
   way to reach it is a wiring mistake, and it is logged as one.
4. **The reason string is both the response `code` and the metric label.** One vocabulary
   means the 401 a client sees and the spike on a dashboard name the same thing. No separate
   log line: the access log already records the reason, and logging twice would put two lines
   at different levels for one event without either being more true.
5. **`friendship` stores the pair ordered and rejects the reversed insert** rather than
   normalising on write: the constraint is what makes "one row per friendship" true regardless
   of which handler inserted it.
6. **`party_member`'s primary key is the "one party per player" rule.** No check, no lock —
   the database is the rule.
7. **The event stream's oldest retained sequence is derived from `LLEN` and the counter**, not
   stored. One fewer key to keep in step inside the script.
8. **`Read` treats a missing counter as an empty stream, and a missing list as an error.** The
   first is the state of every player nothing has been sent to; the second cannot happen
   without a real fault.
9. **`audit_log.actor_id` is text, not uuid**, because the actor is a staff account id this
   service does not own. Nothing in the log is a foreign key.
10. **Migrations are a separate command**, so several replicas can start at once and a schema
    change is an observable act with its own exit code.
11. **`migrate` opens its own `database/sql` handle** rather than the pgx pool, because goose
    wants a `*sql.DB` and the command never runs alongside `serve`.
12. **Gated tests, not testcontainers.** A hard dependency on Docker would turn CI red for
    environmental reasons.
13. **Module path is `github.com/otomo-live/otomo/services/session`**, and this service
    imports nothing outside its own directory: Jenkins sparse-checks out
    `services/${params.SERVICE}` alone, so a shared module (COM-13) would be absent in CI.
14. **Prometheus uses a private registry**, so a second `Server` in a test cannot panic on
    duplicate registration.
15. **The smoke test runs binaries, not an image.** Auth's smoke test needs Docker; this one
    needs only Go, curl and the two dependencies, so it runs on a workstation with no Docker
    and still exercises a real process, real Postgres and real Valkey.

---

## Scope

**In:** config, HTTP server and middleware, the COM-5 error shape, COM-10 metrics, graceful
shutdown, the two-domain token guard with per-route role minimums (SES-A1), the schema and
migrations (SES-A2), the Valkey event producer (SES-A4), the audit writer (SES-A5), and the
full route table of §5 with every route registered and guarded. Profiles (SE-2):
`POST /me/init`, `GET /me` and `PATCH /me` with the name rules and the rename cooldown,
read per request from a `rules.Source`. The rules loader (LB-1) follows the
live `session.rules` (see "Game rules (LB-1)" above). The event long-poll (SE-4):
`GET /events` with one pub/sub subscription per instance, one poll per player, and resync.
Parties (SE-6) and the lobby model (LB-2): create, get, invite (any member), accept, decline, leave, kick,
promote, settings and ready. The party has a `state` (`forming`, `launching`, `in_game`), `settings` and per-member `ready`; outside `forming` only leaving is allowed (`409 party_locked`). Every change locks the party row first and bumps `revision`; kick, promote and settings need
the current `revision`; events are published only after the transaction commits; expired
invites are swept every minute (`sweepInvites` in `session.go`). Launch (LB-3) and
the return from a match (LB-4): see "Launch (LB-3)" and "Return from a match
(LB-4)" above. Presence (SE-3): see "Presence (SE-3)" above. Content release enforcement (SE-8): see "Content release enforcement (SE-8)" above. Friends and blocks (SE-5): see "Friends and blocks (SE-5)" above. The staff routes (SE-7): player
lookup, force-disband audited in the same transaction, and the audit feed the Dashboard
merges as source `session`. Every route in the table is implemented.

**Out: do not assume it exists:**

- **Party metrics (SES-D7)** are not exported yet.
- **`SESSION_EVENT_HOLD`-dependent Gateway wiring.** §5a's 35 s read timeout on `/events` is
  a Gateway-side setting; changing it is part of that service's deploy.
- **COM-7's compose file and SES-A3's settings inside it** — `deploy/` is empty. The Valkey
  settings are documented above instead.
- **Rate limiting** anywhere except the rename cooldown on `PATCH /me`, the heartbeat
  limit and the friend request limit.
- **The shared `platform` module (COM-13).** Deliberate: see decision 13.
