package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"golang.org/x/sync/errgroup"

	"github.com/briankim06/adaptive-batching-engine/internal/api"
	"github.com/briankim06/adaptive-batching-engine/internal/api/handlers"
	"github.com/briankim06/adaptive-batching-engine/internal/batcher"
	"github.com/briankim06/adaptive-batching-engine/internal/batcher/strategies"
	"github.com/briankim06/adaptive-batching-engine/internal/config"
	"github.com/briankim06/adaptive-batching-engine/internal/metrics"
	"github.com/briankim06/adaptive-batching-engine/internal/models"
	"github.com/briankim06/adaptive-batching-engine/internal/worker"
)

const (
	latencyMaterializeInterval = 100 * time.Millisecond
	latencyAwareKP             = 0.05
)

// workerSnapshotAdapter satisfies metrics.WorkerInfoProvider by projecting
// the pool's []worker.WorkerInfo into []metrics.WorkerSnapshot. Keeping
// this tiny type in main.go avoids pulling the metrics package into
// internal/worker.
type workerSnapshotAdapter struct {
	pool *worker.WorkerPool
}

func (a workerSnapshotAdapter) GetWorkerSnapshots() []metrics.WorkerSnapshot {
	infos := a.pool.GetStatus()
	out := make([]metrics.WorkerSnapshot, 0, len(infos))
	for _, info := range infos {
		out = append(out, metrics.WorkerSnapshot{
			ID:               info.ID,
			Status:           int(info.Status),
			CircuitState:     int(info.CircuitState),
			BatchesProcessed: info.BatchesProcessed,
			TokensProcessed:  info.TokensProcessed,
		})
	}
	return out
}

func (a workerSnapshotAdapter) UpstreamUnhealthy() bool {
	return a.pool.UpstreamUnhealthy()
}

func main() {
	logger := zerolog.New(os.Stderr).With().Timestamp().Logger()

	cfg, err := config.Load()
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to load config")
	}

	signalCtx, stopSignal := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignal()

	collector := metrics.NewCollector(cfg.Metrics.PercentileWindowSize, cfg.Metrics.P99SmoothingAlpha)
	collector.Latency.Start(signalCtx, latencyMaterializeInterval)

	aggregator := metrics.NewTimeSeriesAggregator()

	pool := worker.NewWorkerPool(cfg.Workers, cfg.Health, cfg.Upstream)
	collector.SetWorkerProvider(workerSnapshotAdapter{pool: pool})

	strategy, err := buildInitialStrategy(cfg.Batching)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to build initial strategy")
	}

	batcherCfg := batcher.NewBatcherConfig(cfg.Batching, cfg.Workers)
	adaptive := batcher.NewAdaptiveBatcher(batcherCfg, strategy, pool.BatchChans())
	adaptive.SetDispatchBlockedHook(func(p models.Priority) {
		collector.RecordDispatchBlocked(p.String())
	})

	publisher := handlers.NewSnapshotPublisher()

	server := api.NewServer(cfg, adaptive, pool, collector, aggregator, publisher, logger)

	publisher.Start(signalCtx, collector, cfg.Dashboard.WSPushInterval)
	aggregator.StartDownsampler(signalCtx)

	g, gCtx := errgroup.WithContext(signalCtx)

	// Batcher + pool channel-close coordination: as soon as the batcher's
	// drain finishes, close the pool's batch channels so workers observe
	// the closure and exit their priority-select loops.
	g.Go(func() error {
		err := adaptive.Start(gCtx)
		pool.CloseBatchChans()
		return err
	})

	g.Go(func() error {
		return pool.Start(gCtx)
	})

	g.Go(func() error {
		return server.Start(gCtx)
	})

	g.Go(func() error {
		return runMetricsPush(gCtx, cfg.Metrics.CollectionInterval, cfg.Batching.TargetP99Ms, adaptive, collector, aggregator, pool)
	})

	logger.Info().
		Str("host", cfg.Server.Host).
		Int("port", cfg.Server.Port).
		Str("strategy", cfg.Batching.Strategy).
		Int("workers", cfg.Workers.Count).
		Msg("adaptive batching engine starting")

	if err := g.Wait(); err != nil && err != context.Canceled {
		logger.Error().Err(err).Msg("shutdown with error")
	} else {
		logger.Info().Msg("shutdown clean")
	}
}

func buildInitialStrategy(cfg config.BatchingConfig) (strategies.Strategy, error) {
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
		return strategies.NewLatencyAwareStrategy(base, latencyAwareKP, cfg.TargetP99Ms), nil
	default:
		return nil, errUnknownStrategy(cfg.Strategy)
	}
}

type errUnknownStrategy string

func (e errUnknownStrategy) Error() string { return "unknown batching.strategy: " + string(e) }

// runMetricsPush is the periodic bridge between the live component state
// (batcher queue depth, pool health, latency tracker) and the metrics
// subsystem. It ticks at cfg.Metrics.CollectionInterval.
func runMetricsPush(
	ctx context.Context,
	interval time.Duration,
	targetP99Ms float64,
	b *batcher.AdaptiveBatcher,
	c *metrics.Collector,
	agg *metrics.TimeSeriesAggregator,
	pool *worker.WorkerPool,
) error {
	if interval <= 0 {
		interval = 100 * time.Millisecond
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			queueDepth := b.QueueDepth()
			c.SetQueueDepth(queueDepth)
			c.SetUpstreamUnhealthy(pool.UpstreamUnhealthy())

			for _, info := range pool.GetStatus() {
				c.SetWorkerStatus(info.ID, int(info.Status))
				c.SetCircuitState(info.ID, int(info.CircuitState))
			}

			snap := c.BuildSnapshot()
			agg.Add("queue_depth", float64(queueDepth))
			agg.Add("latency_p99", c.Latency.P99())
			agg.Add("latency_p99_smoothed", c.Latency.P99Smoothed())
			agg.Add("throughput_rps", snap.ThroughputRPS)

			b.UpdateStrategyMetrics(strategies.StrategyMetrics{
				P99LatencyMs: c.Latency.P99Smoothed(),
				TargetP99Ms:  targetP99Ms,
			})
		}
	}
}
