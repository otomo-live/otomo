// Command gameplay_proxy is the Otomo Gameplay Proxy.
//
// Players reach game servers only through this service. It listens on public UDP 27000,
// verifies the join ticket carried by a client's handshake datagram, and forwards that
// client's traffic to the private game server named by the ticket's srv claim. It is
// the only component besides the game server itself that sees a ticket, and it is the
// one that enforces single use. The protocol is fixed by
// design/14-launch-handoff.md §8; the ticket format is §5.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/otomo-live/otomo/services/gameplay_proxy/internal/config"
	"github.com/otomo-live/otomo/services/gameplay_proxy/internal/directory"
	"github.com/otomo-live/otomo/services/gameplay_proxy/internal/metrics"
	"github.com/otomo-live/otomo/services/gameplay_proxy/internal/proxy"
	"github.com/otomo-live/otomo/services/gameplay_proxy/internal/server"
	"github.com/otomo-live/otomo/services/gameplay_proxy/internal/servicekey"
)

// version is the build identifier reported in the startup log line and as
// gameplay_proxy_build_info. The Docker build stamps it with -X main.version=${GIT_SHA};
// a plain `go build` leaves the "dev" default.
var version = "dev"

const (
	// httpTimeout bounds every Allocator HTTP call. The directory poller retries on
	// its own timer, and a handshake that needs an immediate refresh would rather fail
	// fast than hold the client's retry window open.
	httpTimeout = 5 * time.Second

	// jwksRefreshInterval is the shortest gap between two JWKS fetches triggered by an
	// unknown kid, fixed by doc 14 §5. A ticket lives 60 s, so a key rotation is
	// picked up well inside one ticket lifetime even with this floor.
	jwksRefreshInterval = 10 * time.Second
)

func main() {
	os.Exit(run())
}

// run wires the service and blocks until a signal or a fatal listener error.
//
// Start-up is fail-fast on the one secret the proxy owns: the Allocator key file must
// be readable and well formed before the UDP socket opens. The JWKS and the server
// directory are loaded in the background and reported through /readyz instead, because
// the Allocator may still be starting and a proxy that refuses to boot cannot become
// ready later.
func run() int {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	log := newLogger(cfg.LogLevel)
	log.Info("gameplay proxy starting",
		slog.String("version", version),
		slog.String("listen_addr", cfg.ListenAddr),
		slog.String("metrics_addr", cfg.MetricsAddr),
		slog.String("allocator_url", cfg.AllocatorURL),
	)

	// The key is the one thing the proxy presents rather than verifies. Loading it here
	// turns a missing or malformed secret into a start-up failure instead of a
	// directory that silently never refreshes.
	key, err := servicekey.ReadKeyFile(cfg.AllocatorKeyPath)
	if err != nil {
		log.Error("cannot load the Allocator proxy key; run deploy/scripts/generate-secrets.sh to create it",
			slog.Any("error", err))
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// auth and dir are filled in after the server is constructed, because the server
	// owns the metrics registry they report on. The readiness closure reads them only
	// when a /readyz request arrives, by which time the assignment below has happened.
	var (
		auth *proxy.Authenticator
		dir  *directory.Directory
	)

	srv := server.New(cfg, server.Deps{
		Version: version,
		Logger:  log,
		Ready: func(context.Context) error {
			if auth == nil || !auth.Loaded() {
				return errors.New("ticket JWKS not loaded yet")
			}
			if dir == nil || !dir.Loaded() {
				return errors.New("server directory not loaded yet")
			}
			return nil
		},
	})
	m := metrics.New(srv.Registry())

	client := &http.Client{Timeout: httpTimeout}
	base := strings.TrimSuffix(cfg.AllocatorURL, "/")

	dir = directory.New(directory.Config{
		URL:     base + "/internal/servers",
		Key:     key,
		Refresh: cfg.DirectoryRefresh,
		Client:  client,
		Metrics: m,
		Logger:  log,
	})
	auth = proxy.NewAuthenticator(proxy.AuthenticatorConfig{
		JWKSURL:         base + "/.well-known/jwks.json",
		Issuer:          cfg.Issuer,
		Audience:        cfg.Audience,
		Leeway:          proxy.TicketLeeway,
		RefreshInterval: jwksRefreshInterval,
		Client:          client,
		Logger:          log,
	})
	p := proxy.New(proxy.Config{
		ListenAddr:  cfg.ListenAddr,
		IdleTimeout: cfg.IdleTimeout,
		MaxSessions: cfg.MaxSessions,
	}, dir, auth, m, log)

	// Both loaders run in the background so the listener is up immediately; /readyz
	// stays 503 until each has succeeded at least once.
	go dir.Run(runCtx)
	go auth.Run(runCtx, cfg.DirectoryRefresh)

	// The gate flag only guards the instant before the closures above are usable; the
	// real readiness answer comes from the loaded state inside the closure.
	srv.SetReady(true)

	errCh := make(chan error, 2)
	go func() { errCh <- srv.Run(runCtx) }()
	go func() { errCh <- p.Run(runCtx) }()

	// If either listener dies, cancel the other so the process exits as one. On a
	// signal both return nil and the join is nil.
	first := <-errCh
	cancel()
	second := <-errCh
	if err := errors.Join(first, second); err != nil {
		log.Error("service stopped with an error", slog.Any("error", err))
		return 1
	}
	log.Info("shutdown complete")
	return 0
}

// newLogger returns the process-wide JSON logger: one JSON object per line on stdout at
// the configured level. Logs go to stdout rather than stderr so the container log
// stream is a single ordered sequence. Ticket and key material are never passed to it.
func newLogger(level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}
