package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/briankim06/adaptive-batching-engine/internal/batcher"
	"github.com/briankim06/adaptive-batching-engine/internal/batcher/strategies"
	"github.com/briankim06/adaptive-batching-engine/internal/config"
	"github.com/briankim06/adaptive-batching-engine/internal/worker"
)

// HandleGetConfig returns the current runtime configuration as JSON.
func HandleGetConfig(cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, cfg)
	}
}

type setStrategyRequest struct {
	Strategy string `json:"strategy"`
}

type setStrategyResponse struct {
	Strategy string `json:"strategy"`
}

// HandleSetStrategy swaps the batcher's active strategy at runtime. It
// reconstructs the selected strategy from the batching config so the
// knobs stay authoritative in config.yaml.
func HandleSetStrategy(b batcher.Batcher, batchingCfg *config.BatchingConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req setStrategyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
			return
		}
		strategy, err := buildStrategy(req.Strategy, batchingCfg)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		b.SetStrategy(strategy)
		writeJSON(w, http.StatusOK, setStrategyResponse{Strategy: strategy.Name()})
	}
}

func buildStrategy(name string, cfg *config.BatchingConfig) (strategies.Strategy, error) {
	if cfg == nil {
		return nil, errInvalidStrategy(name)
	}
	switch name {
	case "fixed":
		return strategies.NewFixedStrategy(cfg.MaxWaitMs, cfg.MaxBatchSize), nil
	case "queue_depth":
		return strategies.NewQueueDepthStrategy(
			cfg.QueueDepthLowThreshold,
			cfg.QueueDepthHighThreshold,
			cfg.MinWaitMs,
			cfg.MaxWaitMs,
			cfg.MaxBatchSize,
		), nil
	case "latency_aware":
		base := strategies.NewQueueDepthStrategy(
			cfg.QueueDepthLowThreshold,
			cfg.QueueDepthHighThreshold,
			cfg.MinWaitMs,
			cfg.MaxWaitMs,
			cfg.MaxBatchSize,
		)
		return strategies.NewLatencyAwareStrategy(base, 0.05, cfg.TargetP99Ms), nil
	default:
		return nil, errInvalidStrategy(name)
	}
}

type invalidStrategyError struct{ name string }

func (e invalidStrategyError) Error() string {
	return "unknown strategy: " + e.name
}

func errInvalidStrategy(name string) error {
	return invalidStrategyError{name: name}
}

// HandleGetWorkers returns the pool's per-worker status array.
func HandleGetWorkers(pool *worker.WorkerPool) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if pool == nil {
			writeJSON(w, http.StatusOK, []worker.WorkerInfo{})
			return
		}
		writeJSON(w, http.StatusOK, pool.GetStatus())
	}
}

// HandleHealth is the liveness check mounted at /health.
func HandleHealth() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}
