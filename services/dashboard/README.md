# Dashboard

Live-ops dashboard backend. A thin, access-controlled query layer in front of Prometheus
and Loki: it owns no time series and no log index, and it never lets a browser reach those
tools directly. Staff call the Dashboard, the Dashboard calls the stores, and only named
query templates are ever run.

**What exists today is the foundation plus the query layer:**
configuration, both HTTP listeners, structured logging, metrics, graceful shutdown,
staff-token verification with per-route role enforcement (DSH-C2), readiness derived from
the staff JWKS, the **whole** §4 route table registered at its external paths, the named
PromQL template catalogue (`internal/promql`) and the Prometheus client that implements
`source.MetricsSource` (`internal/prometheus`). `GET /overview` (DSH-C5/C7),
`GET /services` (DSH-C4), `GET /services/{name}/series` (DSH-C6) and the log surface —
`GET /logs` (DSH-C8) and the SSE `GET /logs/tail` (DSH-C9), backed by the Loki client in
`internal/loki` — are live. The merged `GET /audit` (DSH-C11) is live too, fanning out to
Config's, admin-auth's and Session's own audit feeds through `internal/auditsrc`. The overview and
series responses share a bounded single-flight response cache. See "Scope" at the bottom.

Source of truth for this service: `design/01-dashboard.md` (§3 architecture, §4 API, §4a Go
notes), `design/00-common-stack.md` (COM-1…COM-14), and
`design/06-auth-identity-contract.md` §3 and §5 (the staff identity domain and the
`viewer < live_ops < admin` role ladder).

---

## Running it

```sh
export DASHBOARD_STAFF_JWKS_URL=http://localhost:8081/.well-known/jwks.json
# optional, and the defaults work in compose:
export DASHBOARD_STAFF_ISSUER=https://admin-auth.otomo.internal
export DASHBOARD_STAFF_AUDIENCE=otomo:staff

go run . serve     # or just: go run .
```

`serve` is the only command and the default. Unknown commands print usage and exit `2`. A
configuration error prints **every** problem at once and exits `1`. The service refuses to
start without a staff JWKS URL, because without one it cannot verify a single token.

admin-auth being undeployed does not stop start-up: the service starts, rejects every
token until keys arrive, and reports itself unready in the meantime.

```sh
docker build -t otomo-dashboard:staging .
docker run --rm --env-file dashboard.env -p 8080:8080 -p 9090:9090 otomo-dashboard:staging serve
```

The image is distroless and runs as nonroot. The internal port is not published in
deployment; only Gateway reaches the public one.

---

## Configuration

Every variable is read once at startup.

| Variable | Default | Notes |
|---|---|---|
| `DASHBOARD_LISTEN_ADDR` | `:8080` | Public listener; Gateway forwards `/api/admin/dashboard/*` without stripping the prefix. |
| `DASHBOARD_METRICS_ADDR` | `:9090` | Internal listener: `/healthz`, `/readyz`, `/metrics`, `/debug/pprof/`. |
| `DASHBOARD_STAFF_JWKS_URL` | — | **Required.** admin-auth's JWKS. |
| `DASHBOARD_STAFF_ISSUER` | `https://admin-auth.otomo.internal` | Checked against the token's `iss`. |
| `DASHBOARD_STAFF_AUDIENCE` | `otomo:staff` | Checked against the token's `aud`. |
| `DASHBOARD_STAFF_JWKS_REFRESH` | `30s` | JWKS re-fetch interval. |
| `DASHBOARD_JWT_CLOCK_SKEW` | `30s` | Leeway on `exp`/`nbf`; `0` is accepted. |
| `DASHBOARD_PROMETHEUS_URL` | `http://prometheus:9090` | Query-time only. |
| `DASHBOARD_LOKI_URL` | `http://loki:3100` | Query-time only. |
| `DASHBOARD_CONFIG_URL` | `http://config:8080` | Query-time only; base of Config's `/api/admin/config/audit`. |
| `DASHBOARD_ADMIN_AUTH_URL` | `http://admin-auth:8080` | Query-time only; base of admin-auth's `/admin-auth/audit`. |
| `DASHBOARD_SESSION_URL` | `http://session:8080` | Query-time only; base of Session's `/api/admin/session/audit` (SE-7). |
| `DASHBOARD_UPSTREAM_TIMEOUT` | `2s` | Per upstream HTTP call. |
| `DASHBOARD_TARGETS` | — | Comma-separated `name=url` pairs whose `/readyz` the prober polls. Empty is valid. |
| `DASHBOARD_PROBE_INTERVAL` | `15s` | How often every target is re-probed; minimum `1s`. |
| `DASHBOARD_READ_TIMEOUT` | `10s` | |
| `DASHBOARD_WRITE_TIMEOUT` | `30s` | |
| `DASHBOARD_IDLE_TIMEOUT` | `120s` | |
| `DASHBOARD_SHUTDOWN_TIMEOUT` | `15s` | |
| `DASHBOARD_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`. |

A malformed or non-positive value is not fatal on its own: its default is used and the
problem recorded, so one failed start-up names every variable that needs fixing.

---

## Query templates

The browser never sends PromQL. A client names a template and a service; the catalogue in
`internal/promql` owns every query string (design/01-dashboard.md §3, rule 2). `Build`
substitutes `{service}` and `{window}` only after the service has passed both the
allow-list of currently-scraped services and the label grammar `^[a-z][a-z0-9_-]{0,31}$`,
so a service name can never break out of its label value. The window is floored to whole
seconds and to a minimum of `60s`.

| Name | Title | Unit | Kinds |
|---|---|---|---|
| `rps_by_status` | Requests per second by status class | `req/s` | instant, range |
| `error_ratio` | 5xx error ratio | `ratio` | instant, range |
| `latency_p50` | Latency p50 | `s` | instant, range |
| `latency_p95` | Latency p95 | `s` | instant, range |
| `latency_p99` | Latency p99 | `s` | instant, range |
| `cpu` | CPU usage | `cores` | instant, range |
| `memory` | Memory working set | `bytes` | instant, range |
| `goroutines` | Goroutines | `count` | instant, range |
| `heap_inuse` | Heap in use | `bytes` | instant, range |
| `gc_pause_max` | GC pause max (summary quantile; p99 unavailable) | `s` | instant, range |

`gc_pause_max` reads `go_gc_duration_seconds{quantile="1"}`: client_golang's default Go
collector exposes a summary, not a histogram, so the maximum recorded stop-the-world pause
is the closest thing to a p99 that exists. A true p99 is not available from that metric.

**`service` label requirement:** the scrape config must attach a
`service="<name>"` label to every target. Both the allow-list (built from
`GET /api/v1/targets`) and every per-service metric selector rely on it; a target without
the label is invisible to the Dashboard. HTTP metrics are selected by name too —
`{__name__=~".+_http_requests_total",service="…"}` and the
`_bucket` equivalent for latency — so services must keep the
`<svc>_http_requests_total{route,method,status}` and
`<svc>_http_request_duration_seconds_bucket{route,method,le}` names the template uses.

`internal/prometheus` is the `source.MetricsSource` implementation behind those queries:
`Instant` and `Range` bound every call with `DASHBOARD_UPSTREAM_TIMEOUT`, read at most
16 MiB of body, and render each series as a stable sorted label string with `__name__`
dropped. `Range` returns the shared timestamp grid and per-series value slices uPlot
consumes, with gaps filled as `NaN`. `Targets` feeds `GET /services`; `AllowList` caches
the scraped-service set for 30 s with a single refresh in flight, and a failed refresh
keeps the last good set.

---

## Overview (DSH-C5 / DSH-C7)

`GET /overview` runs one fixed batch of instant queries — no client parameters at all — and
merges them into the cards the admin WebUI parses. The queries live in `promql.Overview()`,
one query per card across every service, grouped `by (service)`:

| Card | Query |
|---|---|
| `up` | `max by (service) (up{service!=""})` |
| `rps` | `sum by (service) (rate({__name__=~".+_http_requests_total",service!=""}[5m]))` |
| `error_ratio` | 5xx rate / total rate `by (service)`, with `clamp_min(…, 1e-9)` guarding divide-by-zero |
| `p95_ms` | `1000 * histogram_quantile(0.95, sum by (service, le) (rate(…_bucket[5m])))` |
| `version` | `max by (service, version) ({__name__=~".+_build_info",service!=""})`; the handler reads the `version` label |
| `host.cpu` | `1 - avg(rate(node_cpu_seconds_total{mode="idle"}[1m]))` |
| `host.mem` | `1 - sum(node_memory_MemAvailable_bytes) / sum(node_memory_MemTotal_bytes)` |
| `host.disk` | `1 - sum(node_filesystem_avail_bytes{mountpoint="/"}) / sum(node_filesystem_size_bytes{mountpoint="/"})` |
| `online_players` | `sum(otomo_online_players)` |

The `services` array is the union of Prometheus's scraped targets (`AllowList`) and the
prober's targets, sorted by name. `up` prefers Prometheus's `up` when a series exists for
the service and falls back to the prober's `up`; `ready`/`reason` always come from the
prober (`ready:false, reason:"not probed"` when the prober does not know the service).
Every query runs concurrently under one 2 s deadline derived from the request context.

**Degraded semantics.** A query failure or timeout nulls its field(s) and adds the card's
identifier (`services.p95_ms`, `host.disk`, `online_players`, …) to `degraded`; a host card
whose query returns nothing is degraded too, which is the expected state of
`online_players` until a service exports the metric. A service whose query returned nothing
gets `null` for that field but does not by itself degrade the card. A failing query never
fails the response: the handler always answers `200`. Even an unreachable Prometheus yields
`200` with every field null and every card degraded — the page renders degraded cards
rather than nothing.

**Cache.** Responses are cached as pre-serialised JSON `[]byte`, keyed by
`endpoint | time bucket` (`now.Unix()/10`), TTL 10 s, at most 64 entries with the oldest
evicted. A `singleflight` group collapses concurrent misses so ten simultaneous overview
requests run exactly one batch. A degraded response is cached like any other. The handler
sets `Cache-Control: no-store`, so the browser re-requests every time while the service
absorbs the load. The clock is injectable, so the bucket and TTL are tested without
sleeping.

---

## Series (DSH-C6)

`GET /services/{name}/series?metric=&from=&to=&step=` runs one range query for one named
template against one service and returns the columnar shape uPlot consumes directly
(`design/01-dashboard.md` §4). The metric must name a catalogue template — the browser still
never sends PromQL — the service must pass both the allow-list and `promql`'s label grammar,
and the query is built by `Template.Build` as everywhere else. An unknown metric or service
is `400 unknown_metric` / `400 unknown_service`; an unparsable or inverted window is `400
validation_failed`; a failed allow-list fetch with no last-good set is `503
upstream_unavailable`; a Prometheus error or timeout is `502 upstream_error` with a short,
URL-free message. An allow-list refresh that fails but still carries the last good set keeps
serving.

`from`/`to` accept RFC3339 or whole unix seconds; `to` defaults to now and is clamped to now
when a client clock runs ahead, and `from` defaults to `to-1h`. A window wider than seven
days is clamped to seven days and the response's `clamped` flag says so. The step is the
larger of the request (floored to 15 s), that points-based floor, and `ceil((to-from)/1500)`,
rounded up to a whole second, so the response carries at most 1500 points; `from` is aligned
down to a multiple of the step and the rate window passed to `Build` is `max(4*step, 60s)`.
The response's series are named by their single grouping label (`class` → `2xx`), by the
rendered label key when there is more than one, or by the template name for one unlabelled
series, and are sorted by name; a `NaN` gap is `null`.

**Cache.** The series handler shares the overview's bounded cache: pre-serialised JSON keyed
by `endpoint | service | metric | aligned from | to | step`, TTL 10 s, singleflight on
concurrent misses, and `Cache-Control: no-store` on the wire.

---

## Logs and live tail (DSH-C8 / DSH-C9)

`internal/loki` implements `source.LogSource` against Loki's HTTP API with plain `net/http`:
`GET /loki/api/v1/query_range` for search and `GET /loki/api/v1/label/service/values` for the
service allow-list. Every call is bounded by `DASHBOARD_UPSTREAM_TIMEOUT`, reads at most
16 MiB, and maps a Loki error envelope onto a short, URL-free error. A response's
`data.result[].stream` labels provide `service`/`level`; each `values` pair `[ns, line]`
becomes a `source.LogLine`. A line that is a JSON object is parsed into `Fields`, and its
`msg` field (when present) becomes `Message`; a plain line stays its own message. Streams are
merged and sorted newest first (oldest first for the forward tail poll), then cut to the
limit. The service label list is cached for 30 s with one refresh in flight and the last good
set kept on error, mirroring the Prometheus allow-list.

**LogQL rules.** The builder in `internal/loki/logql.go` is the only place LogQL is written
(design/01-dashboard.md §3, rule 2). It is a pure function tested exhaustively for escaping and
injection. The shape is a stream selector followed by line filters:

```
{service="gateway",level="error"} |= "publish" |= "request_id\":\"req-1\""
```

- An empty `service` selects every stream (`{service=~".+"}`); a named service must match
  `^[a-z][a-z0-9_-]{0,31}$` **and** be in the current Loki label-value set.
- `level` must be one of `debug`, `info`, `warn`, `error` (empty means no filter).
- `contains` is applied as `|= "<value>"`, quoted as a Go/LogQL double-quoted string so a
  quote, brace or backslash is data rather than syntax; a newline is rejected. An injection
  attempt such as `"} |= "x" or {job=~".+` is a literal.
- `request_id` must match `^[A-Za-z0-9_-]{1,64}$` and is matched as the literal JSON
  substring `request_id":"<id>"`, not parsed.
- `| json` is deliberately **not** used: Alloy indexes only `service` and `level`, and a
  parser stage would make every search scan every stream.

**`GET /logs?service=&level=&contains=&request_id=&from=&to=&limit=`** returns
`{"entries":[{at,service,level,message,fields}]}` newest first. `from`/`to` accept RFC3339 or
whole unix seconds; `to` defaults to now (clamped to now) and `from` to `to-1h`. The window
must be non-empty and at most 7 days, and `limit` defaults to 200 and must be 1..1000 — any
bad value is `400 validation_failed`, including an unknown service or level and a newline in
`contains`. `fields` is omitted when the line was not JSON. A Loki failure is
`502 upstream_error`.

**`GET /logs/tail?service=&level=&contains=`** is Server-Sent Events. The parameters are
validated as above before any stream byte, and the concurrency caps are checked before that:
**2 live tails per staff subject and 10 globally**, answered `429 too_many_tails` in the
COM-5 JSON envelope. The handler sets `Content-Type: text/event-stream`,
`Cache-Control: no-store` and `X-Accel-Buffering: no`, clears the listener's write deadline
with `ResponseController.SetWriteDeadline(time.Time{})`, and writes `: connected`, flushes,
then each line as `data: <json>\n\n`. A `: keep-alive` comment is flushed every 15 s. The
stream ends on client disconnect (`r.Context().Done()`), which cancels the Loki poll, or
after the optional 1 h hard cap. The limiter is a small mutex-guarded type with its own
tests; a tail slot is released by `defer` when the handler returns.

---

## Merged audit (DSH-C11)

`internal/auditsrc` is one client per feed: Config at `GET /api/admin/config/audit`, PHP
Admin Auth at `GET /admin-auth/audit`, Session at `GET /api/admin/session/audit` (SE-7). All
three speak the same page shape
(`{"entries":[{id,at,actor_id,actor_name,source,action,target,details}],"next_cursor"}`),
newest first by id, with an opaque `cursor` and `limit`/`actor`/`from`/`to` filters. The
client forwards the caller's own bearer token and **nothing else** from the request, bounds
every call with `DASHBOARD_UPSTREAM_TIMEOUT`, reads at most 4 MiB and decodes strictly. A
`401`/`403` from a feed is propagated as that status; any other failure is a per-feed error.

**`GET /audit?source=&actor=&from=&to=&limit=&cursor=`** includes `source=config`,
`source=admin-auth`, `source=session`, or all three when `source` is empty. `limit` defaults to 50 and must be
1..200; `from`/`to` are RFC3339; an unknown source, a bad `limit`/`from`/`to` or a malformed
cursor is `400 validation_failed`. One page (size `limit`) is fetched from each included
feed concurrently, and the feeds are k-way merged by `at` descending (ties broken by source
name, then id descending) and cut to `limit`. A feed whose page runs out mid-merge is
refilled from its next cursor before an older entry from the other feed can be chosen, so
the order holds even when a page boundary falls inside a run of equal timestamps. Each entry
keeps its upstream `source` field.

**Composite cursor.** `next_cursor` is base64url(JSON) of
`{"config":{"cursor":"<opaque>","skip":n},"admin-auth":{…}}`; a key is present only for a
feed in the session, and `"done":true` marks an exhausted one. `cursor` is the upstream page
the feed was reading and `skip` is how many of that page's entries have already been
returned, so a resume refetches that page and drops the first `skip`. Because both feeds are
append-only and a cursor names a position below which no later entry can appear, the
refetched page is identical to the one the skip was counted against — no entry is lost or
duplicated across pages. `next_cursor` is null only when every included feed is exhausted.

When one feed fails for any reason other than 401/403, its entries are omitted and its name
is listed in `"degraded":["admin-auth"]`; the other feed's entries are still returned with
`200`. When every included feed fails the request is `502 upstream_error`. The body is
`{"entries":[…],"next_cursor":…,"degraded":[…]}`.

---

## API

All routes are under `/api/admin/dashboard`, require a staff token, and require role
`viewer` or higher. Every route in the table below is live.

| Method & path | Returns | Ticket |
|---|---|---|
| `GET /overview` | Per service `up`, `rps`, `error_ratio`, `p95_ms`, `version`; host CPU/mem/disk; `online_players`; `degraded` | DSH-C5/C7 |
| `GET /services` | Known services and their `/readyz` health | DSH-C4 |
| `GET /services/{name}/series?metric=&from=&to=&step=` | Columnar time series for one named template | DSH-C6 |
| `GET /logs?service=&level=&contains=&request_id=&from=&to=&limit=` | Log lines, newest first, max 1000 | DSH-C8 |
| `GET /logs/tail?service=&level=&contains=` | SSE stream of new log lines | DSH-C9 |
| `GET /audit?source=&actor=&from=&to=&cursor=` | Merged audit entries from Config and PHP, paginated | DSH-C11 |

Every route, **including the fallback for unmatched paths**, is behind the token guard, so
an anonymous caller cannot tell a real path from an invented one. A missing or unusable
token is `401`; a correctly-issued token without a sufficient role is `403`; an unknown path
with a valid token is `404`; the wrong method on a real path is `405` with `Allow`.

### Readiness

`/readyz` is `503` until the staff JWKS has at least one key, and `200` after. Prometheus
and Loki reachability is **not** part of readiness: a down upstream must degrade a panel,
not take the Dashboard out of rotation. `/healthz` never touches either.

### Metrics

Own metrics are on the internal listener under the `dashboard_` prefix:
`dashboard_http_requests_total{route,method,status}`,
`dashboard_http_request_duration_seconds{route,method}`,
`dashboard_token_rejected_total{reason}`, `dashboard_build_info{version}`,
`dashboard_service_up{service}`, `dashboard_service_ready{service}`,
`dashboard_probe_duration_seconds{service}`,
`dashboard_upstream_requests_total{upstream,result}` and
`dashboard_upstream_duration_seconds{upstream}` (DSH-C12),
`dashboard_cache_requests_total{endpoint,result}`, `dashboard_tail_streams` and
`dashboard_overview_degraded_total{card}`. `route` is the mux pattern, never the raw
path; `service` is a configured target name. The upstream, cache and degraded labels come
only from closed sets declared in `internal/server` and its callers — never a service name
or URL — and every closed label set is pre-initialised at 0. The instruments are fed
through small observer seams (`internal/prometheus`, `internal/loki`, `internal/auditsrc`,
`internal/cache`, `internal/api`) so none of those packages imports a metrics library.

See `TESTING.md` for the acceptance-criteria-to-test map.

---

## Layout

```
dashboard.go            serve entry point: config → logger → verifier → prober → server
TESTING.md              DSH-C1…C12 / DSH-H1 acceptance criteria mapped to tests
internal/config/        environment parsing and validation
internal/auth/          staff-token verification against admin-auth's JWKS
internal/api/           COM-5 errors, health/readiness, route table, handler seam, overview, series, logs, audit
internal/auditsrc/      the Config, admin-auth and Session audit feed clients behind GET /audit
internal/cache/         bounded TTL response cache with single-flight misses
internal/health/        /readyz prober: concurrent polling and per-target status
internal/promql/       named PromQL template catalogue and service allow-list grammar
internal/prometheus/   source.MetricsSource over Prometheus's HTTP API
internal/loki/         source.LogSource over Loki's HTTP API, the LogQL builder
internal/source/        MetricsSource and LogSource interfaces
internal/server/        listeners, middleware chain, metrics, token guard
```

`internal/source` is the seam the transports implement: `MetricsSource` has its Prometheus
implementation, handed to the overview batch and the series handler, and `LogSource` has its
Loki implementation, handed to the log search and tail handlers. Defining them apart from
the transports keeps handlers testable against a fake, with no Prometheus or Loki process.

---

## Tests

```sh
gofmt -l .
go vet ./...
go test -race -count=1 ./...
go build -o /tmp/dashboard-build .
```

The server tests spin up both listeners and sign real Ed25519 staff tokens against a
throwaway JWKS, so the `401`/`403`/`501` boundaries are asserted against verification
rather than against a stub. The overview tests use a scriptable fake
`source.MetricsSource` that counts calls and can delay or fail per query: they assert the
field mapping, per-card degradation, the 2 s deadline, that ten concurrent requests run one
batch, that a request inside the bucket hits the cache and one after the roll does not, and
that the handler's own overhead is well under 300 ms. The series tests add a step table and
point cap, the 30-day clamp with alignment, the `400`/`502`/`503` boundaries (including a
label-grammar injection attempt that never reaches Prometheus), the columnar `NaN → null`
shape, series naming and sorting, and a cache hit. `internal/cache` and `internal/promql`'s
overview batch have their own table and golden tests.

The Loki client is tested against `httptest` fixtures: a two-stream `query_range` merges
newest first, a JSON line yields `Fields` and its `msg`, an error envelope surfaces as an
error, and the label-value cache is exercised without a long sleep. The LogQL builder has
golden tests for the selector, escaping and the injection string, plus the validation table.
The tail is tested with a fake Loki whose results grow: every line is delivered exactly once
across the poll boundary and cancellation stops polling. The log handlers add a parameter
validation table, the response shape (including `fields` omitted when nil) and the `502`.
Finally, the SSE handler is exercised through a real `httptest.Server` with a real viewer
token: `: connected` and events arrive, a third tail by one subject is `429 too_many_tails`
while two are open, an eleventh global tail is refused, a disconnected client's slot is freed
(bounded wait), and the keep-alive comment is observed with an injected 50 ms interval.

The audit client is tested against `httptest` fixtures for token forwarding, the exact
upstream query, `401`/`403` propagation, the 4 MiB cap and strict decode. The merge has a
property test: two fake feeds with random timestamps (equal ones included) are walked page
by page at several limits and must yield every entry exactly once in global order. The
handler tests cover interleaving, a degraded feed, `502` when both fail, `source` selection
and the `400` validation table; the server test walks the real route with a viewer token.

---

## Scope

The Dashboard is a query layer, not a data store. It does not scrape Prometheus, ingest
logs, or hold a database. In M1 it reports service health, per-service series, log search
and tail, host/container resources, online player count and the audit trail; alerting,
tracing and player-level analytics are out of scope. Phase C is where the remaining
business handlers land; the overview and the access-control boundaries they sit behind are
live.
