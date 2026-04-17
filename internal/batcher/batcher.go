package batcher

import (
	"context"

	"github.com/briankim06/adaptive-batching-engine/internal/batcher/strategies"
	"github.com/briankim06/adaptive-batching-engine/internal/config"
	"github.com/briankim06/adaptive-batching-engine/internal/models"
)

// Batcher is the batching-layer contract. See docs/spec/02-batching.md
// §2.6 for the spec-aligned surface.
type Batcher interface {
	// Submit routes a request into the exact QueueMatrix sub-queue for its
	// (Priority, RequestType, TokenBucket) and wakes formBatch. It is
	// non-blocking; returns models.ErrQueueFull when the target sub-queue
	// is at capacity, models.ErrShuttingDown after Stop has been signalled.
	Submit(ctx context.Context, req *models.InferenceRequest) error

	// Start begins the formBatch loop. The loop runs until ctx is
	// cancelled or Stop is called.
	Start(ctx context.Context) error

	// Stop signals shutdown. In-flight batches complete; queued requests
	// receive ErrShuttingDown on their ResultChans.
	Stop(ctx context.Context) error

	// QueueDepth returns the current total across the QueueMatrix.
	QueueDepth() int64

	// Metrics snapshots batcher state.
	Metrics() BatcherMetrics

	// SetStrategy atomically swaps the active batching strategy.
	SetStrategy(s strategies.Strategy)

	// UpdateStrategyMetrics publishes the latest smoothed latency view
	// for the strategy's next CalculateTimeout call.
	UpdateStrategyMetrics(m strategies.StrategyMetrics)
}

// BatcherMetrics is the struct shape exposed on Metrics() and by the API
// layer. Keep field names consistent with docs/spec/02-batching.md §2.6.
type BatcherMetrics struct {
	QueueDepth     int64   `json:"queue_depth"`
	BatchesFormed  int64   `json:"batches_formed"`
	RequestsQueued int64   `json:"requests_queued"`
	AvgBatchSize   float64 `json:"avg_batch_size"`
	ActiveStrategy string  `json:"active_strategy"`
}

// BatcherConfig is the narrow slice of runtime config consumed by the
// batching layer. Strategy-specific knobs (wait/threshold tunables,
// target P99) are consumed by the strategy constructors in NewStrategy.
type BatcherConfig struct {
	MaxBatchSize  int
	MinBatchSize  int
	QueueCapacity int // total across the QueueMatrix; sub-queue cap is derived
	BatchChanSize int // per-priority batchChans[i] buffer size (defaults to workers.count)
}

// NewBatcherConfig projects the runtime config onto BatcherConfig.
// BatchChanSize defaults to the worker pool size.
func NewBatcherConfig(batching config.BatchingConfig, workers config.WorkerConfig) BatcherConfig {
	batchChanSize := workers.Count
	if batchChanSize < 1 {
		batchChanSize = 1
	}
	return BatcherConfig{
		MinBatchSize:  batching.MinBatchSize,
		MaxBatchSize:  batching.MaxBatchSize,
		QueueCapacity: batching.QueueCapacity,
		BatchChanSize: batchChanSize,
	}
}

// NewStrategy constructs the concrete strategies.Strategy selected by
// batching.Strategy. It lives in this package so that strategies/ does
// not need to depend on config/.
func NewStrategy(cfg config.BatchingConfig) (strategies.Strategy, error) {
	switch cfg.Strategy {
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
		const defaultKP = 0.05
		return strategies.NewLatencyAwareStrategy(base, defaultKP, cfg.TargetP99Ms), nil
	default:
		return nil, models.ErrInvalidRequest
	}
}
