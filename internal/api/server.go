package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"

	"github.com/briankim06/adaptive-batching-engine/internal/api/handlers"
	"github.com/briankim06/adaptive-batching-engine/internal/api/middleware"
	"github.com/briankim06/adaptive-batching-engine/internal/batcher"
	"github.com/briankim06/adaptive-batching-engine/internal/config"
	"github.com/briankim06/adaptive-batching-engine/internal/metrics"
	"github.com/briankim06/adaptive-batching-engine/internal/worker"
)

const shutdownGrace = 10 * time.Second

// Server owns the chi router and the underlying http.Server. It holds
// references to every component the handlers need; nothing is constructed
// here — main.go wires components and passes them in.
type Server struct {
	router    chi.Router
	batcher   batcher.Batcher
	pool      *worker.WorkerPool
	collector *metrics.Collector
	agg       *metrics.TimeSeriesAggregator
	publisher *handlers.SnapshotPublisher
	cfg       *config.Config
	logger    zerolog.Logger
	httpSrv   *http.Server
}

// NewServer constructs the chi router, mounts middleware, and registers
// all routes documented in docs/spec/05-api.md. The publisher is owned
// by main.go; the server just forwards it to HandleWebSocket.
func NewServer(
	cfg *config.Config,
	b batcher.Batcher,
	pool *worker.WorkerPool,
	collector *metrics.Collector,
	agg *metrics.TimeSeriesAggregator,
	publisher *handlers.SnapshotPublisher,
	logger zerolog.Logger,
) *Server {
	s := &Server{
		router:    chi.NewRouter(),
		batcher:   b,
		pool:      pool,
		collector: collector,
		agg:       agg,
		publisher: publisher,
		cfg:       cfg,
		logger:    logger,
	}
	s.mountMiddleware()
	s.mountRoutes()
	return s
}

func (s *Server) mountMiddleware() {
	s.router.Use(middleware.RequestID)
	s.router.Use(middleware.Recovery(s.logger))
	s.router.Use(middleware.Logging(s.logger))
	s.router.Use(middleware.MetricsMiddleware(s.collector))
}

func (s *Server) mountRoutes() {
	writeTimeout := s.cfg.Server.WriteTimeout
	if writeTimeout <= 0 {
		writeTimeout = 30 * time.Second
	}

	s.router.Post("/v1/completions", handlers.HandleCompletions(s.batcher, s.pool, writeTimeout))
	s.router.Post("/v1/embeddings", handlers.HandleEmbeddings(s.batcher, s.pool, writeTimeout))

	s.router.Method(http.MethodGet, "/metrics", handlers.HandlePrometheusMetrics())
	s.router.Get("/metrics/json", handlers.HandleJSONMetrics(s.collector))
	s.router.Get("/metrics/history", handlers.HandleMetricsHistory(s.agg))

	s.router.Get("/admin/config", handlers.HandleGetConfig(s.cfg))
	s.router.Post("/admin/strategy", handlers.HandleSetStrategy(s.batcher, &s.cfg.Batching))
	s.router.Get("/admin/workers", handlers.HandleGetWorkers(s.pool))

	s.router.Get("/health", handlers.HandleHealth())
	s.router.Get("/ws", handlers.HandleWebSocket(s.publisher, s.cfg.Dashboard.WSPushInterval))
}

// Router exposes the underlying router so tests can hit handlers directly
// via httptest without going through net.Listen.
func (s *Server) Router() chi.Router { return s.router }

// Start brings up the http.Server on cfg.Server.Host:Port and blocks until
// ctx is cancelled. A 10s grace period is given to Shutdown.
func (s *Server) Start(ctx context.Context) error {
	addr := fmt.Sprintf("%s:%d", s.cfg.Server.Host, s.cfg.Server.Port)
	s.httpSrv = &http.Server{
		Addr:         addr,
		Handler:      s.router,
		ReadTimeout:  s.cfg.Server.ReadTimeout,
		WriteTimeout: s.cfg.Server.WriteTimeout,
	}

	errCh := make(chan error, 1)
	go func() {
		s.logger.Info().Str("addr", addr).Msg("http server listening")
		if err := s.httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if err := s.httpSrv.Shutdown(shutdownCtx); err != nil {
			s.logger.Error().Err(err).Msg("http shutdown returned error")
			return err
		}
		<-errCh
		return nil
	case err := <-errCh:
		return err
	}
}
