package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/otomo-live/otomo/services/config/internal/api"
	"github.com/otomo-live/otomo/services/config/internal/blob"
	"github.com/otomo-live/otomo/services/config/internal/schema"
	"github.com/otomo-live/otomo/services/config/internal/store"
)

// unlabelledValue reads a counter with no labels out of the Prometheus text exposition,
// returning 0 when the series is absent.
func unlabelledValue(t *testing.T, h *harness, name string) float64 {
	t.Helper()

	body := h.do(t, h.internal, http.MethodGet, "/metrics", "").body
	prefix := name + " "
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatalf("cannot read a value out of %q", line)
		}
		var v float64
		if _, err := fmt.Sscanf(fields[1], "%g", &v); err != nil {
			t.Fatalf("cannot parse %q: %v", fields[1], err)
		}
		return v
	}
	return 0
}

// TestPublishChannelSeriesExistAtZero pins the pre-initialisation: a fresh server must
// expose all three channel series at 0 before any publish, so an alert can be expressed
// against them without waiting for the first release. It needs no database.
func TestPublishChannelSeriesExistAtZero(t *testing.T) {
	h := newHarness(t, nil)

	for _, ch := range []string{"dev", "staging", "live"} {
		labels := fmt.Sprintf(`channel=%q`, ch)
		if got := counterValue(t, h, "config_publish_total", labels); got != 0 {
			t.Errorf("config_publish_total{%s} = %g on a fresh server, want 0", labels, got)
		}
	}
	if got := unlabelledValue(t, h, "config_validation_failures_total"); got != 0 {
		t.Errorf("config_validation_failures_total = %g on a fresh server, want 0", got)
	}
}

// TestPublishMetricCountsOnlySuccessfulPublishes drives the real handler path: one
// successful publish moves the channel's counter by one, and a stale publish (409) must
// leave it where it was.
func TestPublishMetricCountsOnlySuccessfulPublishes(t *testing.T) {
	db := openTestDB(t)
	lockDevHead(t, db)
	original := devHead(t, db)
	t.Cleanup(func() { restoreDevHead(t, db, original) })

	blobs, err := blob.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	h := newHarness(t, nil, &api.Handlers{
		Store:   db,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Schemas: &schema.Cache{},
		Blobs:   blobs,
	})

	name := fmt.Sprintf("srv.metric_publish_%d", time.Now().UnixNano())
	_, version, pack := publishFixture(t, db, blobs, name)
	liveOps := h.token(t, []string{"live_ops"}, nil)
	body := fmt.Sprintf(
		`{"base_release_id":%d,"versions":[{"namespace":%q,"version":%d}],"packs":[{"sha256":%q}],"min_client_version":"1.4.0","message":"metrics"}`,
		original, name, version, pack.SHA256)

	if resp := h.doJSON(t, http.MethodPost, "/api/admin/config/channels/dev/releases", liveOps, body); resp.status != http.StatusCreated {
		t.Fatalf("publish = %d, want 201 (%s)", resp.status, resp.body)
	}
	if got := counterValue(t, h, "config_publish_total", `channel="dev"`); got != 1 {
		t.Errorf("config_publish_total{channel=dev} = %g after one publish, want 1", got)
	}

	// Replaying the same base is stale: 409, and not a publish.
	if resp := h.doJSON(t, http.MethodPost, "/api/admin/config/channels/dev/releases", liveOps, body); resp.status != http.StatusConflict {
		t.Fatalf("stale publish = %d, want 409 (%s)", resp.status, resp.body)
	}
	if got := counterValue(t, h, "config_publish_total", `channel="dev"`); got != 1 {
		t.Errorf("config_publish_total{channel=dev} = %g after a failed publish, want 1", got)
	}
}

// TestValidationFailureMetric covers both ways a document is found invalid and the
// malformed-body case that must not be counted as one.
func TestValidationFailureMetric(t *testing.T) {
	db := openTestDB(t)
	blobs, err := blob.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	h := newHarness(t, nil, &api.Handlers{
		Store:   db,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Schemas: &schema.Cache{},
		Blobs:   blobs,
	})

	ctx := context.Background()
	actor := store.Entry{ActorID: "staff-1", ActorName: "Validation Operator"}
	name := fmt.Sprintf("srv.metric_validate_%d", time.Now().UnixNano())
	if _, err := db.CreateNamespace(ctx, name, "client", "", actor); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	if _, err := db.ReplaceSchema(ctx, name,
		[]byte(`{"type":"object","required":["level"],"additionalProperties":false}`), nil, actor); err != nil {
		t.Fatalf("ReplaceSchema: %v", err)
	}
	// The draft is deliberately invalid; SaveDraft does not validate.
	if _, err := db.SaveDraft(ctx, name, []byte(`{}`), 1, actor); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	liveOps := h.token(t, []string{"live_ops"}, nil)
	base := unlabelledValue(t, h, "config_validation_failures_total")
	validatePath := "/api/admin/config/namespaces/" + name + "/draft/validate"

	// draft/validate answering valid:false counts once.
	resp := h.doJSON(t, http.MethodPost, validatePath, liveOps, `{"document":{}}`)
	if resp.status != http.StatusOK {
		t.Fatalf("validate = %d, want 200 (%s)", resp.status, resp.body)
	}
	if got := unlabelledValue(t, h, "config_validation_failures_total"); got != base+1 {
		t.Errorf("validation failures = %g after valid:false, want %g", got, base+1)
	}

	// A malformed body is a 400 before schema validation, so it does not count.
	beforeMalformed := unlabelledValue(t, h, "config_validation_failures_total")
	if resp := h.doJSON(t, http.MethodPost, validatePath, liveOps, `{`); resp.status != http.StatusBadRequest {
		t.Fatalf("malformed validate = %d, want 400 (%s)", resp.status, resp.body)
	}
	if got := unlabelledValue(t, h, "config_validation_failures_total"); got != beforeMalformed {
		t.Errorf("validation failures = %g after a malformed body, want %g", got, beforeMalformed)
	}

	// Cutting a version from the invalid draft is rejected with schema issues and counts.
	beforeVersion := unlabelledValue(t, h, "config_validation_failures_total")
	resp = h.doJSON(t, http.MethodPost,
		"/api/admin/config/namespaces/"+name+"/versions", liveOps, `{"message":"v1","revision":2}`)
	if resp.status != http.StatusBadRequest {
		t.Fatalf("create version = %d, want 400 (%s)", resp.status, resp.body)
	}
	if got := unlabelledValue(t, h, "config_validation_failures_total"); got != beforeVersion+1 {
		t.Errorf("validation failures = %g after a schema-issue version rejection, want %g", got, beforeVersion+1)
	}
}
