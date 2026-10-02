package main

import (
	"net/url"
	"strings"
	"testing"

	"gateway/gateway/internal/proxy"
	"gateway/gateway/internal/router"
)

func TestVerifyUpstreams_MissingUpstreamFails(t *testing.T) {
	routes := []router.Route{
		{Method: "GET", Pattern: "/api/test", Upstream: "missing-svc"},
	}
	reg := proxy.NewRegistry(map[string]*url.URL{})

	err := verifyUpstreams(routes, reg)
	if err == nil {
		t.Fatal("expected error for missing upstream, got nil")
	}
	if !strings.Contains(err.Error(), "missing-svc") {
		t.Errorf("error %q does not mention the missing upstream key", err)
	}
}

func TestVerifyUpstreams_AllPresentSucceeds(t *testing.T) {
	u, _ := url.Parse("http://localhost:8080")
	routes := []router.Route{
		{Method: "GET", Pattern: "/api/a", Upstream: "svc-a"},
		{Method: "POST", Pattern: "/api/b", Upstream: "svc-b"},
	}
	reg := proxy.NewRegistry(map[string]*url.URL{
		"svc-a": u,
		"svc-b": u,
	})

	if err := verifyUpstreams(routes, reg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
