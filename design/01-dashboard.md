# Milestone 1 — Dashboard

**Owner domain:** Staff (admin-auth tokens only)
**Prerequisites:** `00-common-stack.md` tasks COM-1 → COM-10 and WEB-1 → WEB-7

---

## 1. Purpose and scope

The Dashboard gives staff a live view of the health of every essential service: whether it is up, how fast it responds, how often it fails, what it is logging, and who changed what.

**In scope for M1**

- Service health overview (up/down, request rate, error rate, p95 latency)
- Per-service detail charts
- Log search and live log tail
- Host and container resource usage (CPU, memory, disk)
- Audit trail viewer (config publishes, rollbacks, staff account actions)
- Online player count (from Session)

**Out of scope for M1**

- Alerting and on-call notifications (Alertmanager, next milestone)
- Distributed tracing
- Match and gameplay-server metrics (arrive with Allocator)
- Player-level analytics (retention, funnels)

**Key design decision:** you are not building a metrics database or a log index. Proven open-source tools store and query the data. The Dashboard service is a thin, access-controlled query layer, and the WebUI is the live-ops-specific presentation. That is the part worth showcasing.

---

## 2. Tools you will need

### 2.1 Observability stack

| Tool | What it is | Role here |
|---|---|---|
| **Prometheus** | Time-series database that periodically *scrapes* (HTTP GET) each service's `/metrics` endpoint | Stores all numeric metrics |
| **Loki** | Log database that indexes only labels (service, level), not full text, so it stays small | Stores all service logs |
| **Grafana Alloy** | Collector agent (Grafana's OpenTelemetry Collector distribution) | Reads Docker container logs and ships them to Loki. Replaces Promtail, which is end-of-life |
| **node_exporter** | Prometheus exporter for the host machine | VM CPU, memory, disk, network |
| **cAdvisor** | Container resource exporter | Per-container CPU and memory |
| **postgres_exporter** | Prometheus exporter for PostgreSQL | Connections, query rates, database size |
| **Grafana** (optional, internal only) | Generic dashboard UI | Your own debugging while building queries. Not exposed to staff, not the product |

Config validation tools used in CI: `promtool check config` (ships with Prometheus) and `alloy fmt` (ships with Alloy).

### 2.2 Query languages you will write (server-side only)

- **PromQL**: Prometheus query language. Example: `histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket{service="gateway"}[5m])))` gives p95 latency.
- **LogQL**: Loki query language. Example: `{service="config", level="error"} |= "publish"`.

### 2.3 Frontend libraries

| Library | What it is | Why |
|---|---|---|
| **uPlot** | Very small, very fast time-series chart library using columnar arrays | Renders thousands of points cheaply; data format matches what the backend returns |
| **TanStack Virtual** | List virtualization (renders only visible rows) | Log viewer with tens of thousands of lines |
| Browser `EventSource` API | Built-in Server-Sent Events client | Live log tail |

Apache ECharts is a heavier alternative to uPlot if you want pie/heatmap charts later.

---

## 3. Architecture

```
          ┌───────────┐  scrape /metrics every 15s
          │Prometheus │◄──────────────── Auth, Gateway, admin-auth, Config, Patch, Session,
          └─────┬─────┘                  node_exporter, cAdvisor, postgres_exporter
                │ HTTP API
┌─────────┐   ┌─┴───────────┐   ┌──────────┐    ┌──────────────┐
│ Ionic   │──►│  Gateway    │──►│Dashboard │───►│ Loki         │◄── Alloy ◄── Docker
│ WebUI   │   │(staff token)│   │ service  │    └──────────────┘     container logs
└─────────┘   └─────────────┘   └────┬─────┘
                                     │ REST (staff token forwarded)
                                     ▼
                              Config /audit, admin-auth /audit
```

Rules:

1. The browser never talks to Prometheus or Loki directly. Neither has user authentication, and arbitrary queries can exhaust the VM.
2. The Dashboard service only runs **named, parameterized query templates**. Clients choose a template, service and time range; they never send PromQL or LogQL.
3. Audit data stays owned by the service that produced it. Dashboard reads it over REST and merges the results; it does not connect to other services' databases.

---

## 4. API contract

All routes are under `/api/admin/dashboard`, require a staff token, and require role `viewer` or higher.

| Method & path | Returns |
|---|---|
| `GET /overview` | Per service: `up`, `rps`, `error_ratio`, `p95_ms`, `version`; plus host CPU/mem/disk, `online_players`, and `alerts`: the **firing** Prometheus alerts (`name`, `severity`, `service`, `summary`, `active_at`), critical first. This is how alerts reach staff in M1; there is no Alertmanager. A failed alerts query gives `[]` and `"alerts"` in `degraded` |
| `GET /services` | List of known services and their scrape status |
| `GET /services/{name}/series?metric={template}&from=&to=&step=` | Time series for one named template |
| `GET /logs?service=&level=&contains=&from=&to=&limit=` | Log lines, newest first, max 1000 |
| `GET /logs/tail?service=&level=` | SSE stream of new log lines |
| `GET /audit?source=&actor=&from=&to=&cursor=` | Merged audit entries from Config and admin-auth, paginated |

**Time-series response format (columnar):**

```json
{
  "t": [1789000000, 1789000015, 1789000030],
  "series": [
    { "name": "2xx", "values": [120.5, 118.0, 131.2] },
    { "name": "5xx", "values": [0.0, 0.2, 0.0] }
  ]
}
```

One shared timestamp array and one contiguous array per series. This is what uPlot consumes directly, and it avoids an array of `{t, v}` objects per point, which is larger on the wire and slower to parse.

---

## 4a. Go implementation notes

- **Prometheus and Loki clients:** plain `net/http` calls to their HTTP APIs with a shared `http.Client` that has a timeout. `prometheus/client_golang/api` offers a typed Prometheus client if you prefer; Loki has no official Go client worth taking on, so call its HTTP API directly.
- **Overview fan-out (DSH-C5):** run the batch of instant queries concurrently with `sync.WaitGroup` (or `errgroup`) under one request context with a 2 s deadline; a slow query degrades one card instead of the whole response.
- **Response cache (DSH-C7):** store pre-serialized response `[]byte` keyed by endpoint + params + time bucket. Use a single-flight pattern (`golang.org/x/sync/singleflight`) so concurrent cache misses trigger one upstream query.
- **Columnar encoding:** decode Prometheus's `[[timestamp, "value"], …]` pairs directly into preallocated `[]int64` and `[]float64` slices (capacity from the known point count), then encode once.
- **SSE (DSH-C9):** set `Content-Type: text/event-stream`, write each event as `data: …\n\n`, and flush with `http.NewResponseController(w).Flush()`. Clear the write deadline for this handler with `ResponseController.SetWriteDeadline(time.Time{})`, since the template's server-wide write timeout would otherwise kill long streams. Exit the loop on `r.Context().Done()`.
- **Tail limits:** a per-user counter in a mutex-guarded map, decremented in a `defer`.

## 5. Task breakdown

### Phase A — Observability infrastructure

| ID | Task | Acceptance criteria | Depends on |
|---|---|---|---|
| DSH-A1 | Add Prometheus to compose with a volume, `prometheus.yml` in the repo, scrape interval 15s, retention set via `--storage.tsdb.retention.time` (start at 7d) and `--storage.tsdb.retention.size` below free disk | Prometheus targets page shows all services; restart keeps data | COM-7 |
| DSH-A2 | Add Loki (single-binary mode) with filesystem storage, retention enabled via compactor, start at 72h | Loki `/ready` returns 200; old logs are deleted after retention | COM-7 |
| DSH-A3 | Add Grafana Alloy: discover Docker containers, parse JSON log lines, promote only `service` and `level` to labels, push to Loki | Logs from every service are queryable in Loki by `service` and `level` | DSH-A2, COM-10 |
| DSH-A4 | Add node_exporter, cAdvisor, postgres_exporter; add them as scrape targets | Host CPU, per-container memory and Postgres connections visible in Prometheus | DSH-A1 |
| DSH-A5 | Jenkins validation stage: `promtool check config` and `alloy fmt` on every change to observability configs | Invalid config fails the build before deploy | DSH-A1, DSH-A3 |
| DSH-A6 | (Optional) Internal Grafana on an unpublished port for query development | Accessible only via SSH tunnel | DSH-A1, DSH-A2 |
| DSH-A7 | Size disk budget: measure bytes/day for metrics and logs after 48h of normal traffic; adjust retention | Documented numbers; projected disk use under 50% of volume | DSH-A1 → A3 |

### Phase B — Instrumentation of existing services

| ID | Task | Acceptance criteria | Depends on |
|---|---|---|---|
| DSH-B1 | Standard HTTP metrics in the service template: `http_requests_total{route,method,status}` counter and `http_request_duration_seconds{route,method}` histogram. `route` is the route *pattern* (`/players/{id}`), never the raw path | Present on every service built from the template | COM-2, COM-10 |
| DSH-B2 | Build info gauge: `build_info{version,commit}` = 1 | Overview can show deployed version per service | DSH-B1 |
| DSH-B3 | Auth: `auth_login_total{result}`, `auth_token_issued_total`, `auth_jwks_requests_total` | Visible in Prometheus | DSH-B1 |
| DSH-B4 | admin-auth: `staff_login_total{result}` and `staff_refresh_total{result}`, exported from the Go service (`services/admin_auth`) through its own registry like every other backend — no PHP runtime, so no FPM-worker shared-memory workaround (in review) | Counters visible in Prometheus | DSH-B1 |
| DSH-B5 | Gateway: upstream latency and status per route group; `gateway_token_rejected_total{reason,group}` | Cross-issuer rejections are countable | DSH-B1 |
| DSH-B6 | Audit endpoints: Config exposes `GET /api/admin/config/audit`; admin-auth exposes `GET /admin-auth/audit` (in review); same entry shape `{id, at, actor_id, actor_name, source, action, target, details}` | Both return paginated entries with a cursor | Config doc CFG-B9 |

### Phase C — Dashboard backend service

| ID | Task | Acceptance criteria | Depends on |
|---|---|---|---|
| DSH-C1 | Create service from template; register Gateway route `/api/admin/dashboard/*` with staff issuer | `/healthz` reachable through Gateway with a staff token only | COM-2, COM-4 |
| DSH-C2 | Re-verify the staff token and role in the service (defense in depth; Gateway misconfiguration must not expose data) | Direct call bypassing Gateway without token returns 401 | DSH-C1 |
| DSH-C3 | Query template catalog: a static table mapping template name → PromQL string with `{service}` and `{range}` placeholders. Service names validated against the known-service list before substitution | Unknown template or service returns 400; no user string is concatenated into PromQL | DSH-C1 |
| DSH-C4 | Prometheus client: call `/api/v1/query` and `/api/v1/query_range`, convert results to the columnar format | Unit test converts a recorded Prometheus response correctly | DSH-C3 |
| DSH-C5 | `GET /overview`: runs a fixed batch of instant queries concurrently, merges into one response | Responds in under 300 ms at M1 scale | DSH-C4 |
| DSH-C6 | `GET /services/{name}/series`: clamp range to max 7d and choose `step` so any response has ≤ 1500 points per series | 30-day request is clamped; point count never exceeds the cap | DSH-C4 |
| DSH-C7 | Response cache keyed by (endpoint, params, time bucket), TTL 10s, bounded size | Ten concurrent overview requests produce one set of Prometheus queries | DSH-C5 |
| DSH-C8 | Loki client and `GET /logs`: build LogQL from validated `service`/`level` labels; `contains` is escaped and applied as a line filter; limit ≤ 1000 | Injection attempt in `contains` is treated as literal text | DSH-A2, DSH-C1 |
| DSH-C9 | `GET /logs/tail` as Server-Sent Events: poll Loki for new lines every 1–2 s per active stream, or bridge Loki's tail endpoint. Cap concurrent tails per staff user (e.g. 2) and globally (e.g. 10) | Third tail from one user is rejected with 429; stream closes cleanly on client disconnect | DSH-C8 |
| DSH-C10 | Configure Gateway for SSE on this route: disable response buffering, read timeout ≥ 1 h, send a keep-alive comment every 15 s | Tail stream stays open for 10 minutes without dropping | DSH-C9 |
| DSH-C11 | `GET /audit`: fan out to Config and admin-auth audit endpoints (forwarding the caller's staff token), merge by timestamp, return a combined cursor. | Entries from both sources appear interleaved in correct order | DSH-B6 |
| DSH-C12 | Dashboard's own metrics: upstream query latency and cache hit ratio | Dashboard appears in its own overview | DSH-B1 |

### Phase D — Frontend module (inside the admin WebUI)

| ID | Task | Acceptance criteria | Depends on |
|---|---|---|---|
| DSH-D1 | Overview page: card per service (status dot, rps, error ratio, p95, version) plus host resource strip and online player count | Auto-refreshes every 15 s | DSH-C5, WEB-5 |
| DSH-D2 | Pause polling when the tab is hidden (Page Visibility API) and resume on focus | Background tabs generate zero requests | DSH-D1 |
| DSH-D3 | Service detail page with uPlot charts: request rate by status class, p50/p95/p99 latency, error ratio, container CPU/memory | Charts render 1500 points × 4 series without visible lag | DSH-C6 |
| DSH-D4 | Time-range picker (15m, 1h, 6h, 24h, 7d) shared across charts via URL query parameters | Shareable URL reproduces the same view | DSH-D3 |
| DSH-D5 | Log explorer: filters for service, level, text, time range; virtualized list; expand a line to see full JSON; click `request_id` to filter by it | 10,000 loaded lines scroll smoothly | DSH-C8 |
| DSH-D6 | Live tail view: start/stop button, auto-scroll that pauses when the user scrolls up, client-side cap of 5,000 lines (drop oldest) | Memory stays flat during a 10-minute tail | DSH-C9 |
| DSH-D7 | Audit trail page: table with actor, action, target, time; link config publish entries to the Config diff view | Clicking a publish entry opens the matching diff | DSH-C11, CFG-D5 |
| DSH-D8 | Empty, loading and error states for every panel (Prometheus down must not blank the whole page) | Stopping Loki shows an error on the log panel only | DSH-D1 → D7 |

### Phase E — Testing and delivery

| ID | Task | Acceptance criteria | Depends on |
|---|---|---|---|
| DSH-E1 | Unit tests: template substitution, step calculation, Prometheus/Loki response conversion (using recorded fixtures) | Run in Jenkins on every commit | Phase C |
| DSH-E2 | Hurl contract tests: staff `viewer` token succeeds; player token 401; unknown template 400 | Run in Jenkins against a compose test stack | Phase C |
| DSH-E3 | k6 test: 20 simulated dashboards polling overview every 15 s for 10 minutes | Prometheus CPU increase < 10%; p95 overview latency < 300 ms | DSH-C7 |
| DSH-E4 | Jenkins pipeline for the Dashboard service using COM-8 | Push to main deploys; failed readiness rolls back | COM-8 |

---

## 6. Definition of done

- Every M1 service, the host, containers and Postgres appear on the overview with correct status.
- Stopping any service container turns its card red within 30 seconds.
- A staff member can find the error logs for a failed request by `request_id` within a minute.
- A config publish performed in the Config module appears in the audit trail.
- A player token cannot reach any Dashboard endpoint.

## 7. Risks

| Risk | Mitigation |
|---|---|
| High-cardinality labels (player IDs, raw URLs) explode Prometheus memory | COM-10 label rules; review `/metrics` output in code review |
| Logs fill the VM disk | Retention limits (DSH-A1, A2) and measured budget (DSH-A7) |
| Unbounded queries from the UI overload Prometheus | Named templates, range clamping, point caps, caching |
| SSE connections silently cut by Gateway timeouts | DSH-C10 keep-alives and timeout configuration |
