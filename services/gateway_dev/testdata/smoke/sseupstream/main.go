// sseupstream/main.go: the dashboard upstream, with one route that deliberately
// outlives the gateway's write timeout.
//
// GET /api/admin/dashboard/logs/tail is a Stream route. internal/proxy clears
// the server-wide WriteTimeout for it with
// http.NewResponseController(w).SetWriteDeadline(time.Time{}) and flushes every
// write (FlushInterval = -1). This server is what proves that: it emits one SSE
// chunk, waits longer than the configured GATEWAY_DEV_WRITE_TIMEOUT, then emits
// a second. If the deadline were not cleared, the connection would die during
// the wait and the second chunk would never arrive.
//
// Every other path answers with the same echo JSON testdata/manual/fake_upstream.go
// serves, so this can stand in for the whole dashboard upstream rather than just
// the streaming route.
//
// Usage (normally from smoke.sh, which builds and starts it):
//
//	go run ./testdata/smoke/sseupstream -addr 127.0.0.1:18093 -hold 3s
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"time"
)

// ssePath is the streaming route in services/gateway_dev's table. It is matched
// exactly, so the echo handler below must not shadow it.
const ssePath = "/api/admin/dashboard/logs/tail"

func main() {
	addr := flag.String("addr", "127.0.0.1:18093", "address to listen on")
	hold := flag.Duration("hold", 3*time.Second, "how long to wait between the two SSE chunks")
	flag.Parse()

	mux := http.NewServeMux()

	mux.HandleFunc(ssePath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "response writer cannot flush", http.StatusInternalServerError)
			return
		}

		log.Printf("%s %s: chunk-1", r.Method, r.URL.Path)
		fmt.Fprint(w, "data: chunk-1\n\n")
		flusher.Flush()

		// The whole point: a gap longer than GATEWAY_DEV_WRITE_TIMEOUT.
		select {
		case <-time.After(*hold):
		case <-r.Context().Done():
			log.Printf("%s %s: client went away during the hold", r.Method, r.URL.Path)
			return
		}

		log.Printf("%s %s: chunk-2, after holding %s", r.Method, r.URL.Path, *hold)
		fmt.Fprint(w, "data: chunk-2\n\n")
		flusher.Flush()
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		out, _ := json.MarshalIndent(map[string]any{
			"upstream":   "dashboard",
			"method":     r.Method,
			"path":       r.URL.Path,
			"request_id": r.Header.Get("X-Request-Id"),
			"timestamp":  time.Now().Format(time.RFC3339),
		}, "", "  ")
		_, _ = w.Write(out)
		_, _ = w.Write([]byte("\n"))
	})

	log.Printf("sse upstream listening on %s (%s holds %s between chunks)", *addr, ssePath, *hold)

	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("sse upstream: %v", err)
	}
}
