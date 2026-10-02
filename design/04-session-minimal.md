# Milestone 1 — Session (minimal)

**Scope:** Session, including the lobby (SE-1…SE-9 and LB-1…LB-4 in `13-player-plane-plan.md`)
**Owner domain:** Player (Auth tokens) for gameplay routes; Staff for a small admin surface
**Lobby and launch:** `14-launch-handoff.md` is the contract for the party's lobby states, the launch
routes, the Allocator calls, Session's internal callback listener (`:8081`) and the launch events. Where
this document and doc 14 differ on those, doc 14 wins.
**Prerequisites:** `00-common-stack.md` COM-1 → COM-10; Auth service issuing player tokens with a stable `sub`
**Consumers:** Godot client (M1); Matchmaker (next milestone, reads parties and presence)

---

## 1. Purpose and scope

Session owns the social state of a player outside a match: who they are publicly, whether they are online, who their friends are, and which party they are in. The party model built here is what the Matchmaker will queue next milestone, so it needs to be correct under concurrency even if the feature set is small.

**In scope for M1**

- Player profile (display name) created on first login
- Presence (`online`, `in_menus`, `away`, sent by the client; `offline` when heartbeats stop)
- Friends: request, accept, decline, remove; block
- Parties: create, invite, join, leave, kick, promote leader, disband
- Event delivery to clients (invites, friend requests, party changes) over REST long-polling
- Minimal staff lookup endpoints

**Out of scope for M1**

- Economy, inventory, currencies
- Text/voice chat
- Lobbies with game settings (next milestone, with Matchmaker)
- WebSocket transport (next milestone if long-poll becomes a limit)
- Moderation tooling beyond lookup

### Ownership boundaries

- **Auth** owns credentials and the player ID (`sub`). Session never stores passwords and never queries Auth's database.
- **Session** owns everything social, keyed by that player ID.

---

## 2. Tools you will need

| Tool | What it is | Role here |
|---|---|---|
| **PostgreSQL** | Relational database | Durable data: profiles, friendships, blocks, parties |
| **Valkey** | In-memory key/value store with expiring keys, sorted sets, lists and publish/subscribe; wire-compatible with Redis clients | Fast, short-lived data: presence, pending events, cross-instance notifications |
| **`valkey-io/valkey-go`** | Go Valkey client with automatic pipelining and pub/sub | Talking to Valkey |
| **Long-polling** | A REST pattern: the client makes a GET request that the server holds open until an event arrives or a timeout (e.g. 25 s) passes, then the client immediately asks again | Real-time-ish notifications while keeping your all-REST architecture |
| **Godot `HTTPRequest`** | Godot's HTTP node | Profile, friends, party calls; long-poll loop (use a separate node so a held request doesn't block other calls) |
| **Godot `Timer`** | Periodic callback node | Presence heartbeats |

### Why Valkey here and not only Postgres

Presence changes every few seconds for every online player and expires on its own when a client disappears; expiring keys do that without cleanup jobs. Waking a long-poll request held by Session instance A when an event is produced by instance B needs publish/subscribe. Postgres can do both, but not as cheaply. The Matchmaker will need Valkey for queues next milestone anyway, so Session is where you learn it.

---

## 3. Data model

### 3.1 PostgreSQL (`session` database)

```sql
player_profile (
  player_id        uuid primary key,          -- Auth token 'sub'
  display_name     text not null,             -- 3–16 chars, validated
  discriminator    smallint not null,         -- 0001–9999, allows duplicate names
  created_at       timestamptz not null default now(),
  updated_at       timestamptz not null default now()
)
-- Expression uniqueness needs an index, not a table constraint
create unique index player_name_uq on player_profile (lower(display_name), discriminator);

-- One row per pair; player_lo < player_hi keeps pairs unique regardless of order
friendship (
  player_lo        uuid not null references player_profile(player_id),
  player_hi        uuid not null references player_profile(player_id),
  state            text not null check (state in ('pending','accepted')),
  requested_by     uuid not null,
  created_at       timestamptz not null default now(),
  primary key (player_lo, player_hi),
  check (player_lo < player_hi)
)

block (
  blocker          uuid not null,
  blocked          uuid not null,
  created_at       timestamptz not null default now(),
  primary key (blocker, blocked)
)

party (
  party_id         uuid primary key,
  leader_id        uuid not null,
  max_size         smallint not null default 4,
  revision         int not null default 1,     -- bumps on every membership change
  created_at       timestamptz not null default now()
)

party_member (
  player_id        uuid primary key,           -- primary key = one party per player
  party_id         uuid not null references party(party_id) on delete cascade,
  joined_at        timestamptz not null default now()
)

party_invite (
  invite_id        uuid primary key,
  party_id         uuid not null references party(party_id) on delete cascade,
  from_player      uuid not null,
  to_player        uuid not null,
  expires_at       timestamptz not null,
  unique (party_id, to_player)
)
```

Friend and member limits (e.g. 200 friends, party size 4) come from a server-audience Config namespace later; hard-code them behind constants in M1.

### 3.2 Valkey keys

| Key | Type | Contents | Lifetime |
|---|---|---|---|
| `presence:{player_id}` | hash | `status`, `party_id`, `client_version`, `updated_at` | TTL 60 s, refreshed by heartbeat |
| `presence:online` | sorted set | member = player_id, score = last heartbeat unix time | Entries older than 60 s trimmed every 15 s |
| `events:{player_id}` | list | JSON events, newest at tail, capped at 100 | TTL 10 min |
| `events:seq:{player_id}` | counter | Monotonic event sequence number | TTL 10 min |
| channel `notify:{player_id}` | pub/sub | Wake-up signal only (no payload needed) | — |

Online player count = `ZCOUNT presence:online (now-60) +inf`. This is a single O(log N) call, instead of scanning all presence keys.

---

## 4. Event delivery (long-poll)

```
Client                     Session (any instance)                 Valkey
  │ GET /events?after=41 ─────►│ read events:{me} with seq > 41 ──►│
  │                            │ none? SUBSCRIBE notify:{me},      │
  │                            │ wait up to 25 s                   │
  │                            │◄──────── PUBLISH notify:{me} ─────│ (another instance
  │                            │ re-read events:{me} ─────────────►│  appended an event)
  │◄── 200 [{seq:42,...}] ─────│                                   │
  │ GET /events?after=42 ─────►│ ...                               │
```

- Producing an event: `INCR events:seq:{p}` → `RPUSH events:{p}` → `LTRIM` to 100 → `PUBLISH notify:{p}`.
- If the client's `after` is older than the oldest retained event, respond with `{ "resync": true }`; the client then re-fetches friends and party state via normal REST calls.
- Events are hints. The database is the source of truth. A missed event can never corrupt state, only delay the UI.
- The Gateway read timeout for `/api/player/session/events` must exceed the 25 s hold time (set 35 s).

Event types in M1: `friend.request`, `friend.accepted`, `friend.removed`, `party.invite`, `party.updated` (includes `revision`), `party.kicked`, `party.disbanded`, `presence.changed` (friends only).

---

## 5. API contract

### Player routes — `/api/player/session`, Auth issuer only

| Method & path | Purpose |
|---|---|
| `POST /me/init` | Idempotent: create profile on first login with a provisional name; returns profile |
| `GET /me` · `PATCH /me` | Read profile / change display name (rate-limited, e.g. once per 24 h) |
| `POST /presence/heartbeat` | Body `{status}`; refresh presence; every 20 s |
| `GET /friends` | Accepted friends with presence, plus incoming and outgoing requests |
| `POST /friends/requests` | Body `{display_name, discriminator}` |
| `POST /friends/requests/{player_id}/accept` · `/decline` | Respond to request |
| `DELETE /friends/{player_id}` | Remove friend |
| `POST /blocks/{player_id}` · `DELETE` | Block / unblock (blocking removes friendship and pending invites) |
| `POST /party` | Create party (fails if already in one) |
| `GET /party` | Current party with members, presence and `revision` |
| `POST /party/invites` | Body `{player_id}`; leader or any member (decide) |
| `POST /party/invites/{invite_id}/accept` · `/decline` | Join or decline |
| `POST /party/leave` | Leave; if leader, promote longest-standing member; if last, disband |
| `POST /party/kick/{player_id}` · `/promote/{player_id}` | Leader only |
| `GET /events?after={seq}` | Long-poll |

### Staff routes — `/api/admin/session`, staff issuer only

| Method & path | Purpose |
|---|---|
| `GET /players?name=&discriminator=` · `/players/{id}` | Lookup profile, presence, party |
| `POST /parties/{party_id}/disband` | Force disband (audited) |

---

## 5a. Go implementation notes

- **Long-poll handler:** after the immediate read finds nothing, register a wake channel for the player, then `select` on the wake channel, `time.After(25 * time.Second)` (or a reused `time.Timer`), and `r.Context().Done()`. Client disconnect cancels the context, so abandoned polls release their goroutine immediately.
- **One pub/sub connection per instance (SES-E2):** a single goroutine owns a pattern subscription to `notify:*` and fans messages out through a `map[playerID]chan struct{}` guarded by a mutex. Wake channels are buffered with size 1 and sent to with a non-blocking `select`, so a slow handler never blocks the dispatcher. Deregister in a `defer`.
- **Replacing an older poll (SES-E3):** store a cancel function per player alongside the wake channel; a new poll cancels the previous one's context before registering.
- **Event producer atomicity (SES-A4):** a short Lua script via `EVALSHA` (`INCR`, `RPUSH`, `LTRIM`, `EXPIRE`, `PUBLISH`) keeps sequence assignment and append atomic in one round trip.
- **Presence fan-out (SES-C4):** use valkey-go's `DoMulti` to send all `HGETALL presence:{id}` commands as one pipeline.
- **Party join (SES-D3):** run the transaction with `pgx.BeginFunc`; publish events only **after** it returns successfully, never inside it, so a rolled-back join never notifies anyone.
- **Unique-violation handling (SES-B1):** detect `*pgconn.PgError` with `Code == "23505"` to retry discriminator assignment.
- **Leak check:** run the `goroutineleak` profile after the SES-G3 load test; long-poll code is the most likely place to leak.

## 6. Task breakdown

### Phase A — Foundation

| ID | Task | Acceptance criteria | Depends on |
|---|---|---|---|
| SES-A1 | Create service from template; Gateway routes: `/api/player/session/*` (Auth issuer), `/api/admin/session/*` (staff issuer) | Staff token on player route 401 and vice versa | COM-2, COM-4 |
| SES-A2 | Migrations for §3.1 tables | Jenkins migration step succeeds | COM-6 |
| SES-A3 | Add Valkey to compose with a password (ACL), memory limit and `maxmemory-policy noeviction` (so presence/events are never silently evicted; errors are visible instead) | Service connects; `INFO` shows limit applied | COM-7 |
| SES-A4 | Event producer module (`publish_event(player_id, type, payload)`) implementing §4 atomically with a Lua script or `MULTI` | Concurrent producers never assign duplicate sequence numbers | SES-A3 |
| SES-A5 | Audit log table + writer for staff actions (same shape as Config's, exposed at `/api/admin/session/audit` for Dashboard) | Force-disband appears in Dashboard audit | SES-A2, DSH-B6 |

### Phase B — Profiles and presence

| ID | Task | Acceptance criteria | Depends on |
|---|---|---|---|
| SES-B1 | `POST /me/init` as `INSERT … ON CONFLICT DO NOTHING` then select; assign random free discriminator (retry on unique violation) | Calling init 10× concurrently creates exactly one profile | SES-A2 |
| SES-B2 | Display name validation: length, allowed characters (define Unicode policy), trimmed, reserved-word list | Invalid names rejected with field-level error | SES-B1 |
| SES-B3 | Heartbeat: `HSET` + `EXPIRE presence:{p}` + `ZADD presence:online`; if status changed, emit `presence.changed` to accepted friends who are online | Friend sees status change within one long-poll cycle | SES-A4 |
| SES-B4 | Background trimmer every 15 s: `ZREMRANGEBYSCORE presence:online -inf (now-60)`; emit offline events for removed players | Killing a client shows it offline to friends within ~75 s | SES-B3 |
| SES-B5 | Metric gauge `otomo_online_players` (the name the Dashboard overview sums) updated every 15 s from `ZCOUNT` | Online count appears on Dashboard overview | SES-B4, DSH-B1 |
| SES-B6 | Heartbeat rate limit (max 1 per 10 s per player) | Spamming heartbeats returns 429 | SES-B3 |

### Phase C — Friends and blocks

| ID | Task | Acceptance criteria | Depends on |
|---|---|---|---|
| SES-C1 | Send request: order the pair as `(lo, hi)`; if a pending request exists *from the other player*, auto-accept; reject if either side blocked; enforce friend limit | Mutual simultaneous requests end as one accepted friendship | SES-A2 |
| SES-C2 | Accept / decline / remove, each emitting events to the other player | Both clients update without manual refresh | SES-C1, SES-A4 |
| SES-C3 | Block: in one transaction insert block, delete friendship, delete party invites between the two | Blocked player cannot send requests or invites | SES-C1 |
| SES-C4 | `GET /friends`: one SQL query for friendships + one Valkey pipeline (`HGETALL` for each friend in a single round trip) for presence | 200 friends resolved with 2 round trips total | SES-C1, SES-B3 |
| SES-C5 | Request rate limit (e.g. 20 friend requests per hour per player) | Exceeding limit returns 429 | SES-C1 |

### Phase D — Parties

| ID | Task | Acceptance criteria | Depends on |
|---|---|---|---|
| SES-D1 | Create party: insert `party` + `party_member` in one transaction; the `party_member` primary key rejects players already in a party | Player cannot be in two parties | SES-A2 |
| SES-D2 | Invite: validate membership, not blocked, target not already a member; upsert invite with expiry (e.g. 5 min); emit `party.invite` | Duplicate invite refreshes expiry instead of creating a second row | SES-D1 |
| SES-D3 | Join via invite: transaction with `SELECT … FROM party WHERE party_id = $1 FOR UPDATE`, check invite unexpired, check member count < `max_size`, insert member, delete invite, bump `revision`; emit `party.updated` to all members after commit | 5 players accepting invites to a 4-slot party concurrently: exactly 3 join (plus leader), the rest get a "party full" error | SES-D2 |
| SES-D4 | Leave, kick, promote, disband with the same row lock and `revision` bump; leader leaving promotes the earliest `joined_at` member | Leader leaving never leaves a party without a leader | SES-D3 |
| SES-D5 | Invite expiry cleanup job (every minute, delete expired rows) | Expired invites cannot be accepted and are removed | SES-D2 |
| SES-D6 | `GET /party` returns members with presence and `revision`; clients discard any `party.updated` event whose `revision` is older than what they hold | Out-of-order events never roll back client UI | SES-D3 |
| SES-D7 | Metrics: `session_parties_active` gauge, `session_party_join_total{result}` | Visible on Dashboard | DSH-B1 |

### Phase E — Event delivery

| ID | Task | Acceptance criteria | Depends on |
|---|---|---|---|
| SES-E1 | `GET /events?after=`: immediate return if newer events exist; otherwise subscribe to `notify:{p}` and wait up to 25 s; return `[]` on timeout; `resync` when `after` is too old | Event delivered in < 1 s when produced by another Session instance | SES-A4 |
| SES-E2 | Share one pub/sub connection per Session instance (pattern-subscribe or a subscription multiplexer) instead of one Valkey connection per waiting request | 1,000 waiting requests use a handful of Valkey connections | SES-E1 |
| SES-E3 | Cap concurrent long-polls per player (1; a new one ends the old with `[]`) | Reconnecting clients never accumulate held requests | SES-E1 |
| SES-E4 | Gateway: read timeout 35 s on the events route; confirm no buffering | No 504s during idle periods | SES-E1 |
| SES-E5 | Run two Session replicas in compose to prove cross-instance delivery works | Test passes with requests routed to different replicas | SES-E1 |

### Phase F — Godot client

| ID | Task | Acceptance criteria | Depends on |
|---|---|---|---|
| SES-F1 | `SessionClient` autoload: holds player token from Auth login, calls `/me/init`, exposes profile | Profile available after login | SES-B1 |
| SES-F2 | Heartbeat `Timer` (20 s) with status derived from game state (menu/away) | Presence correct across scene changes | SES-B3 |
| SES-F3 | Event loop on a dedicated `HTTPRequest` node: request → dispatch events by type as signals → immediately request again; exponential backoff (1 s → 30 s) on errors; handle `resync` | Pulling the network cable and restoring it recovers without restart | SES-E1 |
| SES-F4 | Friends screen: list with presence, add by `name#1234`, incoming requests with accept/decline | Two clients on different machines can become friends | SES-C4 |
| SES-F5 | Party screen: create, invite from friends list, invite popup with accept/decline, member list, leave/kick | Two to four clients form a party end-to-end | SES-D6 |

### Phase G — Testing and delivery

| ID | Task | Acceptance criteria | Depends on |
|---|---|---|---|
| SES-G1 | Integration tests (real Postgres + Valkey): concurrent init, mutual friend requests, party overfill race, leader leave, block side effects | Pass in Jenkins | Phases B–D |
| SES-G2 | Hurl contract tests for issuer separation and every 4xx path in §5 | Pass in Jenkins | SES-A1 |
| SES-G3 | k6 test: 2,000 simulated players heartbeating every 20 s and holding long-polls; 10% forming parties | p95 heartbeat < 30 ms; no Valkey memory growth over 15 min | Phase E |
| SES-G4 | Jenkins pipeline (COM-8) with migrations | Push to main deploys | COM-8 |

---

## 7. Definition of done

- Two players log in through Auth, add each other as friends, see each other's presence, form a party, and see changes appear without manual refresh.
- Concurrency tests for party joins and friend requests pass repeatedly.
- The Dashboard shows online players, active parties and Session request metrics.
- A staff member can look up a player and force-disband a party, and it appears in the audit trail.

## 8. Risks

| Risk | Mitigation |
|---|---|
| Party overfill or duplicate membership under concurrent joins | Row lock + primary-key constraint (SES-D1, SES-D3) and race tests (SES-G1) |
| Long-poll connections exhaust Gateway or Valkey connections | Shared pub/sub connection, one poll per player, correct timeouts (SES-E2 → E4) |
| Lost events desynchronize client UI | Events are hints; `revision` and `resync` force a REST refresh |
| Valkey restart wipes presence | Acceptable: presence repopulates within one heartbeat interval; durable data is in Postgres |
| Scope creep into lobbies and matchmaking | Parties only; lobby settings wait for the Matchmaker milestone |
