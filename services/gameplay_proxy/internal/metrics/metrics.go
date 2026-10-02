// Package metrics owns the Gameplay Proxy's Prometheus instruments: handshake outcomes,
// the live session count, forwarded traffic, and the state of the server directory.
//
// Every method is safe on a nil receiver and records nothing, so a caller without a
// registry — most unit tests — passes nil rather than building a throwaway one. The
// registry itself belongs to internal/server; New registers on it through
// Server.Registry, so the Go and process collectors and these domain instruments share
// one /metrics.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

// The result label values for gameplay_proxy_handshakes_total. One of these is
// recorded for every well-formed handshake: the answered ones, including full, and busy,
// a handshake dropped unanswered because every handshake slot was taken.
const (
	ResultOK            = "ok"
	ResultInvalid       = "invalid"
	ResultExpired       = "expired"
	ResultReused        = "reused"
	ResultUnknownServer = "unknown_server"
	ResultFull          = "full"
	ResultBusy          = "busy"
)

// The direction label values for the packet and byte counters. to_server is traffic
// the proxy forwarded to a game server, to_client is traffic it forwarded back.
const (
	DirectionToServer = "to_server"
	DirectionToClient = "to_client"
)

// The result label values for gameplay_proxy_directory_refresh_total.
const (
	RefreshOK    = "ok"
	RefreshError = "error"
)

// Metrics holds the proxy's collectors. Build it once with New; the collectors are safe
// for concurrent use, and a nil *Metrics is a no-op.
type Metrics struct {
	handshakes       *prometheus.CounterVec
	sessions         prometheus.Gauge
	packets          *prometheus.CounterVec
	bytes            *prometheus.CounterVec
	directoryServers prometheus.Gauge
	directoryRefresh *prometheus.CounterVec
}

// New builds the collectors, registers them on reg (skipping registration when reg is
// nil), and pre-initialises every label combination to 0.
//
// Pre-initialising means a dashboard shows a real zero before the first event rather
// than a missing series, and a rate() over a counter that has just come up does not
// start at an absent series.
func New(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		handshakes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gameplay_proxy_handshakes_total",
			Help: "Handshakes answered by the proxy, by result.",
		}, []string{"result"}),
		sessions: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gameplay_proxy_sessions",
			Help: "Client sessions currently tracked by the proxy.",
		}),
		packets: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gameplay_proxy_packets_total",
			Help: "UDP datagrams forwarded by the proxy, by direction.",
		}, []string{"direction"}),
		bytes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gameplay_proxy_bytes_total",
			Help: "UDP payload bytes forwarded by the proxy, by direction.",
		}, []string{"direction"}),
		directoryServers: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gameplay_proxy_directory_servers",
			Help: "Game servers in the last good directory snapshot.",
		}),
		directoryRefresh: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gameplay_proxy_directory_refresh_total",
			Help: "Server-directory refreshes attempted, by result.",
		}, []string{"result"}),
	}

	if reg != nil {
		reg.MustRegister(
			m.handshakes,
			m.sessions,
			m.packets,
			m.bytes,
			m.directoryServers,
			m.directoryRefresh,
		)
	}
	m.initLabels()
	return m
}

// initLabels touches every series once so it exists at 0 before the first event.
func (m *Metrics) initLabels() {
	for _, result := range []string{
		ResultOK,
		ResultInvalid,
		ResultExpired,
		ResultReused,
		ResultUnknownServer,
		ResultFull,
		ResultBusy,
	} {
		m.handshakes.WithLabelValues(result).Add(0)
	}
	for _, direction := range []string{DirectionToServer, DirectionToClient} {
		m.packets.WithLabelValues(direction).Add(0)
		m.bytes.WithLabelValues(direction).Add(0)
	}
	for _, result := range []string{RefreshOK, RefreshError} {
		m.directoryRefresh.WithLabelValues(result).Add(0)
	}
	m.sessions.Set(0)
	m.directoryServers.Set(0)
}

// RecordHandshake counts one answered handshake under result. A handshake that is
// dropped without an answer — a malformed datagram or an unknown source — is not
// counted, because the client never learns it happened.
func (m *Metrics) RecordHandshake(result string) {
	if m == nil {
		return
	}
	m.handshakes.WithLabelValues(result).Inc()
}

// SessionOpened and SessionClosed keep gameplay_proxy_sessions in step with the live
// session table. The gauge is adjusted on both ends so a reaped idle session and a
// handshake-refused session cannot leave it permanently high.
func (m *Metrics) SessionOpened() {
	if m == nil {
		return
	}
	m.sessions.Inc()
}

// SessionClosed decrements the live-session gauge.
func (m *Metrics) SessionClosed() {
	if m == nil {
		return
	}
	m.sessions.Dec()
}

// RecordPacket counts one forwarded datagram of n payload bytes under direction.
func (m *Metrics) RecordPacket(direction string, n int) {
	if m == nil {
		return
	}
	m.packets.WithLabelValues(direction).Inc()
	m.bytes.WithLabelValues(direction).Add(float64(n))
}

// SetDirectoryServers records the size of the last good directory snapshot. It is
// called only after a successful refresh, so a failed poll keeps the previous gauge
// rather than reporting zero and looking like an empty pool.
func (m *Metrics) SetDirectoryServers(n int) {
	if m == nil {
		return
	}
	m.directoryServers.Set(float64(n))
}

// RecordDirectoryRefresh counts one poll under result. Both a non-2xx answer and a
// transport error are RefreshError; the difference is in the log, not the metric.
func (m *Metrics) RecordDirectoryRefresh(result string) {
	if m == nil {
		return
	}
	m.directoryRefresh.WithLabelValues(result).Inc()
}

// HandshakeCounter returns the single counter for one result, so a test can assert one
// series with testutil.ToFloat64 instead of collecting the whole vector.
func (m *Metrics) HandshakeCounter(result string) prometheus.Counter {
	return m.handshakes.WithLabelValues(result)
}

// SessionsGauge returns the live-session gauge for the same reason.
func (m *Metrics) SessionsGauge() prometheus.Gauge {
	return m.sessions
}

// DirectoryServersGauge returns the directory-size gauge for the same reason.
func (m *Metrics) DirectoryServersGauge() prometheus.Gauge {
	return m.directoryServers
}
