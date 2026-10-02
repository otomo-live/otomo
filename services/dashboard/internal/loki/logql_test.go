package loki

import (
	"errors"
	"strings"
	"testing"

	"github.com/otomo-live/otomo/services/dashboard/internal/promql"
	"github.com/otomo-live/otomo/services/dashboard/internal/source"
)

func TestBuildGolden(t *testing.T) {
	services := promql.NewServiceSet("gateway", "config")

	tests := []struct {
		name string
		q    source.LogQuery
		want string
	}{
		{
			name: "service only",
			q:    source.LogQuery{Service: "gateway"},
			want: `{service="gateway"}`,
		},
		{
			name: "service and level",
			q:    source.LogQuery{Service: "gateway", Level: "error"},
			want: `{service="gateway",level="error"}`,
		},
		{
			name: "contains only",
			q:    source.LogQuery{Service: "config", Contains: "publish"},
			want: `{service="config"} |= "publish"`,
		},
		{
			name: "all filters",
			q:    source.LogQuery{Service: "gateway", Level: "warn", Contains: "timeout", RequestID: "req-1"},
			want: `{service="gateway",level="warn"} |= "timeout" |= "request_id\":\"req-1\""`,
		},
		{
			name: "no service selects every stream",
			q:    source.LogQuery{},
			want: `{service=~".+"}`,
		},
		{
			name: "no service with level",
			q:    source.LogQuery{Level: "info"},
			want: `{service=~".+",level="info"}`,
		},
		{
			// A quote and a brace in a filter value are data: they are escaped inside
			// the Go/LogQL string literal, so the filter can never break out into
			// selector syntax.
			name: "injection is literal",
			q:    source.LogQuery{Service: "gateway", Contains: `"} |= "x" or {job=~".+`},
			want: `{service="gateway"} |= "\"} |= \"x\" or {job=~\".+"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Build(tt.q, services)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if got != tt.want {
				t.Errorf("Build = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildValidation(t *testing.T) {
	services := promql.NewServiceSet("gateway")

	tests := []struct {
		name string
		q    source.LogQuery
		want error
	}{
		{"unknown service", source.LogQuery{Service: "nope"}, ErrUnknownService},
		{"malformed service", source.LogQuery{Service: `gateway"}`}, ErrUnknownService},
		{"uppercase service", source.LogQuery{Service: "Gateway"}, ErrUnknownService},
		{"unknown level", source.LogQuery{Service: "gateway", Level: "fatal"}, ErrUnknownLevel},
		{"newline in contains", source.LogQuery{Service: "gateway", Contains: "a\nb"}, ErrInvalidContains},
		{"carriage return in contains", source.LogQuery{Service: "gateway", Contains: "a\rb"}, ErrInvalidContains},
		{"bad request id", source.LogQuery{Service: "gateway", RequestID: "has space"}, ErrInvalidRequestID},
		{"empty request id is no filter", source.LogQuery{Service: "gateway", RequestID: ""}, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Build(tt.q, services)
			if tt.want == nil {
				if err != nil {
					t.Fatalf("Build: %v", err)
				}
				return
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("Build error = %v, want %v", err, tt.want)
			}
			if !strings.Contains(err.Error(), "loki:") {
				t.Errorf("error %q is not namespaced", err)
			}
		})
	}
}
