// fake_upstream.go — stub server that echoes request details as JSON.
// Starts one listener per upstream service for manual gateway testing.
//
// Usage:
//
//	go run fake_upstream.go
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

func main() {
	upstreams := map[string]int{
		"auth":      9001,
		"patch":     9002,
		"session":   9003,
		"adminauth": 9004,
		"adminui":   9005,
		"config":    9006,
		"dashboard": 9007,
	}

	var wg sync.WaitGroup
	for name, port := range upstreams {
		wg.Add(1)
		go func(name string, port int) {
			defer wg.Done()
			mux := http.NewServeMux()
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				out, _ := json.MarshalIndent(map[string]any{
					"upstream":          name,
					"method":            r.Method,
					"path":              r.URL.Path,
					"request_id":        r.Header.Get("X-Request-Id"),
					"x_forwarded_for":   r.Header.Get("X-Forwarded-For"),
					"x_forwarded_proto": r.Header.Get("X-Forwarded-Proto"),
					"timestamp":         time.Now().Format(time.RFC3339),
				}, "", "  ")
				w.Write(out)
				w.Write([]byte("\n"))
			})
			addr := fmt.Sprintf("127.0.0.1:%d", port)
			log.Printf("[%s] listening on %s", name, addr)
			if err := http.ListenAndServe(addr, mux); err != nil {
				log.Fatalf("[%s] %v", name, err)
			}
		}(name, port)
	}
	wg.Wait()
}
