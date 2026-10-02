// Liveness and readiness handlers. Both are mounted on the internal listener only;
// nothing routes to them from Gateway, and neither requires a token — which is exactly
// why they must not be reachable from outside the Docker network.
package api

import (
	"context"
	"net/http"
)

// Healthz reports that the process is alive, and nothing more. It deliberately does
// not touch Postgres or any key file: a liveness probe that fails on a dependency
// blip would get the container killed and restarted, which cannot fix a database that
// is down. Whether the service can actually serve is Readyz's question.
func Healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// Readyz wraps a readiness check into a handler: a plain 200 while the check passes,
// and 503 in the COM-5 shape carrying the check's message when it fails. The check is
// called once per request, so it must be cheap — the service composes it from
// store.DB.Ready (which caches its ping) for exactly that reason.
//
// A nil check means "nothing to verify", not a programming error to panic on: a
// readiness endpoint that crashes is worse than useless, because it is the one thing
// that is supposed to report a service as unready.
func Readyz(check func(ctx context.Context) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if check != nil {
			if err := check(r.Context()); err != nil {
				WriteError(w, r, http.StatusServiceUnavailable, "not_ready", err.Error())
				return
			}
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}
}
