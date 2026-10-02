package proxy

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/otomo-live/otomo/services/gateway_dev/internal/apierr"
	"github.com/otomo-live/otomo/services/gateway_dev/internal/router"
)

// Registry holds one pre-built reverse proxy per upstream, reused across
// requests for connection-pool reuse (techspec §5.3).
type Registry struct {
	proxies map[string]*httputil.ReverseProxy
}

// NewRegistry creates a reverse proxy for each configured upstream URL.
func NewRegistry(upstreams map[string]*url.URL) *Registry {
	reg := &Registry{proxies: make(map[string]*httputil.ReverseProxy, len(upstreams))}
	for name, target := range upstreams {
		p := httputil.NewSingleHostReverseProxy(target)
		p.Transport = &http.Transport{
			MaxIdleConnsPerHost: 64,
			IdleConnTimeout:     90 * time.Second,
		}
		p.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
			// MaxBytesReader trips mid-copy for chunked bodies the ContentLength
			// pre-check could not see; surface it as a 413, not a bogus 502.
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				apierr.WriteError(w, r, http.StatusRequestEntityTooLarge, "body_too_large",
					fmt.Sprintf("request body exceeds %d bytes", mbe.Limit))
				return
			}
			apierr.WriteError(w, r, http.StatusBadGateway, "upstream_error", "upstream service unavailable")
		}
		reg.proxies[name] = p
	}
	return reg
}

// Has reports whether the registry contains a proxy for the given upstream key.
func (reg *Registry) Has(name string) bool {
	_, ok := reg.proxies[name]
	return ok
}

// CloseIdleConnections closes idle connections on all proxy transports.
func (reg *Registry) CloseIdleConnections() {
	for _, p := range reg.proxies {
		if t, ok := p.Transport.(*http.Transport); ok {
			t.CloseIdleConnections()
		}
	}
}

// SetTransport replaces the transport on the named upstream's proxy.
func (reg *Registry) SetTransport(name string, rt http.RoundTripper) {
	if p, ok := reg.proxies[name]; ok {
		p.Transport = rt
	}
}

// HandlerFor returns an http.HandlerFunc that proxies requests for the given
// route through its upstream. Per-route behaviour (prefix stripping, stream
// handling) wraps the shared proxy rather than mutating it.
func (reg *Registry) HandlerFor(route router.Route) http.HandlerFunc {
	p := reg.proxies[route.Upstream]
	return func(w http.ResponseWriter, r *http.Request) {
		if route.StripPrefix != "" {
			r.URL.Path = strings.TrimPrefix(r.URL.Path, route.StripPrefix)
		}
		r.Header.Set("X-Request-Id", apierr.RequestIDFrom(r.Context()))
		if !route.ForwardCookies {
			// Cookies reach an upstream only on routes that declare they need them
			//. Every otomo host sits under a parent domain shared with
			// other teams, so browsers attach foreign parent-domain cookies to our
			// requests, and admin-auth's __Host- refresh cookie is Path=/, so it
			// rides on every staff request. Neither may reach an upstream, or its
			// logs, that has no use for it.
			r.Header.Del("Cookie")
		}
		if route.SetForwarded {
			// Drop every client-supplied claim about its own address so the
			// Director replaces X-Forwarded-For with exactly the peer IP, and
			// record the original scheme. Forwarded (RFC 7239) and X-Real-Ip go
			// too: an upstream reading either would otherwise trust the client.
			r.Header.Del("X-Forwarded-For")
			r.Header.Del("Forwarded")
			r.Header.Del("X-Real-Ip")
			if r.TLS != nil {
				r.Header.Set("X-Forwarded-Proto", "https")
			} else {
				r.Header.Set("X-Forwarded-Proto", "http")
			}
		}
		if route.Upload {
			// A large upload outlives both server-wide timeouts: net/http runs
			// WriteTimeout from the end of the request headers, so without this
			// a long read would be killed before the upstream could answer.
			rc := http.NewResponseController(w)
			_ = rc.SetReadDeadline(time.Time{})
			_ = rc.SetWriteDeadline(time.Time{})
		}
		if r.Body != nil && r.Body != http.NoBody {
			limit := route.MaxBody
			if limit == 0 {
				limit = router.DefaultMaxBody
			}
			if r.ContentLength > limit {
				apierr.WriteError(w, r, http.StatusRequestEntityTooLarge, "body_too_large",
					fmt.Sprintf("request body exceeds %d bytes", limit))
				return
			}
			// MaxBytesReader also covers chunked bodies, where ContentLength is -1.
			r.Body = http.MaxBytesReader(w, r.Body, limit)
		}
		if route.Stream {
			rc := http.NewResponseController(w)
			_ = rc.SetWriteDeadline(time.Time{}) // clear server-wide write timeout for this response
			streamProxy := *p                    // shallow copy: same Transport, own FlushInterval
			streamProxy.FlushInterval = -1       // flush immediately for SSE/long-poll
			streamProxy.ServeHTTP(w, r)
			return
		}
		p.ServeHTTP(w, r)
	}
}
