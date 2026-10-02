package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestLabelsArePreinitialised checks the promise that every label combination exists at
// 0 before the first event, so a dashboard shows a real zero rather than a gap.
func TestLabelsArePreinitialised(t *testing.T) {
	reg := prometheus.NewRegistry()
	New(reg)

	if err := testutil.GatherAndCompare(reg, strings.NewReader(`
# HELP gameplay_proxy_handshakes_total Handshakes answered by the proxy, by result.
# TYPE gameplay_proxy_handshakes_total counter
gameplay_proxy_handshakes_total{result="busy"} 0
gameplay_proxy_handshakes_total{result="expired"} 0
gameplay_proxy_handshakes_total{result="full"} 0
gameplay_proxy_handshakes_total{result="invalid"} 0
gameplay_proxy_handshakes_total{result="ok"} 0
gameplay_proxy_handshakes_total{result="reused"} 0
gameplay_proxy_handshakes_total{result="unknown_server"} 0
`), "gameplay_proxy_handshakes_total"); err != nil {
		t.Error(err)
	}

	if err := testutil.GatherAndCompare(reg, strings.NewReader(`
# HELP gameplay_proxy_packets_total UDP datagrams forwarded by the proxy, by direction.
# TYPE gameplay_proxy_packets_total counter
gameplay_proxy_packets_total{direction="to_client"} 0
gameplay_proxy_packets_total{direction="to_server"} 0
`), "gameplay_proxy_packets_total"); err != nil {
		t.Error(err)
	}

	if err := testutil.GatherAndCompare(reg, strings.NewReader(`
# HELP gameplay_proxy_bytes_total UDP payload bytes forwarded by the proxy, by direction.
# TYPE gameplay_proxy_bytes_total counter
gameplay_proxy_bytes_total{direction="to_client"} 0
gameplay_proxy_bytes_total{direction="to_server"} 0
`), "gameplay_proxy_bytes_total"); err != nil {
		t.Error(err)
	}

	if err := testutil.GatherAndCompare(reg, strings.NewReader(`
# HELP gameplay_proxy_directory_refresh_total Server-directory refreshes attempted, by result.
# TYPE gameplay_proxy_directory_refresh_total counter
gameplay_proxy_directory_refresh_total{result="error"} 0
gameplay_proxy_directory_refresh_total{result="ok"} 0
`), "gameplay_proxy_directory_refresh_total"); err != nil {
		t.Error(err)
	}

	if err := testutil.GatherAndCompare(reg, strings.NewReader(`
# HELP gameplay_proxy_sessions Client sessions currently tracked by the proxy.
# TYPE gameplay_proxy_sessions gauge
gameplay_proxy_sessions 0
`), "gameplay_proxy_sessions"); err != nil {
		t.Error(err)
	}

	if err := testutil.GatherAndCompare(reg, strings.NewReader(`
# HELP gameplay_proxy_directory_servers Game servers in the last good directory snapshot.
# TYPE gameplay_proxy_directory_servers gauge
gameplay_proxy_directory_servers 0
`), "gameplay_proxy_directory_servers"); err != nil {
		t.Error(err)
	}
}

// TestRecordsMovement checks the setters actually move the series they claim to.
func TestRecordsMovement(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg)

	m.RecordHandshake(ResultOK)
	m.SessionOpened()
	m.RecordPacket(DirectionToServer, 10)
	m.RecordPacket(DirectionToClient, 20)
	m.SetDirectoryServers(3)
	m.RecordDirectoryRefresh(RefreshError)

	if got := testutil.ToFloat64(m.HandshakeCounter(ResultOK)); got != 1 {
		t.Errorf("ok handshakes = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.SessionsGauge()); got != 1 {
		t.Errorf("sessions = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.DirectoryServersGauge()); got != 3 {
		t.Errorf("directory servers = %v, want 3", got)
	}
}
