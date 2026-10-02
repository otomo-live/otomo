# Otomo: player plane plan

**For:** the network engineer building the player-facing service layer (Auth, Session,
Allocator, game-server hand-off, player gateway).
**Companion docs:** `12-godot-sdk-guide.md` (what the client does, the other side of every
contract here), `03-patch-minimal.md`, `04-session-minimal.md`, `06-auth-identity-contract.md`,
`07-auth-techspec.md`, `05-gateway-techspec.md`, `10-communication-schema.md` (every packet,
with handler locations), `14-launch-handoff.md` (the launch contract: lobby states, the
Allocator API, join tickets, client events), `15-allocator-and-edge-plan.md` (the Allocator
and the public edge as built).

This is the player-side counterpart of `11-admin-plane-plan.md`: where things stand, the
agreed flow, the decisions, and every task with acceptance criteria.

---

## 1. Where things stand (2026-10)

**The whole player flow runs end to end:** patch, device login and `/me/init`; a party,
ready and launch; a join ticket, the Gameplay Proxy handshake, and ENet to a game server.
The Allocator takes the server from free → reserved → active → ended → free.

| Piece | State |
|---|---|
| **Public edge** (`services/edge`) | **Done.** nginx terminates TLS with a Let's Encrypt certificate (issued once; certbot renews it). Routes `/auth/`, `/patch/` and `/api/player/` go to the player gateway, `/docs/` to the technical wiki, and `/` to the game's site. It strips `Cookie`/`Set-Cookie` and sets `X-Forwarded-For` to the client. |
| **Player gateway** (`services/gateway`) | Done for routing: JWKS verification of both domains, route policy, rate limits (general 20 rps/burst 40, `/auth/` 5 rps/burst 10), streaming routes. **Behind the edge**, it takes the client IP from the edge only (`GATEWAY_TRUSTED_PROXIES`), so every player has their own rate-limit bucket. It drops `Cookie`, because the player plane is bearer-only. Also on `127.0.0.1:8080`, for SSH-tunnel development. **Open:** GW-2, the route review for player traffic. |
| **Patch** | **Done.** The `live` manifest and blobs are public; `dev`/`staging` manifests need a staff token. The server manifest and its blobs are served on the internal listener `:8081` behind `patch_session.key` (CF-3). |
| **Auth** | **Done:** device login, refresh rotation with family revocation, logout. Tokens only in the login and refresh answers (D2); clients reach Session at `{gateway}/api/player/session`. AU-6: `auth_logins_total{result,new_account}`, `auth_refresh_total{result}`, and log-hygiene tests. |
| **Session** | **Every route is implemented** (SE-1…SE-8, LB-1…LB-4): profiles, presence and `otomo_online_players`, the event long-poll, friends and blocks, parties, the lobby (state, settings, ready), launch through the Allocator, the return from a match (callback on `:8081` plus the 30 s repair poll), staff routes and audit, content release enforcement (D5; a missing `X-Otomo-Release` passes unless `SESSION_REQUIRE_RELEASE_HEADER=true`), and the `session.rules` loader. SE-9's CI levels, gateway smoke and k6 load test are written (`services/session/load/README.md`); the load-test numbers are not recorded yet. |
| **Config** | **Done:** releases carry a server manifest built from server-audience namespaces, the admin UI composer picks them, and `session.rules` is seeded. |
| **Allocator** (`services/allocator`) | **Done:** internal only, with per-role service keys. It covers the game-server registry (register, 5 s heartbeat, reaper at 15 s), allocation (idempotent per party, `no_capacity`), Ed25519 join tickets with JWKS, reservation expiry (60 s), the outbox callback to Session, metrics, and `GET /internal/servers` for the proxy's discovery. |
| **Gameplay Proxy** (`services/gameplay_proxy`) | **Done:** the `OTJ1` handshake, ticket checks (signature, claims, single-use `jti`), forwarding to the server named by `srv`, a 30 s idle timeout. Listens on UDP 27000 by default (`GAMEPLAY_PROXY_PORT`). |
| **Game servers** | **Pool done:** `gs-1`/`gs-2` in compose (profile `gameservers`), from the game's Linux server export. Each registers, heartbeats, and at the end of a match reports `ended` and exits so compose restarts it with a fresh world. The game-server side of the contract, including the ticket check (GS-2), is the game's code (`docs/guide/game-servers.md`). |
| **Matchmaker** | Placeholder. Not in this milestone. |
| **Godot SDK** (`godot-plugin/`) | **Done:** Patch and Auth in full, Session profiles, presence, events, friends, parties and the lobby, launch and the proxy handshake, plus download-progress hooks for game UI. |
| **Observability** | **Done:** every player-plane service scraped; alerts `PlayerServiceDown`, `NoFreeGameServers` and `GameServerPoolEmpty`, shown on the admin Dashboard overview (no Alertmanager yet). |

**Open work:** GW-2 (player route review: long-poll timeout ≥ 35 s end to end, separate
buckets for heartbeat and long-poll, `X-Otomo-Release` passes through), OP-2 (a scripted
end-to-end acceptance client), OP-3 (load tests: SE-9's k6 run, Auth login bursts,
Allocator contention), and requiring `X-Otomo-Release` by default.

**What to check when reviewing** (the contract points that are easy to get wrong):
- **CF-3 (Patch):** the server manifest and its blobs only on Patch's internal listener. It must be a 404 through both gateways, and a wrong service key gets 401 (D4). It serves `release.server_manifest`, and a NULL row serves an empty manifest.
- **LB-1 (Session):** reload every 60 s with ETag; compiled-in defaults and last-good on a bad document; the cross-field checks `session.rules` can't express (`max_length >= min_length`, each `default` in its `allowed`); a metric on rejection.
- **LB-2…4:** doc 14 §2 settings semantics, `revision` on every leader call, `409 party_locked` outside `forming`, events after commit, and a callback listener on `:8081` only.
- **GS-1…3:** game servers publish no host ports; the proxy on UDP 27000; tickets checked twice (proxy, then game server), `jti` single use.
- **SDK:** `X-Otomo-Release` on every Session call; one long-poll loop (35 s timeout, resync); the handshake from the same local port ENet binds.

---

## 2. The agreed flow

```
Client ─► Gateway ─► 1. Patch      public; always first; content (.pck) + client config
                     2. Auth       device ID → access token (15 min) + rotating refresh token (30 d)
                     3. Session    profile, presence, friends, party = lobby, events (long-poll)
                          │ leader launches
                          ▼
                     4. Allocator  (internal only) reserves a game server, issues join tickets
                          │
                          ▼  proxy address + ticket delivered to each member as a Session event
Client ─── UDP 27000 ─► Gameplay Proxy   verifies the ticket, forwards to the game server it names
                          ▼
                     Game server   headless Godot; verifies the ticket again; runs the match
```

Decided:

- **Patch first, public.** The client patches before login. Patch cannot update the game
  executable; `X-Min-Client-Version` stops old executables.
- **Auth issues, the gateway verifies.** Nobody calls Auth to check a token; the gateway and
  Session verify signatures locally from Auth's JWKS. The client only knows the gateway's
  base URL.
- **Session owns lobbies.** The party *is* the lobby: settings, ready flags, launch.
  Membership has one owner.
- **Level 1 rules only.** Game rules (party size, name rules, lobby option values) are data
  in **server-audience Config namespaces**, enforced by Session's Go code. No server-side
  scripting in M1.
- **Allocator after Session.** Session calls the Allocator when a lobby launches; the
  Allocator manages a small fixed pool of headless Godot game servers. No matchmaking in M1.
- **Clients report their content release.** Session refuses clients whose loaded
  `release_id` is older than the live channel's.

---

## 3. Decisions

D1 to D5 are settled.

| ID | Decision | Recommendation | Blocks |
|---|---|---|---|
| **D1** | Player gateway public **domain and TLS** (who terminates, cert source) | **Settled 2026-09-28:** `https://play.example.com`. TLS terminates at the edge (nginx), with a Let's Encrypt certificate from the host's certbot. The certificate is requested once; certbot renews it. Recorded in doc 06 §12 and doc 12 §3.4; details in doc 15 §3 | — |
| **D2** | Drop Auth's `services` hand-off payload? | **Settled 2026-09-29: dropped.** The login and refresh responses carry tokens only, and `AUTH_PUBLIC_SESSION_URL` is gone. The client reaches every service at a fixed path under its configured gateway base URL, so a second copy of the address in Auth could only drift, and a wrong one would send the token elsewhere (doc 06 §12) | none |
| **D3** | **Gameplay Proxy** in M1, or direct connection? | **Settled 2026-09-28: proxy in M1** (GS-3). Game servers stay private behind it; the proxy and the game server both check the join ticket | GS-1, GS-3 |
| **D4** | **Service credential** for internal calls (Session → Patch server manifest, Session → Allocator, game server → Allocator) | **Settled and built:** one static key per caller role, generated into `deploy/secrets/service_keys/` and compared in constant time. The key decides which routes the caller may use: a wrong key gets 401, the wrong role 403 (doc 14 §4.1). Session's callback route goes on a separate internal listener, `:8081` | — |
| **D5** | Error for an **outdated content release** | **Settled 2026-09-29:** `409` with code `release_outdated` when the client's `X-Otomo-Release` is not the live channel head's `release_id`. Compare for equality, not "older": a rollback moves the head back to a lower id, and clients still on the rolled-back release must re-patch too. The client re-runs Patch and retries once (the SDK does this) | — |

---

## 4. The launch contract (PL-1, done)

**`design/14-launch-handoff.md`** is the contract Session, the Allocator, the
Gameplay Proxy and game servers build against. It covers:

- the lobby state machine (`forming → launching → in_game → forming`) and what every member
  action does in each state (§2), including Session's new routes and schema (§2.1);
- the launch sequence (§3) and every internal call's body, status codes and service key (§4);
- the join ticket's claims and each verifier's checks (§5);
- timeouts and the two Session sweeps that make restarts and lost callbacks safe (§6);
- client events `party.launching`, `party.launch_failed` and `party.returned` (§7);
- the UDP handshake to the Gameplay Proxy on port 27000 (§8), and the failure paths (§9).

---

## 5. Tasks

### Phase AU: Auth

| ID | Task | Acceptance criteria |
|---|---|---|
| AU-1 ✅ | Device login `POST /auth/anonymous` `{device_id}`: validate, find-or-create account via `identity_binding` (`ON CONFLICT`), issue access + refresh token | Same device → same `sub`; 10 concurrent first logins for one device → one account; token accepted by the player gateway's real middleware |
| AU-2 ✅ | Refresh `POST /auth/refresh`: atomic rotation, reuse revokes the family, 30-day sliding expiry; `POST /auth/logout` revokes the family (204, idempotent) | Reused token → 401 and the whole family dead; concurrent refresh with one token → exactly one success |
| AU-3 ✅ | Pin the HTTP contract (bodies, lifetimes, error codes) in docs 10 and 12; apply D2 | `12-godot-sdk-guide.md` §6 has no "proposed" labels left |
| AU-4 ✅ | End-to-end: login through the gateway, token passes Session's guard, refresh-then-retry | Scripted smoke passes in the compose stack |
| AU-5 ✅ | Tests: stable `sub`, claims/signature vs JWKS, rotation, reuse, logout | Green in Jenkins with Postgres |
| AU-6 ✅ | Metrics and log hygiene: `auth_logins_total{result,new_account}`, `auth_refresh_total{result}` (incl. `reuse_detected`); device IDs and tokens never logged | Metrics on `:9090`; a grep of logs for a known device ID finds nothing |

### Phase SE: Session core

SE-1…SE-8 are built and merged. SE-9's tests are written; the load-test numbers are not recorded yet.

| ID | Task | Acceptance criteria | Depends on |
|---|---|---|---|
| SE-1 ✅ | Deploy Session: `session-migrate` + `session` in compose (Valkey is already there), player and staff JWKS env, readiness; Jenkins job green | Host `/readyz` 200; a player call through the gateway returns Session's answer, not 502 | — |
| SE-2 ✅ | Profiles: `POST /me/init` (idempotent, provisional name, discriminator with unique-violation retry), `GET /me`, `PATCH /me` (name rules, 24 h rename limit → 429) | 10 concurrent `init` for one player → one profile; rename twice in a day → 429 | SE-1 |
| SE-3 ✅ | Presence: `POST /presence/heartbeat` (hash TTL 60 s, `presence:online` zset, ≤1 per 10 s → 429, trim loop); export `otomo_online_players` (the Dashboard overview already sums it) | Player shows offline 60 s after the last beat; Dashboard overview shows the online count | SE-1 |
| SE-4 ✅ | Events: atomic producer (Lua: INCR/RPUSH/LTRIM/EXPIRE/PUBLISH), one pub/sub connection per instance with fan-out, `GET /events?after=` long-poll (25 s hold, replaces an older poll, `{"resync":true}` past the window) | An event produced on instance B wakes a poll held on instance A within 100 ms; no goroutine growth after SE-9's load test | SE-1 |
| SE-5 ✅ | Friends and blocks: request by name#discriminator, accept/decline/remove, list with presence (one pipeline), block/unblock in one transaction; events | Blocked player cannot request or invite; friends list is one Valkey round trip | SE-2, SE-4 |
| SE-6 ✅ | Parties: create/get/invite/accept/decline/leave/kick/promote with `revision`; events published only after commit | Nobody can be in two parties; a rolled-back join notifies nobody; leader leaving promotes the longest member | SE-2, SE-4 |
| SE-7 ✅ | Staff routes: player lookup, force disband (`live_ops`, audited), `GET /api/admin/session/audit`; add Session as a Dashboard audit source | Disband appears in the admin Audit page with source `session` | SE-6 |
| SE-8 ✅ | Content release enforcement: clients send `X-Otomo-Release: <release_id>`; Session compares it with the live channel head and refuses any other release per D5. Let a request without the header through until the SDK sends the header, then refuse it too | A client on any release but the head (older, or newer after a rollback) gets `409 release_outdated` on every player route; the current release passes | SE-1 |
| SE-9 | Tests: DB + Valkey integration tests, Hurl smoke through the gateway, k6 with 2,000 clients heartbeating and long-polling | p95 heartbeat < 50 ms; no leaked goroutines; green in Jenkins | SE-2…SE-8 |

### Phase CF: server-only Config for Level 1 rules

All of CF is done (2026-09-29).

| ID | Task | Acceptance criteria | Depends on |
|---|---|---|---|
| CF-1 ✅ | Releases may include **server-audience** namespaces. Publishing builds two manifests: the client manifest (unchanged) and a **server manifest** (same format, server namespaces only). Rollback and promote carry both | A release without server namespaces produces a byte-identical client manifest; server namespaces never appear in the client manifest | — |
| CF-2 ✅ | Admin UI release composer: pick server namespaces; the diff and preview show both manifests | Composing a release with `session.rules` shows it under "server" | CF-1 |
| CF-3 ✅ | Patch serves the server manifest and its blobs on its **internal listener only**, authenticated per D4 | Not reachable through either gateway (404); wrong key → 401 | CF-1, D4 |
| CF-4 ✅ | Seed namespace `session.rules` with its schema: max party size, name length and charset, rename cooldown, allowed lobby setting values (e.g. expedition IDs) | `config seed` creates it; the admin UI edits it with validation | CF-1 |

### Phase LB: lobby (Session)

All of LB is built and merged (2026-09-29).

| ID | Task | Acceptance criteria | Depends on |
|---|---|---|---|
| LB-1 ✅ | Rules loader: fetch the live server manifest at start and every 60 s (ETag), load `session.rules` into typed rules with compiled-in defaults; a bad document keeps the last good one and bumps a metric | Publishing a new max party size changes invite limits within 60 s | CF-3, CF-4 |
| LB-2 ✅ | Lobby model per doc 14 §2/§2.1: party `state` (`forming`/`launching`/`in_game`), `settings` validated against the rules, per-member `ready` (cleared when settings change); `PATCH /party/settings` (leader, with `revision`), `POST /party/ready`; the migration in §2.1; `party.updated` carries them; `409 party_locked` for roster/settings changes outside `forming` | Invalid settings → 400; non-leader settings → 403; SDK sees ready changes via events; invite/kick while `in_game` → 409 | SE-6, LB-1 |
| LB-3 ✅ | Launch per doc 14 §3: `POST /party/launch` `{revision}` (leader, all ready, `forming`) commits `launching`, **then** calls `POST allocator:8080/internal/allocations` (5 s deadline, key `allocator_session.key`); success → `in_game` + `party.launching` to each member with **their own** ticket; failure → `forming` + `party.launch_failed`. Also `POST /party/launch/ticket` (fresh ticket while `in_game`) and the stuck-`launching` sweep (every 10 s, > 15 s old, repeats the idempotent call) | Two launch presses start one allocation; no capacity → every member gets `launch_failed` and the lobby is usable again; killing Session mid-launch still ends in one allocation | LB-2 |
| LB-4 ✅ | Return from game per doc 14 §4.4/§6: serve `POST /internal/session/allocations/{id}/ended` `{party_id, reason}` on a **separate internal listener** `SESSION_INTERNAL_ADDR` (`:8081`, key `session_allocator.key`), idempotent `204`; party → `forming`, every `ready` cleared, `party.returned{reason}`. Plus the 30 s repair poll of `in_game` parties via `GET /internal/allocations/{id}` | After a match ends every member is back in the same lobby; with Session down during the callback, the repair poll still returns the lobby | LB-3 |

### Phase AL: Allocator

Phase AL is **done and deployed**; the rows record what was accepted.

| ID | Task | Acceptance criteria | Depends on |
|---|---|---|---|
| AL-1 ✅ | Service skeleton like the other Go services (config, internal listener only, `/healthz`, `/readyz`, `/metrics`, JSON logs), state in Postgres or Valkey, compose + Jenkins | Deployed; no route on either gateway | — |
| AL-2 ✅ | Server registry: game servers register (id, internal address, capacity) and heartbeat every 5 s; 3 missed → dead; states `free`/`reserved`/`busy`; survives an Allocator restart | Killing a game server marks it dead within 15 s | AL-1, PL-1 |
| AL-3 ✅ | `POST /internal/allocations` `{party_id, player_ids}` → reserve a free server, return `{allocation_id, address, port, tickets{player_id: ticket}}`; idempotent per `party_id`; `503 no_capacity`; authenticated per D4 | Concurrent allocations never share a server; retries return the same allocation | AL-2, AL-4, D4 |
| AL-4 ✅ | Join tickets: Ed25519 key (`genkey` subcommand, like Auth), claims per PL-1, public key endpoint for game servers | A ticket for another server, expired, or reused is rejected by the verifier tests | AL-1, PL-1 |
| AL-5 ✅ | Release and timeouts: game server reports "ended" → `free`; reserved but nobody connected in 60 s → `free` and Session told; notify Session of ends and deaths | A reserved server with no joins is back in the pool after 60 s | AL-3 |
| AL-6 ✅ | Metrics: servers by state, allocations by result, time to allocate; visible in the Dashboard | Dashboard service detail shows allocator series | AL-3 |

### Phase GS: game servers

GS-1 and GS-3 are built. GS-2 (register, heartbeat, `ended` and the ticket check) is the
game server's own code; `docs/guide/game-servers.md` describes the contract.

| ID | Task | Acceptance criteria | Depends on |
|---|---|---|---|
| GS-1 ✅ | Headless Godot (.NET) dedicated-server image from the game's server export; env for Allocator URL (`http://allocator:8080`), server ID and internal address, with the key `allocator_gameserver.key` mounted; a fixed pool (e.g. 3) in compose on the internal network only (the Gameplay Proxy, GS-3, is the public entry point) | Three servers register with the Allocator on `up.sh`; `allocator_servers{state="free"}` reads 3 on the Dashboard | — |
| GS-2 | Game-server side of the hand-off per doc 14 §4.3/§5/§8 step 4: register, heartbeat every 5 s (the `200` answer names the expected players), verify the ticket in `SceneMultiplayer.auth_callback` (JWKS from `http://allocator:8080/.well-known/jwks.json`; `srv`, `alloc`, `sub` and single-use `jti`), report `ended` | A client with a valid ticket joins; a forged, expired, reused or other-server ticket is refused | GS-1 |
| GS-3 ✅ | Gameplay Proxy per doc 14 §8 on **UDP 27000**: the `OTJ1` handshake datagram carries the ticket; answer `OTOK`/`OTNO`+reason; forward `(client IP, port)` to the game server named by `srv`; 30 s idle timeout; `jti` remembered until expiry | A valid ticket reaches its server through the proxy; invalid, expired or reused tickets get `OTNO` and no forwarding; game servers publish no public ports | GS-1 |

### Phase GW: player edge

| ID | Task | Acceptance criteria | Depends on |
|---|---|---|---|
| GW-1 ✅ | Public player edge per D1: domain, TLS termination, bind, ufw; update docs 06 §12 and 12 §3.4 (**done**) | `curl https://play.example.com/patch/v1/live/manifest` works from outside; plain HTTP redirects; two external clients have separate buckets (all verified 2026-09-28) | — |
| GW-2 | Route review for M1 traffic: long-poll timeout ≥ 35 s end to end; separate rate-limit buckets so heartbeats and long-polls don't starve other calls; `X-Otomo-Release` passes through | A client heartbeating and long-polling never hits 429 on normal calls; route tests updated | SE-3, SE-4 |

### Phase OP: acceptance and operations

| ID | Task | Acceptance criteria | Depends on |
|---|---|---|---|
| OP-1 ✅ | Observability: Session, Auth and Allocator metrics in Prometheus and the Dashboard; Loki labels; M1 alerts (Session down, no free game servers), shown on the Dashboard overview | Each service has a Dashboard detail page with data; a firing alert appears on the overview within a refresh | SE-1, AL-6 |
| OP-2 | End-to-end acceptance client (scripted, in Jenkins): patch → login → `me/init` → party → ready → launch → connect to the game server with the ticket | Runs green against the compose stack | LB-3, GS-2 |
| OP-3 | Load: k6 login burst (Auth), 2,000 clients heartbeat + long-poll (Session), allocation contention (Allocator) | Numbers recorded in `docs/testing`; no errors at target load | SE-9, AL-3 |
| OP-4 | Docs to final shapes: 04, 06, 07, 10 and 12 (remove every "proposed"/"design pending" that is now built) | The SDK guide has no pending sections for M1 features | all |

---

## 6. Order of work

Done: PL-1, D1…D5, every AU, CF, LB and AL task, SE-1…SE-8, GW-1, GS-1…GS-3, OP-1, and the
SDK. What's left, in order: GW-2; then OP-2, the scripted acceptance run, and requiring
`X-Otomo-Release`; last, SE-9's recorded run and OP-3.

## 7. Out of scope for M1

- Matchmaking with strangers (Matchmaker).
- Server-side scripting of game rules (Level 2): revisit only if data-driven rules fall short.
- Platform login (Steam etc.), email accounts, account recovery.
- Shop, loadouts and inventory (later milestones; the rules/data boundary in LB-1 is where
  they will plug in).
- A staff "players" page in the admin UI (SE-7 provides the API only).
