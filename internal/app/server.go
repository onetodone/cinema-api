package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/onetodone/cinema-api/internal/config"
	"github.com/onetodone/cinema-api/internal/platform/metrics"
)

// server is an HTTP server of a process, with the name its log lines use, such as "http server".
type server struct {
	name string
	srv  *http.Server
	ln   net.Listener // opened by listen
}

// newServer returns a server for handler on addr with the timeouts of the API.
func newServer(name, addr string, handler http.Handler, cfg config.HTTPConfig, logger *slog.Logger) *server {
	return &server{name: name, srv: &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}}
}

// metricsMux serves the metrics handler at GET /metrics, and extra routes, such as health probes, next to it.
func metricsMux(handler http.Handler, extra map[string]http.HandlerFunc) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", handler)
	for pattern, h := range extra {
		mux.HandleFunc(pattern, h)
	}
	return mux
}

// listen opens the listeners of all servers before any of them serves, so that an address in use fails startup
// instead of leaving a process that is half up.
func listen(ctx context.Context, servers ...*server) error {
	var lc net.ListenConfig
	for i, s := range servers {
		ln, err := lc.Listen(ctx, "tcp", s.srv.Addr)
		if err != nil {
			for _, opened := range servers[:i] {
				_ = opened.ln.Close()
			}
			return fmt.Errorf("%s: listen on %s: %w", s.name, s.srv.Addr, err)
		}
		s.ln = ln
	}
	return nil
}

// serve serves s on its listener until ctx is canceled, then stops accepting connections and waits for requests
// in flight for at most shutdownTimeout. It returns an error if the server fails before that.
func (s *server) serve(ctx context.Context, logger *slog.Logger, shutdownTimeout time.Duration) error {
	serveErr := make(chan error, 1)
	go func() { serveErr <- s.srv.Serve(s.ln) }()
	logger.InfoContext(ctx, s.name+" started", slog.String("addr", s.ln.Addr().String()))

	select {
	case err := <-serveErr:
		return fmt.Errorf("%s: %w", s.name, err)
	case <-ctx.Done():
	}

	logger.Info(s.name+" shutting down; draining connections", slog.String("timeout", shutdownTimeout.String()))
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := s.srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("%s: graceful shutdown: %w", s.name, err)
	}
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("%s: %w", s.name, err)
	}
	logger.Info(s.name + " stopped")
	return nil
}

// newMetrics returns the metrics of a process and the handler that serves them.
func newMetrics() (*metrics.Metrics, http.Handler) {
	registry := metrics.NewRegistry()
	return metrics.New(registry), metrics.Handler(registry)
}
