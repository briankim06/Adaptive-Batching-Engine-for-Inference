package middleware

import (
	"context"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/briankim06/adaptive-batching-engine/internal/metrics"
)

type ctxKey int

const (
	ctxKeyRequestID ctxKey = iota
)

const headerRequestID = "X-Request-ID"

// RequestID generates a UUID per request, sets X-Request-ID on the
// response, and stashes it in the request context for downstream handlers.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(headerRequestID)
		if id == "" {
			id = uuid.New().String()
		}
		w.Header().Set(headerRequestID, id)
		ctx := context.WithValue(r.Context(), ctxKeyRequestID, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// GetRequestID returns the request id attached to ctx by RequestID. Empty
// string if not set.
func GetRequestID(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyRequestID).(string); ok {
		return v
	}
	return ""
}

// statusRecorder captures the status code written by downstream handlers
// so the Logging and MetricsMiddleware wrappers can observe it.
type statusRecorder struct {
	http.ResponseWriter
	status int
	size   int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.size += n
	return n, err
}

// Logging emits one zerolog line per request with method, path, status,
// duration, and request id.
func Logging(logger zerolog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)
			status := rec.status
			if status == 0 {
				status = http.StatusOK
			}
			logger.Info().
				Str("request_id", GetRequestID(r.Context())).
				Str("method", r.Method).
				Str("path", r.URL.Path).
				Int("status", status).
				Dur("duration", time.Since(start)).
				Msg("http_request")
		})
	}
}

// MetricsMiddleware records the request's end-to-end latency into the
// Prometheus histogram once the response has been written.
func MetricsMiddleware(collector *metrics.Collector) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			next.ServeHTTP(w, r)
			if collector != nil {
				collector.RecordLatency(float64(time.Since(start).Milliseconds()))
			}
		})
	}
}

// Recovery catches panics from downstream handlers, logs a stack trace,
// and returns 500 to the client.
func Recovery(logger zerolog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					logger.Error().
						Str("request_id", GetRequestID(r.Context())).
						Str("path", r.URL.Path).
						Interface("panic", rec).
						Bytes("stack", debug.Stack()).
						Msg("panic recovered")
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = w.Write([]byte(`{"error":{"message":"internal server error","type":"internal"}}`))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}
