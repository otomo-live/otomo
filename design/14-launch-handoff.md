# Otomo: launch hand-off (lobby → Allocator → game server)

**Plan ID:** PL-1 in `13-player-plane-plan.md`.
**Status:** Agreed and built. This doc is the contract. Implementations that
disagree with it are the bug.
**Builds on:** `13-player-plane-plan.md` (§3 decisions D3, D4), `15-allocator-and-edge-plan.md`
(§4 Allocator shape), `04-session-minimal.md` (parties, events), `06-auth-identity-contract.md`
(token format the join ticket copies).

This doc fixes every interface between Session, the Allocator, the Gameplay Proxy and a game
server, plus the events clients see. Otomo doesn't depend on any particular game. A **match**
is one run of a game on one game server for one party; what happens inside a match is the
game's business.

---

## 1. Actors

| Actor | Where | Talks to |
|---|---|---|
| Client | Player's machine | Gateway (HTTP), Gameplay Proxy (UDP) |
| Session | `session`, otomo-net | Allocator (HTTP, internal) |
| Allocator | `allocator`, otomo-net, **no gateway route** | Session (callback), game servers |
| Game server | Headless Godot, otomo-net only | Allocator (HTTP), Gameplay Proxy (UDP) |
| Gameplay Proxy | `gameplay-proxy`, public UDP **27000** | Allocator (JWKS), game servers |

UDP 27000 is the proxy's port by default (`GAMEPLAY_PROXY_PORT`); a deployment can change it.
`ALLOCATOR_PUBLIC_PORT` follows the same setting, so clients are always told the right
port.

---

## 2. Lobby state machine (Session)

The party *is* the lobby. It gains a `state`, leader-owned `settings` (a JSON object
validated against the `session.rules` Config namespace, LB-1) and a per-member `ready` flag.

`settings` is a flat object of string values, e.g. `{"expedition": "expedition_1",
"difficulty": "hard"}`. `session.rules` → `lobby.settings` lists every key a party may use,
with its `allowed` values and a `default` (schema and seeded values:
`services/config/internal/seed/seed/session.rules/`). A new party starts with every default.
`PATCH /party/settings` answers `400 invalid_settings` for a key that isn't listed or a value
that isn't allowed; a key the request leaves out keeps its current value.

```
            launch (leader, all ready)          allocation granted
 forming ─────────────────────────────► launching ─────────────────► in_game
    ▲                                       │                           │
    │        launch failed / timed out      │                           │
    ├───────────────────────────────────────┘                           │
    │        match ended / expired / server died                        │
    └───────────────────────────────────────────────────────────────────┘
```

| Action | `forming` | `launching` | `in_game` |
|---|---|---|---|
| Invite, accept invite | allowed | **409 `party_locked`** | **409 `party_locked`** |
| Change settings (leader) | allowed, clears every `ready` | 409 `party_locked` | 409 `party_locked` |
| Toggle ready | allowed | 409 `party_locked` | 409 `party_locked` |
| Kick, promote (leader) | allowed | 409 `party_locked` | 409 `party_locked` |
| Leave | allowed | allowed¹ | allowed¹ |
| Launch (leader) | → `launching` if every member is ready | idempotent: `202`, no second allocation | 409 `party_locked` |
| Staff force-disband | allowed | allowed² | allowed² |

1. A player can always leave; nobody is trapped in a lobby. Leaving while `launching` or
   `in_game` doesn't cancel the match for the others. The leaver's ticket simply goes unused,
   or their connection drops. A leader who leaves promotes the earliest member as usual.
2. The match isn't cancelled. When the Allocator later reports it ended, Session finds no
   party and ignores the callback (`204`).

Every transition bumps `party.revision` and emits `party.updated` after commit, as today.

### 2.1 Session additions (owned by LB-2…LB-4)

Schema (new migration):

```sql
ALTER TABLE party
  ADD COLUMN state text NOT NULL DEFAULT 'forming'
      CHECK (state IN ('forming','launching','in_game')),
  ADD COLUMN settings jsonb NOT NULL DEFAULT '{}',
  ADD COLUMN state_changed_at timestamptz NOT NULL DEFAULT now(),
  ADD COLUMN allocation_id uuid;                    -- set in in_game only
ALTER TABLE party_member ADD COLUMN ready boolean NOT NULL DEFAULT false;
CREATE INDEX party_state_idx ON party (state, state_changed_at) WHERE state <> 'forming';

-- LB-3: the match's public address, kept from the allocation response, because
-- GET /party's match and POST /party/launch/ticket answer with it and the Allocator
-- returns it only when allocating (§4.2).
ALTER TABLE party
  ADD COLUMN match_address text,                    -- set in in_game only
  ADD COLUMN match_port integer CHECK (match_port BETWEEN 1 AND 65535);
```

Player routes (all under `/api/player/session`):

| Call | Rules | Answer |
|---|---|---|
| `PATCH /party/settings` `{settings, revision}` | leader; `forming`; `revision` must equal the current one | `200` party; `400 invalid_settings`; `403`; `409 revision_mismatch` / `party_locked` |
| `POST /party/ready` `{ready}` | member; `forming` | `200` party |
| `POST /party/launch` `{revision}` | leader; every member ready | `202` party in `launching`; `409 not_ready` / `revision_mismatch` / `party_locked` |
| `POST /party/launch/ticket` | member; `in_game` | `200 {address, port, ticket, ticket_expires_at}`: a fresh ticket to (re)connect |
| `GET /party` | — | adds `state`, `settings`, `members[].ready`, and when `in_game`, `match: {allocation_id, address, port}` (no ticket) |

`POST /party/launch/ticket` covers a client that missed the `party.launching` event, came
back after a resync, crashed mid-match, or held a ticket that expired. The ticket is never
stored by Session.

---

## 3. Launch sequence

```
Client(leader)   Session                       Allocator                 Game server   Proxy
  │ POST /party/launch ─►│ tx: state=launching, revision++
  │◄── 202 ──────────────│ party.updated → members
  │                      │ POST /internal/allocations ─►│ reserve free server (SKIP LOCKED)
  │                      │   {party_id, player_ids}     │ allocation 'reserved', expires +60 s
  │                      │◄── 201 {allocation_id, address, port, tickets} ──│
  │                      │ tx: state=in_game, allocation_id, revision++
  │◄ party.launching ────│ (one event per member, each with only their own ticket)
  │                                                     │◄── heartbeat ─────────│
  │                                                     │── 200 {allocation} ──►│ expects these players
  │ UDP handshake(ticket) ───────────────────────────────────────────────────────────────►│ verify, route on srv
  │ ENet connect ─────────────────────────────────────────────────────────►│◄─ forwarded ─│
  │                                                     │◄─ heartbeat {players_connected:1}
  │                                                     │ allocation 'active', server 'busy'
  ⋮ match runs ⋮
  │                                                     │◄── POST …/ended ──────│
  │                      │◄── callback ended ───────────│ allocation 'ended', server 'free'
  │◄ party.returned ─────│ tx: state=forming, ready=false for all, revision++
```

Session calls the Allocator **after** committing `launching`, never inside the transaction,
with a **5 s** deadline. Success, failure and the recovery sweep (§6) all finish with one
conditional update, `UPDATE party … WHERE party_id=$1 AND state='launching'`, so a
duplicate finisher changes nothing and emits nothing.

---

## 4. Internal HTTP API

### 4.1 Authentication (D4)

Every internal call carries `Authorization: Bearer <service key>`. Keys are 32 random bytes,
base64url, generated by `deploy/scripts/generate-secrets.sh` into `deploy/secrets/`, mounted
read-only, and compared in constant time. **The key identifies the caller's role**, and each
route accepts exactly one role:

| Key file | Presented by | Accepted by |
|---|---|---|
| `allocator_session.key` | Session | Allocator: allocation routes |
| `allocator_gameserver.key` | every game server (one shared key in M1) | Allocator: server routes |
| `allocator_proxy.key` | the Gameplay Proxy | Allocator: the pool read (`GET /internal/servers`) |
| `session_allocator.key` | Allocator | Session: internal callback route |
| `patch_session.key` | Session | Patch: server manifest and its blobs, on Patch's internal listener `:8081` (CF-3; read by LB-1) |

A missing or wrong key → `401 unauthorized`. A valid key on the wrong route → `403 forbidden`.
No internal route is reachable through either gateway. Session serves its internal route on a
**separate listener** (`SESSION_INTERNAL_ADDR`, default `:8081`) that no gateway points at, and
reads the key it accepts there from `SESSION_CALLBACK_KEY_PATH`. The Allocator has no public
listener at all.

Errors use the shared body `{"error":{"code","message","request_id"}}`.

### 4.2 Session → Allocator

**`POST /internal/allocations`**

```json
{"party_id":"6f1c…","player_ids":["a1…","b2…"]}
```

| Case | Answer |
|---|---|
| New allocation | `201` body below |
| A live (`reserved`/`active`) allocation exists for `party_id` with the same player set | `200`, same allocation, **fresh tickets** |
| A live allocation exists with a different player set | `409 allocation_conflict` |
| No free server | `503 no_capacity` |
| 1–8 unique UUIDs not given | `400 invalid_request` |

```json
{
  "allocation_id": "0b7e…",
  "server_id": "gs-1",
  "status": "reserved",
  "address": "play.example.com",
  "port": 27000,
  "expires_at": "2026-10-01T12:00:60Z",
  "tickets": {"a1…": "eyJhbGciOiJFZERTQSIs…", "b2…": "eyJ…"}
}
```

`address`/`port` are always the **Gameplay Proxy's public** address (`ALLOCATOR_PUBLIC_ADDRESS`,
`ALLOCATOR_PUBLIC_PORT`), never the game server's.

**`GET /internal/allocations/{allocation_id}`** → `200 {allocation_id, party_id, server_id, status, end_reason, created_at, ended_at}` · `404 not_found`

**`GET /internal/allocations?party_id=<uuid>`** → `200` with the party's **latest** allocation (same
body) · `404 not_found`. This is Session's repair path for a missed callback (§6).

**`POST /internal/allocations/{allocation_id}/tickets`** `{"player_id":"a1…"}` →
`200 {"ticket","expires_at"}` · `404` unknown allocation or player not in it · `409 allocation_ended`.

### 4.3 Game server → Allocator

**`POST /internal/servers/register`** `{"server_id":"gs-1","internal_addr":"gs-1:7777","capacity":4}` → `204`.
Re-registering resets the server to `free` and ends any allocation it held with reason
`server_restarted`.

**`POST /internal/servers/{server_id}/heartbeat`** `{"players_connected":0}` → `200`:

```json
{"allocation": {"allocation_id":"0b7e…","player_ids":["a1…","b2…"],"expires_at":"…"}}
```

`allocation` is `null` when the server is free. A `null` after the server was hosting means
the allocation expired or was ended; the server resets itself. Heartbeats are every **5 s**.
*As built:* a server resets itself by exiting 0; compose's `restart: always`
starts a fresh world, which registers as `free`. It also reports `ended` itself once a
hosted match has had no players for 20 s.
An unknown `server_id` → `404 not_registered`, and the server must register again. While the
allocation is `reserved`, the first heartbeat with `players_connected > 0` moves it to `active`
(and the server to `busy`).

**`POST /internal/servers/{server_id}/ended`** `{"allocation_id":"0b7e…"}` → `204`; the allocation
becomes `ended` (reason `ended`) and the server `free`. A stale `allocation_id` → `409 allocation_mismatch`.

**`GET /.well-known/jwks.json`**: the ticket-verification keys (no auth needed; public keys only).

### 4.3.1 Gameplay Proxy → Allocator

**`GET /internal/servers`** (proxy key) → `200`:

```json
{"servers": [{"server_id":"gs-1","internal_addr":"gs-1:7777","state":"free"}]}
```

Every server not `dead`, ordered by `server_id`; an empty pool is `[]`. This is how the proxy
discovers game servers: they're a **fixed pool started by compose** (not by the Allocator), and
each one registers its own `internal_addr` (§4.3). The proxy refreshes the list on a short
period and whenever a ticket's `srv` isn't in it, and never forwards to a server that's missing
from it (decided 2026-09-29).

### 4.4 Allocator → Session

**`POST /internal/session/allocations/{allocation_id}/ended`** on Session's internal listener:

```json
{"party_id":"6f1c…","reason":"ended"}
```

`reason` ∈ `ended` (the server reported the match over), `expired` (reserved, nobody connected
within 60 s), `server_dead` (no heartbeat for 15 s), `server_restarted`. Session answers `204`
whether or not the party still exists or still points at that allocation (idempotent). The
Allocator retries non-2xx and network errors at 1, 2, 4, 8 and 16 s, then gives up, and the
repair poll (§6) catches it.

---

## 5. Join ticket

An Ed25519-signed JWT issued by the Allocator, built exactly like Auth's access tokens
(`services/auth/internal/token`: `golang-jwt/jwt/v5`, `EdDSA`), so the proxy and game servers
verify it with the same JWKS code as the gateway.

| Field | Value |
|---|---|
| header `alg` / `kid` | `EdDSA` / the key's JWK thumbprint |
| `iss` | `https://allocator.otomo.internal` |
| `aud` | `otomo:gameserver` |
| `sub` | player ID (the Auth `sub`) |
| `alloc` | allocation ID |
| `srv` | game-server ID; the proxy routes on it |
| `jti` | random 128-bit ID |
| `iat` / `exp` | issue time / issue time + **60 s** |

Verifiers must check the signature against the JWKS (`kid`), `iss`, `aud`, and `exp` (5 s
leeway). Then:

- **Proxy:** `srv` names a known server; `jti` unseen. Remember the `jti` until `exp`.
- **Game server:** `srv` is itself, `alloc` is its current allocation, `sub` is in that
  allocation's `player_ids` (from the heartbeat), and the `jti` is unseen by this server.

The private key is created out of band by `allocator genkey` (PKCS#8 PEM, mode 0600), never at
startup, and the Allocator refuses to start without it. The `kid` is the key's RFC 7638 JWK
thumbprint (base64url SHA-256), so it needs no database row or flag. Rotation is a restart
with a new file: tickets live 60 s, so at most one minute of issued tickets stops verifying. The proxy and game servers fetch
`http://allocator:8080/.well-known/jwks.json` and refresh it on an unknown `kid`, at most once
per 10 s.

---

## 6. Timeouts and recovery

| Timer | Value | On expiry |
|---|---|---|
| Session → Allocator call | 5 s | treat as failure: `forming`, `party.launch_failed{reason:"allocator_unavailable"}` |
| Stuck `launching` sweep | every 10 s, parties `launching` > 15 s | repeat `POST /internal/allocations` (idempotent), then finish as §3 |
| Missed-callback repair | every 30 s, parties `in_game` > 30 s | `GET /internal/allocations/{id}`; if not live → `forming` + `party.returned` |
| Reservation | 60 s | `reserved` with nobody connected → `expired`, server `free`, callback |
| Ticket | 60 s | client asks for a fresh one (`POST /party/launch/ticket`) |
| Heartbeat / death | 5 s / 15 s | server `dead`, live allocation ended `server_dead`, callback |
| Proxy flow idle | 30 s | forget the client↔server mapping |

The stuck-launch sweep is what makes a **Session restart mid-launch** safe: the `launching` row
survives, the sweep re-asks the Allocator, and idempotency on `party_id` guarantees one server.

The missed-callback repair returns a party with the allocation's `end_reason`. An allocation
the Allocator answers `404 not_found` for is hosting nothing, so its party is returned with
`ended`. Any other error ends that round of the repair, which tries again 30 s later. The
callback and the repair finish with the same conditional update,
`UPDATE party … WHERE party_id=$1 AND state='in_game' AND allocation_id=$2`, so whichever
comes second changes nothing and emits nothing.

---

## 7. Client events

Added to the Session event stream (doc 12 §7.3). Every payload carries `party_id` and `revision`.

| Type | Sent to | Payload | Client does |
|---|---|---|---|
| `party.updated` | members | existing, plus `state` | re-fetch `GET /party` (unchanged rule) |
| `party.launching` | each member, **individually** | `allocation_id, address, port, ticket, ticket_expires_at` | connect now (§8); if the ticket already expired, call `POST /party/launch/ticket` |
| `party.launch_failed` | members | `reason`: `no_capacity`, `allocator_unavailable` | show the error; the lobby is `forming` again, and ready flags are kept so the leader can retry |
| `party.returned` | members | `reason`: `ended`, `expired`, `server_dead`, `server_restarted` | leave the match scene; the lobby is `forming`, every `ready` cleared |

Events still expire after 10 minutes and are hints. A client that resyncs reads `state` from
`GET /party`, and when it's `in_game` it asks for a ticket.

---

## 8. Gameplay Proxy protocol (UDP)

Godot's ENet can't carry a ticket in its connect packet (32-bit data field), so the ticket goes
in a **handshake datagram** sent from the same local UDP port the ENet client will then bind:

1. Client binds `PacketPeerUDP` to a random local port *P* and sends to the proxy:
   `"OTJ1"` (4 bytes) · `u16` big-endian ticket length · ticket bytes.
2. The proxy verifies the ticket (§5) and answers `"OTOK"`, or `"OTNO"` + one reason byte
   (`1` invalid, `2` expired, `3` reused, `4` unknown server). The client resends the handshake
   up to 5 times, 500 ms apart, until it gets an answer.
3. On `OTOK` the client closes the `PacketPeerUDP` and calls
   `ENetMultiplayerPeer.create_client(address, port, …, local_port = P)`. The proxy forwards
   every datagram from `(client IP, P)` to the game server's `internal_addr` and back. It gets
   `internal_addr` for the ticket's `srv` from the Allocator's pool read (§4.3.1).
4. The game server repeats the ticket check through `SceneMultiplayer.auth_callback`: the client
   sends the same ticket with `send_auth`, and the server checks it per §5 before
   `complete_auth`.

On failure the proxy drops non-handshake datagrams from unknown sources silently. A NAT that
rebinds the client's port loses the mapping, and the client reconnects with a fresh ticket.

---

## 9. Failure paths

| Situation | What happens |
|---|---|
| No free server | Allocator `503 no_capacity` → Session: `forming`, `party.launch_failed{no_capacity}` |
| Allocator down or slow | 5 s deadline → `party.launch_failed{allocator_unavailable}` |
| Session restarts mid-launch | Party stays `launching`; the sweep (§6) repeats the idempotent allocation |
| Double launch press | Second press sees `launching` → `202`, no second call |
| A member never connects | The match starts when anyone connects; the absent member's ticket expires. If **nobody** connects in 60 s → `expired` → `party.returned{expired}` |
| Member disconnects mid-match | Game's decision; they rejoin with `POST /party/launch/ticket` while the party is `in_game` |
| Game server crashes | 15 s without heartbeat → `server_dead` → `party.returned{server_dead}` |
| Game server container restarts | Re-register → allocation ends `server_restarted` |
| Callback lost | Session's 30 s repair poll finds the allocation not live |
| Party disbanded mid-match | Match continues; the ended callback finds no party → `204`, ignored |
| Forged, expired or reused ticket | Proxy answers `OTNO`; the game server rejects it too in `auth_callback` |

---

## 10. Status

Built: this contract, the Allocator (§4.2–4.4, §5, §6 Allocator rows), Session's side (§2,
§4.4, §6 Session rows, §7), the Gameplay Proxy (§8 steps 1–3), the game-server pool in
compose, and the SDK's side (doc 12 §7.5/§8). The game server's own calls (§4.3) and its
ticket check (§8 step 4) are the game's code; `docs/guide/game-servers.md` describes them.
