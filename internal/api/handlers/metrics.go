package handlers

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/briankim06/adaptive-batching-engine/internal/metrics"
)

// HandlePrometheusMetrics exposes the default promhttp handler for
// /metrics.
func HandlePrometheusMetrics() http.Handler {
	return metrics.PrometheusHandler()
}

// HandleJSONMetrics returns the current collector snapshot as JSON.
func HandleJSONMetrics(collector *metrics.Collector) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if collector == nil {
			writeError(w, http.StatusInternalServerError, "internal", "collector not initialised")
			return
		}
		body, err := metrics.JSONSnapshot(collector)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}
}

// HandleMetricsHistory serves time-series points for a single metric over
// the query duration (default 60s).
func HandleMetricsHistory(agg *metrics.TimeSeriesAggregator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if agg == nil {
			writeError(w, http.StatusInternalServerError, "internal", "aggregator not initialised")
			return
		}
		q := r.URL.Query()
		metric := q.Get("metric")
		if metric == "" {
			writeError(w, http.StatusBadRequest, "invalid_request", "metric query param is required")
			return
		}
		durStr := q.Get("duration")
		if durStr == "" {
			durStr = "60s"
		}
		duration, err := time.ParseDuration(durStr)
		if err != nil || duration <= 0 {
			writeError(w, http.StatusBadRequest, "invalid_request", "duration must be a positive Go duration string")
			return
		}
		points := agg.GetHistory(metric, duration)
		if points == nil {
			points = []metrics.DataPoint{}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(points)
	}
}
