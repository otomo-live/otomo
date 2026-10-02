package obslog

import "github.com/prometheus/client_golang/prometheus"

// Metrics collectors following COM-10's <service>_<thing>_<unit> naming.
//
// The names are deliberately the same gateway_* series as services/gateway, not
// gateway_dev_*. COM-10's prefix rule exists so two services emitting one name
// cannot be confused, and here they cannot: every label value is disjoint, since
// `group` is staff on every route this table protects and the `route` label holds
// ServeMux patterns that appear in one table only. Naming them separately would
// instead split a single question ("which edge is rejecting tokens, and why")
// across two queries, and design/01-dashboard.md DSH-B5 and techspec §8 already
// name gateway_token_rejected_total{reason,group}. Do not "fix" this without
// changing those two in the same commit.

var (
	// HTTPRequestDuration records per-request latency on the public listener.
	// Labels follow COM-10's allowed set: route, method, status.
	HTTPRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gateway_http_request_duration_seconds",
		Help:    "Latency of HTTP requests on the public listener.",
		Buckets: prometheus.DefBuckets,
	}, []string{"route", "method", "status"})

	// TokenRejected counts authentication rejections, declared now for GATE-3.
	//
	// TODO(COM-10 conflict): COM-10 allows labels route, method, status, channel.
	// This metric uses reason and group, which are outside that set. The gateway
	// techspec §8 requires these labels for debuggability. Reconcile with COM-10
	// before the Dashboard metrics spec is finalised.
	TokenRejected = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_token_rejected_total",
		Help: "Tokens rejected by the authentication middleware.",
	}, []string{"reason", "group"})
)

func init() {
	prometheus.MustRegister(HTTPRequestDuration)
	prometheus.MustRegister(TokenRejected)
}
