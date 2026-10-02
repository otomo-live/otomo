package api

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/otomo-live/otomo/services/dashboard/internal/cache"
	"github.com/otomo-live/otomo/services/dashboard/internal/health"
	"github.com/otomo-live/otomo/services/dashboard/internal/prometheus"
	"github.com/otomo-live/otomo/services/dashboard/internal/promql"
)

// overviewPath is the external path for GET /overview.
const overviewPath = "/api/admin/dashboard/overview"

// overviewPattern is the ServeMux pattern for overviewPath, compared by For while the mux
// is assembled.
const overviewPattern = http.MethodGet + " " + overviewPath

// The closed endpoint label values for dashboard_cache_requests_total. They name the two
// cache-backed routes; a new cached route adds one here and to the metric's
// pre-initialised set.
const (
	cacheEndpointOverview = "overview"
	cacheEndpointSeries   = "series"
)

const (
	// overviewTimeout is the one deadline the whole batch runs under. A query that misses
	// it degrades its card; the response still returns.
	overviewTimeout = 2 * time.Second
	// overviewBucket is the width of the cache's time bucket. Everything requested inside
	// one bucket shares a key, which is what turns 20 dashboards polling together into one
	// Prometheus batch.
	overviewBucket = 10 * time.Second
)

// overviewResponse is the wire shape the admin WebUI parses. Numbers are pointers so a
// failed or empty query renders as JSON null rather than a misleading zero. Alerts are how
// the Dashboard delivers Prometheus alerts in M1: the stack runs no Alertmanager, so the
// firing rule instances themselves are surfaced here.
type overviewResponse struct {
	GeneratedAt   string            `json:"generated_at"`
	Services      []serviceOverview `json:"services"`
	Host          hostUsage         `json:"host"`
	OnlinePlayers *float64          `json:"online_players"`
	Alerts        []alertSummary    `json:"alerts"`
	Degraded      []string          `json:"degraded"`
}

// alertSummary is one firing Prometheus alert, flattened to the fields the overview
// renders. Pending alerts are left out because they are still inside their for: window and
// are not yet something staff must act on.
type alertSummary struct {
	Name     string `json:"name"`
	Severity string `json:"severity"`
	Service  string `json:"service"`
	Summary  string `json:"summary"`
	ActiveAt string `json:"active_at"`
}

// serviceOverview is one service card.
type serviceOverview struct {
	Name       string   `json:"name"`
	Up         bool     `json:"up"`
	Ready      bool     `json:"ready"`
	Reason     string   `json:"reason"`
	RPS        *float64 `json:"rps"`
	ErrorRatio *float64 `json:"error_ratio"`
	P95Ms      *float64 `json:"p95_ms"`
	Version    string   `json:"version"`
}

// hostUsage is the host resource strip.
type hostUsage struct {
	CPURatio  *float64 `json:"cpu_ratio"`
	MemRatio  *float64 `json:"mem_ratio"`
	DiskRatio *float64 `json:"disk_ratio"`
}

// responseCache returns the handler's cache, building it on first use. The clock is the
// handler's own so a test that advances it expires the cache and rolls the bucket together.
func (h *Handlers) responseCache() *cache.Cache {
	h.cacheOnce.Do(func() {
		c := cache.New(cache.DefaultTTL, cache.DefaultMaxEntries)
		if h.Now != nil {
			c.SetClock(h.Now)
		}
		if h.Observer != nil {
			c.SetObserver(h.Observer)
		}
		h.cache = c
	})
	return h.cache
}

// overviewCacheKey is the documented cache key: endpoint, canonical parameters (none here)
// and the 10-second time bucket. Two requests in the same bucket and behind the same
// endpoint are the same response.
func overviewCacheKey(now time.Time) string {
	bucket := now.Unix() / int64(overviewBucket/time.Second)
	return overviewPattern + "|" + strconv.FormatInt(bucket, 10)
}

// overview answers GET /overview: one concurrent batch of fixed instant queries merged into
// the cards the WebUI renders. No query failure reaches the caller as an HTTP error; a
// failed or timed-out query nulls its field and adds its card to degraded, so a Prometheus
// outage shows a page full of degraded cards rather than no page.
func (h *Handlers) overview(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	if h.Now != nil {
		now = h.Now()
	}

	body, err := h.responseCache().Do(r.Context(), cacheEndpointOverview, overviewCacheKey(now), func(batchCtx context.Context) ([]byte, error) {
		return h.buildOverview(batchCtx, now)
	})
	if err != nil {
		WriteError(w, r, http.StatusInternalServerError, "overview_failed", "could not build the overview")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	// The browser must not cache the page; this service's cache is the only one, and it
	// is explicitly bounded and short-lived.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// buildOverview runs the batch and serialises the response. The batch runs under one 2 s
// deadline derived from ctx, with every query independent: one slow or failing query
// degrades exactly its own card.
func (h *Handlers) buildOverview(ctx context.Context, now time.Time) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, overviewTimeout)
	defer cancel()

	var (
		mu         sync.Mutex
		values     = make(map[string]map[string]float64)
		versions   = make(map[string]string)
		hostValues = make(map[string]*float64)
		alerts     []prometheus.Alert
		degraded   []string
		allow      promql.ServiceSet
	)

	markDegraded := func(card string) {
		if h.Observer != nil {
			h.Observer.OverviewDegraded(card)
		}
		mu.Lock()
		degraded = append(degraded, card)
		mu.Unlock()
	}

	var wg sync.WaitGroup
	for _, q := range promql.Overview() {
		wg.Add(1)
		go func(q promql.OverviewQuery) {
			defer wg.Done()
			if h.Metrics == nil {
				markDegraded(q.Card)
				return
			}

			result, err := h.Metrics.Instant(ctx, q.Query)
			if err != nil {
				markDegraded(q.Card)
				return
			}

			switch q.Scope {
			case promql.OverviewService:
				if q.Name == "version" {
					v := serviceVersions(result)
					mu.Lock()
					versions = v
					mu.Unlock()
					return
				}
				v := serviceValues(result)
				mu.Lock()
				values[q.Name] = v
				mu.Unlock()
			case promql.OverviewHost:
				v, ok := result["{}"]
				if !ok {
					markDegraded(q.Card)
					return
				}
				mu.Lock()
				hostValues[q.Name] = finite(v)
				mu.Unlock()
			}
		}(q)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		if h.AllowList == nil {
			return
		}
		set, _ := h.AllowList(ctx)
		allow = set
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if h.Metrics == nil {
			markDegraded("alerts")
			return
		}
		got, err := h.Metrics.Alerts(ctx)
		if err != nil {
			markDegraded("alerts")
			return
		}
		mu.Lock()
		alerts = got
		mu.Unlock()
	}()

	wg.Wait()

	// Only firing alerts reach the page; pending ones are still inside their for: window.
	// make(..., 0) keeps the JSON array non-null when there is nothing to show.
	firing := make([]alertSummary, 0, len(alerts))
	for _, a := range alerts {
		if a.State != "firing" {
			continue
		}
		firing = append(firing, alertSummary{
			Name:     a.Name,
			Severity: a.Severity,
			Service:  a.Service,
			Summary:  a.Summary,
			ActiveAt: a.ActiveAt,
		})
	}
	sortAlerts(firing)

	statuses := []health.Status{}
	if h.Services != nil {
		statuses = h.Services()
	}

	names := make(map[string]struct{}, len(statuses)+len(allow))
	probed := make(map[string]health.Status, len(statuses))
	for _, s := range statuses {
		probed[s.Name] = s
		names[s.Name] = struct{}{}
	}
	for name := range allow {
		names[name] = struct{}{}
	}

	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)

	services := make([]serviceOverview, 0, len(sorted))
	for _, name := range sorted {
		status, known := probed[name]

		card := serviceOverview{
			Name:       name,
			Version:    versions[name],
			RPS:        floatPtr(values["rps"], name),
			ErrorRatio: floatPtr(values["error_ratio"], name),
			P95Ms:      floatPtr(values["p95_ms"], name),
		}

		card.Up = status.Up
		if upMap := values["up"]; upMap != nil {
			if v, ok := upMap[name]; ok {
				card.Up = v > 0.5
			}
		}

		if known {
			card.Ready = status.Ready
			card.Reason = status.Reason
		} else {
			card.Reason = "not probed"
		}

		services = append(services, card)
	}

	if degraded == nil {
		degraded = []string{}
	}
	sort.Strings(degraded)

	return json.Marshal(overviewResponse{
		GeneratedAt: now.UTC().Format(time.RFC3339),
		Services:    services,
		Host: hostUsage{
			CPURatio:  hostValues["cpu_ratio"],
			MemRatio:  hostValues["mem_ratio"],
			DiskRatio: hostValues["disk_ratio"],
		},
		OnlinePlayers: hostValues["online_players"],
		Alerts:        firing,
		Degraded:      degraded,
	})
}

// alertSeverityRank orders severities for display: critical first, then warning, then
// anything else. It is a presentation order, not a judgement about the label's value.
func alertSeverityRank(severity string) int {
	switch severity {
	case "critical":
		return 0
	case "warning":
		return 1
	default:
		return 2
	}
}

// sortAlerts orders alerts by severity rank, then name, then service, so two instances of
// the same rule are grouped and the list is stable across responses.
func sortAlerts(alerts []alertSummary) {
	sort.SliceStable(alerts, func(i, j int) bool {
		ri, rj := alertSeverityRank(alerts[i].Severity), alertSeverityRank(alerts[j].Severity)
		if ri != rj {
			return ri < rj
		}
		if alerts[i].Name != alerts[j].Name {
			return alerts[i].Name < alerts[j].Name
		}
		return alerts[i].Service < alerts[j].Service
	})
}

// serviceValues maps an instant vector's series keys to their service label. A series
// without a service label is not a service card and is skipped.
func serviceValues(result map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(result))
	for key, v := range result {
		if service := prometheus.LabelValue(key, "service"); service != "" {
			out[service] = v
		}
	}
	return out
}

// serviceVersions maps a build_info vector's series keys to the version label. The value is
// always 1; the label is the answer.
func serviceVersions(result map[string]float64) map[string]string {
	out := make(map[string]string, len(result))
	for key := range result {
		service := prometheus.LabelValue(key, "service")
		version := prometheus.LabelValue(key, "version")
		if service != "" && version != "" {
			out[service] = version
		}
	}
	return out
}

// floatPtr returns a pointer to the value for name, or nil when the query returned nothing
// for it. A nil field serialises as JSON null, which the WebUI renders as a dash.
func floatPtr(m map[string]float64, name string) *float64 {
	v, ok := m[name]
	if !ok {
		return nil
	}
	return finite(v)
}

// finite returns a pointer to v, or nil when v is NaN or ±Inf. Prometheus answers NaN
// for perfectly ordinary states: histogram_quantile over a service that served no
// requests in the window is the common one. JSON has no NaN, so json.Marshal would fail
// the whole overview; a null card ("no data") is what the value means. It is not
// degraded, because the query itself succeeded.
func finite(v float64) *float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return &v
}
