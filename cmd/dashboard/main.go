// Command dashboard serves the monitoring UI on cfg.Dashboard.Port. It is
// intentionally minimal: a static-file server for dashboard/static/ with the
// API WebSocket URL injected into index.html at serve time. See
// docs/spec/06-dashboard.md Task 6.2.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	"github.com/briankim06/adaptive-batching-engine/internal/config"
)

const (
	defaultWSURL     = "ws://localhost:8080/ws"
	defaultStaticDir = "dashboard/static"
	shutdownGrace    = 5 * time.Second
)

type indexData struct {
	WSURL string
}

func main() {
	apiURL := flag.String("api-url", "", "WebSocket URL of the API server (default "+defaultWSURL+")")
	staticDir := flag.String("static-dir", defaultStaticDir, "Directory containing dashboard static assets")
	flag.Parse()

	logger := zerolog.New(os.Stderr).With().Timestamp().Logger()

	cfg, err := config.Load()
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to load config")
	}
	if !cfg.Dashboard.Enabled {
		logger.Info().Msg("dashboard disabled in config; exiting")
		return
	}

	wsURL := strings.TrimSpace(*apiURL)
	if wsURL == "" {
		wsURL = defaultWSURL
	}

	indexPath := filepath.Join(*staticDir, "index.html")
	tmpl, err := template.ParseFiles(indexPath)
	if err != nil {
		logger.Fatal().Err(err).Str("path", indexPath).Msg("failed to parse index template")
	}

	mux := buildMux(*staticDir, tmpl, wsURL, logger)

	addr := fmt.Sprintf(":%d", cfg.Dashboard.Port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	signalCtx, stopSignal := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignal()

	errCh := make(chan error, 1)
	go func() {
		logger.Info().
			Str("addr", addr).
			Str("ws_url", wsURL).
			Str("static_dir", *staticDir).
			Msg("dashboard server listening")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-signalCtx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error().Err(err).Msg("dashboard shutdown error")
		}
		<-errCh
		logger.Info().Msg("dashboard shutdown clean")
	case err := <-errCh:
		if err != nil {
			logger.Fatal().Err(err).Msg("dashboard server error")
		}
	}
}

// buildMux wires the root index route (template-rendered with wsURL) and
// delegates every other path to http.FileServer rooted at staticDir.
func buildMux(staticDir string, tmpl *template.Template, wsURL string, logger zerolog.Logger) *http.ServeMux {
	mux := http.NewServeMux()
	fileServer := http.FileServer(http.Dir(staticDir))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/", "/index.html":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			if err := tmpl.Execute(w, indexData{WSURL: wsURL}); err != nil {
				logger.Error().Err(err).Msg("index template execute")
			}
		default:
			fileServer.ServeHTTP(w, r)
		}
	})

	return mux
}
