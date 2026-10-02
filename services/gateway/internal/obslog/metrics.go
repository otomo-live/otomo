package obslog

import "github.com/prometheus/client_golang/prometheus"

// Metrics collectors following COM-10's <service>_<thing>_<unit> naming.

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
