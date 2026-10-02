# Otomo: Allocator and player edge plan (M1)

**For:** whoever builds or changes the Allocator and the public player edge (reverse
proxy, TLS, Let's Encrypt). Plan IDs are from `13-player-plane-plan.md`.
**Works with:** Session (calls the Allocator and receives its callbacks), game servers
(register with the Allocator) and the Gameplay Proxy (verifies its join tickets).

---

## 1. Constraints of the first deployment host

The first deployment ran on a shared VM whose parent domain and DNS belonged to someone
else. Its firewall allowed only SSH and a few port ranges, so TCP 80 and 443 were closed at
the start, and the Let's Encrypt quota for the parent domain was shared with other sites.
Those constraints shaped §3: they're why the edge uses the host's certbot with a rehearsal
and a one-time issue, and why the Gameplay Proxy's port is configurable
(`GAMEPLAY_PROXY_PORT`, 27000 by default).

---

## 2. Order of work

1. **PL-1, launch hand-off design** (`design/14-launch-handoff.md`). It fixes the
   interfaces Session, the SDK and the proxy build against, so it comes first. §5 below is a
   starting draft.
2. **AL-1, Allocator skeleton**, and **AL-4, join tickets**, in parallel.
3. **AL-2, registry**, then **AL-3, allocation API**, then **AL-5, release and
   timeouts**, then **AL-6, metrics**.
4. **GW-1, public edge**, as soon as the certificate route in §3 is unblocked. It only
   depends on decision D1, so it can run alongside the Allocator.

---

## 3. GW-1: public player edge, TLS and Let's Encrypt

### 3.1 The blocker: getting a certificate

Let's Encrypt only issues a certificate after you prove you control the name, with one of:

| Challenge | Needs | Works here today? |
|---|---|---|
| HTTP-01 | Inbound **TCP 80** from the internet | No: port 80 is closed |
| TLS-ALPN-01 | Inbound **TCP 443** | No: port 443 is closed |
| DNS-01 | Creating a TXT record in the domain's DNS (through a DNS provider API) | Not when someone else controls the domain's DNS |

Two ways forward. Pick one before building:

- **Option A (recommended): get TCP 80 and 443 opened** on `203.0.113.10`, in
  `ufw` and in any upstream firewall. Players then use `https://play.example.com`, the
  standard port, and certificates renew automatically. Check first whether an upstream
  firewall exists: after opening 80 in `ufw`, test from outside with
  `curl -v http://play.example.com/`.
- **Option B: a domain you control**, e.g. a cheap `.dev`/`.gg` domain whose DNS provider has
  an API (Cloudflare, Porkbun, etc.). Point an `A` record at `203.0.113.10`, get the
  certificate with **DNS-01** (no inbound port needed), and serve HTTPS on an allowed port,
  e.g. **5043** (`https://play.<your-domain>:5043`). A certificate is for a *name*, not a
  port, so this is valid TLS, just not on 443.

Either way, record the final base URL in doc 06 §12 and doc 12 §3.4. (Auth once echoed it as
`services.session` from `AUTH_PUBLIC_SESSION_URL`; that hand-off was removed on 2026-09-29, D2,
so the client is the only place the base URL is configured.)

### 3.2 The reverse proxy (as built)

**Decision (2026-09-28):** option A. The host had no upstream firewall on 80/443, so
`ufw` rules for 80/443 were added. The proxy is **nginx (`services/edge`) with the host's
certbot**, not Caddy. The reason is the shared Let's Encrypt quota: 50 new certificates per
week per registered domain, shared with every other site on the parent domain. Caddy requests certificates by itself
whenever its storage is missing, so a lost volume or a misconfigured restart would spend
the quota. With certbot, the certificate is requested **once**, by hand, after a
dry run. certbot's timer renews it, and a deploy hook reloads nginx.

- `services/edge`: nginx on 80/443. `/auth/`, `/patch/` and `/api/player/` go to the
  gateway, `/docs/` to the technical wiki, and `/` to an optional site. Before a
  certificate exists it runs in bootstrap mode (ACME only).
- It sits on its own network `otomo-edge` (fixed subnet, edge at `172.31.250.2`) with the
  gateway and the wiki sites. It can't reach Postgres or any internal service.
- No cookies cross it in either direction. HSTS covers this host only.
- Runbook and the quota guardrails: `deploy/edge/README.md` and `deploy/edge/certbot.sh`.
- The gateway stays published on `127.0.0.1:8080` for SSH-tunnel development.

### 3.3 Required gateway change: client IPs behind the proxy

The player gateway rate-limits **per client IP** (`services/gateway/internal/ratelimit`:
general 20 req/s, burst 40; `/auth/` 5 req/s, burst 10), keyed on the TCP peer address. It
deliberately ignores `X-Forwarded-For` (see the TODO in `ratelimit.go`: "TLS termination
question… is unresolved… Do not trust X-Forwarded-For"). Behind Caddy, **every player's
peer address is Caddy's**, so all players would share one bucket: 20 requests per second
for the whole game.

Add to GW-1:

- A gateway setting such as `GATEWAY_TRUSTED_PROXIES` (CIDRs, e.g. Caddy's container
  address or the compose subnet). Only when the peer is in that list, take the client IP
  from the **rightmost** `X-Forwarded-For` entry that Caddy appended; otherwise keep using the
  peer address. Never trust the header from anyone else.
- Use that client IP for rate limiting and in access logs; keep the proxy's existing
  "replace `X-Forwarded-For` with the peer" behaviour for upstreams, feeding it the resolved
  client IP instead.
- Tests: spoofed `X-Forwarded-For` from an untrusted peer is ignored; two clients behind the
  proxy get separate buckets.

### 3.4 Done when

- `curl https://<domain>[:port]/patch/v1/live/manifest` works from outside the host.
- Plain HTTP is redirected (option A) or closed.
- A login from outside succeeds through the edge (at the time it also returned the public URL in `services.session`, since removed by D2).
- Two external clients are rate-limited independently.
- Certificates survive `up.sh` and container restarts without being re-issued.

---

## 4. AL-1…AL-6: the Allocator

### 4.1 Shape

Build it like the other Go services (copy the layout of `services/patch`: `internal/config`,
`internal/server`, `internal/api`, `internal/store`, `migrations`, subcommands `serve`,
`migrate`, `genkey`), with these differences:

- **No public route.** It listens only on the compose network. It must not appear in either
  gateway's route table.
- **Its own Postgres database and role** (`allocator`, `allocator_rw`), added to
  `deploy/postgres/init/01-provision.sh` **and** `deploy/scripts/provision-upgrade.sh` (the
  existing host's data directory never re-runs the init script).
- Compose: `allocator-migrate`, then `allocator`, with secrets mounted like Auth's; the
  `otomo-allocator` Jenkins job already exists (`ci/services/otomo-allocator/`).
- `/healthz`, `/readyz` (startup done + database ping), `/metrics`, JSON logs, request IDs,
  the shared error body `{"error":{"code","message","request_id"}}`.

### 4.2 State

Postgres is enough for M1 and survives restarts:

```sql
game_server (
  server_id      text primary key,          -- chosen by the game server, e.g. its container name
  internal_addr  text not null,             -- host:port on otomo-net (the proxy forwards here)
  capacity       smallint not null,
  state          text not null check (state in ('free','reserved','busy','dead')),
  allocation_id  uuid,
  last_heartbeat timestamptz not null
)
allocation (
  allocation_id  uuid primary key,
  party_id       uuid not null,
  server_id      text not null references game_server(server_id),
  player_ids     uuid[] not null,
  status         text not null check (status in ('reserved','active','ended','expired')),
  created_at     timestamptz not null default now(),
  expires_at     timestamptz not null          -- reserved and nobody connected by then → expired
)
-- one live allocation per party, which makes POST /internal/allocations idempotent
create unique index allocation_live_party on allocation(party_id) where status in ('reserved','active');
```

- Game servers heartbeat every 5 s. A reaper (every 5 s) marks servers with no heartbeat for
  15 s `dead`, and ends their allocations.
- Reserve with `SELECT … FROM game_server WHERE state = 'free' AND last_heartbeat > now() - interval '15 seconds' ORDER BY server_id LIMIT 1 FOR UPDATE SKIP LOCKED`,
  so concurrent allocations can never pick the same server.

### 4.3 Internal API

Every call carries `Authorization: Bearer <service key>` (decision D4: one static key per
caller, mounted as a secret, compared in constant time). The key identifies the caller, so
Session can't call the game-server endpoints and vice versa.

| Caller | Call | Answer |
|---|---|---|
| Session | `POST /internal/allocations` `{party_id, player_ids}` | `201 {allocation_id, address, port, tickets: {player_id: ticket}}`; the same `party_id` again → the same allocation (`200`); `503 no_capacity` |
| Game server | `POST /internal/servers/register` `{server_id, internal_addr, capacity}` | `204`; re-registering resets the server to `free` |
| Game server | `POST /internal/servers/{server_id}/heartbeat` `{players_connected}` | `204`; first connection moves `reserved` → `active`/`busy` |
| Game server | `POST /internal/servers/{server_id}/ended` | `204`; allocation `ended`, server `free` |
| Anyone internal | `GET /.well-known/jwks.json` | the ticket-signing public key(s) (§4.4) |

`address`/`port` in the allocation answer are the **Gameplay Proxy's public** address, since
clients always connect through the proxy (decision D3). The proxy finds the real server from
the ticket.

**Telling Session**: when an allocation ends, expires, or its server dies, call
Session's internal endpoint (settled in the launch hand-off design, e.g.
`POST /internal/session/allocations/{allocation_id}/ended {reason}`, with Session's key for
the Allocator). Session should also be able to ask `GET /internal/allocations?party_id=` so a
missed callback is repaired by polling.

### 4.4 Join tickets

Signed exactly like Auth's tokens (Ed25519, `golang-jwt`; reuse the approach of
`services/auth/internal/token`), so the same verification code works in the proxy:

| Claim | Value |
|---|---|
| header `alg`/`kid` | `EdDSA` / the Allocator's key id (`allocator genkey -kid`) |
| `iss` | `https://allocator.otomo.internal` |
| `aud` | `otomo:gameserver` |
| `sub` | the player's ID (the Auth `sub`) |
| `alloc` | allocation ID |
| `srv` | game server ID (the proxy routes on this) |
| `jti` | random ID; **single use**: the proxy remembers used `jti`s until they expire |
| `exp` | issue time + 60 s |

The private key is generated out of band with `genkey` (never at startup), like Auth's. The
proxy and game servers fetch `/.well-known/jwks.json` over the internal network.

### 4.5 Metrics

`allocator_servers{state}` (gauge), `allocator_allocations_total{result="ok|no_capacity|error"}`,
`allocator_allocation_seconds` (histogram), `allocator_reaped_total`. The Dashboard picks
the service up from Prometheus once it is scraped (add it to `deploy/observability`).

---

## 5. PL-1: what the launch hand-off design must settle

- Lobby states (`forming → launching → in_game → forming`) and what leave/kick/ready do in each.
- Exact request and response bodies for every call in §4.3, including the Session callback.
- Timeouts: allocation call (e.g. 5 s), reservation expiry (60 s), heartbeat (5 s) and
  death (15 s).
- The client events Session sends (`party.launching` with address, port and that member's
  ticket; `party.launch_failed`; `party.returned`), so the SDK and doc 12 §8 can be final.
- The proxy protocol: how the client presents its ticket on connect (first
  packet or handshake), and what the proxy does on failure.
- Failure paths: no capacity, Session restarting mid-launch, a member who never connects,
  a server dying mid-expedition.
