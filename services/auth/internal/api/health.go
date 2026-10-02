// Liveness and readiness handlers. Both are mounted on the internal listener only;
// Gateway never routes to them.

package api

import (
	"context"
	"net/http"
)

// Healthz reports that the process is alive, and nothing more. It deliberately does
// not touch Postgres: a liveness probe that fails on a dependency blip would get the
// container killed and restarted, which cannot fix a database that is down. Whether
// the service can actually serve is Readyz's question.
func Healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// Readyz wraps a readiness check into a handler: 200 while the check passes, and 503
// in the COM-5 shape carrying the check's message when it fails. The check is called
// once per request, so it must be cheap — store.DB.Ready caches its probe for exactly
// that reason. A nil-safe check is the caller's concern; the server package always
// supplies one.
func Readyz(check func(ctx context.Context) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := check(r.Context()); err != nil {
			WriteError(w, r, http.StatusServiceUnavailable, "not_ready", err.Error())
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}
}
