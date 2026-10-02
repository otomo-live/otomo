# Gameplay Proxy

The public UDP entry point for game traffic. Players send a one-datagram join
handshake carrying their Allocator-minted join ticket; the proxy verifies it, looks up
the game server named by the ticket's `srv` claim, and forwards that client's traffic
to the server. Game servers stay private on the Docker network and publish no public
ports. Protocol: `design/14-launch-handoff.md` §8; ticket: §5.

It has two listeners:

| Port | Purpose |
|---|---|
| `27000/udp` | The public listener. Handshake plus forwarded game traffic. |
| `9090` | `/healthz`, `/readyz`, `/metrics`. Never exposed publicly. |

## What it does

1. **Handshake.** A client sends one datagram from the local port it will later bind
   for ENet: `OTJ1` (4 bytes), the ticket length as a big-endian `uint16`, then the
   ticket bytes. Anything shorter, longer, with the wrong magic or with a length that
   does not match the bytes present is not a handshake.
2. **Verify.** The ticket is an Ed25519 JWT signed by the Allocator. The proxy checks
   the header `kid` against the published JWKS, `iss`, `aud`, and `exp` with 5 s
   leeway, and requires `sub`, `alloc`, `srv` and `jti`.
3. **Look up.** `srv` must name a server in the Allocator's directory.
4. **Replay guard.** Each accepted `jti` is remembered until `exp` + leeway. The same
   jti from a different client address is refused; the same address resending its own
   handshake gets `OTOK` again without opening a second session.
5. **Answer.** `OTOK`, or `OTNO` plus one reason byte: `1` invalid, `2` expired,
   `3` reused, `4` unknown server. Beyond `PROXY_MAX_SESSIONS`, a new handshake gets
   `OTNO 1` and is logged.
6. **Forward.** On `OTOK` the client address gets a session with its own upstream UDP
   socket connected to the server's `internal_addr`. Client datagrams go upstream and
   server datagrams come back from the public socket. A silent session is closed after
   `PROXY_IDLE_TIMEOUT`.
7. A datagram from an address with no session that is not a handshake is dropped
   silently, so the proxy never answers a scan.

## Server discovery

The proxy polls the Allocator every `PROXY_DIRECTORY_REFRESH`:

```
GET {PROXY_ALLOCATOR_URL}/internal/servers
Authorization: Bearer <PROXY_ALLOCATOR_KEY_PATH contents>
```

The answer is `{"servers":[{"server_id","internal_addr","state"}]}`. The last good
snapshot is kept when a poll fails. When a ticket names a server the snapshot does not
know, the proxy refreshes once immediately — at most once per second — and then
decides, so a server that registered moments ago is found without waiting out the poll
interval.

Ticket-verification keys are fetched from `{PROXY_ALLOCATOR_URL}/.well-known/jwks.json`
at start-up and refetched when a ticket names an unknown `kid`, at most once per 10 s.

## Configuration

Every variable is read once at start-up and has a default. A configuration error prints
**every** problem at once and exits `1`. The Allocator key file is read at start-up and
a missing or malformed file fails the container.

| Variable | Default | Meaning |
|---|---|---|
| `PROXY_LISTEN_ADDR` | `:27000` | Public UDP listener. |
| `PROXY_METRICS_ADDR` | `:9090` | `/healthz`, `/readyz`, `/metrics`. Never exposed publicly. |
| `PROXY_ALLOCATOR_URL` | `http://allocator:8080` | Allocator base URL; the JWKS and directory paths are appended. |
| `PROXY_ALLOCATOR_KEY_PATH` | `/run/secrets/allocator_proxy.key` | Base64url proxy key sent as a bearer token when polling the directory. |
| `PROXY_TICKET_ISSUER` | `https://allocator.otomo.internal` | `iss` required on a ticket. |
| `PROXY_TICKET_AUDIENCE` | `otomo:gameserver` | `aud` required on a ticket. |
| `PROXY_IDLE_TIMEOUT` | `30s` | Silence in either direction after which a session is closed. |
| `PROXY_DIRECTORY_REFRESH` | `5s` | Server-directory poll period. |
| `PROXY_MAX_SESSIONS` | `2000` | Live-session cap; a new handshake beyond it gets `OTNO 1`. |
| `PROXY_SHUTDOWN_TIMEOUT` | `15s` | Bound on the private HTTP listener's graceful drain. |
| `PROXY_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`. |

The ticket leeway (5 s) and the unknown-kid JWKS refresh floor (10 s) are fixed by the
contract rather than configured, so a proxy and an Allocator cannot be tuned out of
agreement.

## Metrics

`/metrics` carries the Go and process collectors, `gameplay_proxy_build_info`, and the
domain instruments below. Every label combination is pre-initialised to `0`, so a
series exists before the first event.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `gameplay_proxy_handshakes_total` | counter | `result` | Answered handshakes: `ok`, `invalid`, `expired`, `reused`, `unknown_server`, `full`. |
| `gameplay_proxy_sessions` | gauge | — | Live client sessions. |
| `gameplay_proxy_packets_total` | counter | `direction` | Forwarded datagrams: `to_server`, `to_client`. |
| `gameplay_proxy_bytes_total` | counter | `direction` | Forwarded payload bytes: `to_server`, `to_client`. |
| `gameplay_proxy_directory_servers` | gauge | — | Servers in the last good directory snapshot. |
| `gameplay_proxy_directory_refresh_total` | counter | `result` | Directory polls: `ok`, `error`. |

`/readyz` answers `200` only once the JWKS and the server directory have each loaded at
least once; `/healthz` is liveness only and touches no dependency.

Logs are JSON on stdout. A handshake failure logs the reason, `srv` and the client
address; the ticket and the service key are never logged.

## Secrets it mounts

Read-only files under `/run/secrets`, generated by
`deploy/scripts/generate-secrets.sh`:

- `allocator_proxy.key` — `PROXY_ALLOCATOR_KEY_PATH`, the base64url key the proxy
  presents to the Allocator's `/internal/servers`.

Key material is never logged and never appears in an error message; a load failure
names the file and the role only.

## Tests

```sh
go test -count=1 ./...
```

The tests need no network, no Allocator and no secrets: the fake Allocator is an
`httptest` server, the game server is a UDP echo server, and everything binds
`127.0.0.1`. They cover the handshake codec, the verifier (including JWKS refetch and
its rate limit), the replay guard, configuration, and end-to-end handshake, forwarding,
refusals, replay, idle expiry and directory refresh.
