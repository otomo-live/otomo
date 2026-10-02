// Liveness and readiness handlers. Both are mounted on the internal listener only;
// Gateway never routes to them, and neither requires a token — which is exactly why
// they must not be reachable from outside the Docker network.
package api

import (
	"context"
	"net/http"
	"time"
)

// ReadyCheckTimeout bounds one run of the readiness check. The check runs on a context of
// its own rather than the request's (see Readyz), so this deadline is the only thing that
// stops a hung dependency from holding a probe open. It is well under the 5 s that
// deploy/scripts/lib.sh gives nc, so a slow dependency is reported as a 503 with a reason
// rather than as no response at all.
const ReadyCheckTimeout = 2 * time.Second

// Healthz reports that the process is alive, and nothing more. It deliberately does
// not touch Postgres or the blob volume: a liveness probe that fails on a dependency
// blip would get the container killed and restarted, which cannot fix a database that
// is down. Whether the service can actually serve is Readyz's question.
func Healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// Readyz wraps a readiness check into a handler: 200 while the check passes, and 503
// in the COM-5 shape carrying the check's message when it fails. The check is called
// once per request, so it must be cheap — the service composes it from store.DB.Ready
// (which caches its ping), a stat of the blob root and the verifier's key count, for
// exactly that reason.
//
// A nil check means "nothing to verify", not a programming error to panic on: a
// readiness endpoint that crashes is worse than useless, because it is the one thing
// that is supposed to report a service as unready.
//
// The check does not run on the request context. The deploy probe sends its
// request through nc, which half-closes the connection once the request is written, and
// Go's server takes that EOF as the client leaving and cancels the request context. A
// check that pinged Postgres or Valkey with it failed with "context canceled" while both
// were healthy. The answer must describe the dependencies, not the probe's socket, so the
// check gets a detached context with its own deadline instead.
func Readyz(check func(ctx context.Context) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if check != nil {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), ReadyCheckTimeout)
			defer cancel()
			if err := check(ctx); err != nil {
				WriteError(w, r, http.StatusServiceUnavailable, "not_ready", err.Error())
				return
			}
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}
}
