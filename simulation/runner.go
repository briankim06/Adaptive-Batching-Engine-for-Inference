package simulation

import (
	"context"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/briankim06/adaptive-batching-engine/internal/batcher"
	"github.com/briankim06/adaptive-batching-engine/internal/batcher/strategies"
	"github.com/briankim06/adaptive-batching-engine/internal/config"
	"github.com/briankim06/adaptive-batching-engine/internal/metrics"
	"github.com/briankim06/adaptive-batching-engine/internal/models"
	"github.com/briankim06/adaptive-batching-engine/internal/worker"
)

const (
	// simRequestTimeout is the upstream request timeout used for sim
	// runs. Short enough that stuck batches abort quickly, long enough
	// that the default mock latency (~15ms + tokens) never trips it.
	simRequestTimeout = 5 * time.Second
	// latencyMaterializeInterval matches the server's choice so the
	// strategy's smoothed-P99 controller behaves identically.
	latencyMaterializeInterval = 100 * time.Millisecond
	// metricsPushInterval ticks the strategy-metrics bridge that feeds
	// LatencyAwareStrategy. 50ms lets the controller react within the
	// 60s scenario window.
	metricsPushInterval = 50 * time.Millisecond
)

// SimulationResult captures the outcome of one scenario × strategy run.
// Every field is serialisable so the CLI can emit JSON directly.
type SimulationResult struct {
	ScenarioName  string        `json:"scenario"`
	StrategyName  string        `json:"strategy"`
	Duration      time.Duration `json:"duration"`
	TotalRequests int64         `json:"total_requests"`
	TotalErrors   int64         `json:"total_errors"`
	ErrorRate     float64       `json:"error_rate"`
	ThroughputRPS float64       `json:"throughput_rps"`
	LatencyP50Ms  float64       `json:"latency_p50_ms"`
	LatencyP90Ms  float64       `json:"latency_p90_ms"`
	LatencyP99Ms  float64       `json:"latency_p99_ms"`
	AvgBatchSize  float64       `json:"avg_batch_size"`
}

// Runner owns the template config and constructs a fresh batcher, pool,
// collector and mock upstream for each Run invocation. Runner is safe to
// call sequentially; concurrent Run calls are not supported because
// Prometheus metric registration is process-global.
type Runner struct {
	cfg *config.Config
}

// NewRunner returns a Runner that will clone cfg for each run, overlay
// the mock upstream's URL, and otherwise exercise the production code
// path end-to-end.
func NewRunner(cfg *config.Config) *Runner {
	return &Runner{cfg: cfg}
}

// poolWorkerAdapter bridges the worker pool's []WorkerInfo into the
// []metrics.WorkerSnapshot that Collector.SetWorkerProvider expects.
// Duplicated from cmd/server/main.go so the simulation package does not
// depend on the server command.
type poolWorkerAdapter struct {
	pool *worker.WorkerPool
}

func (a poolWorkerAdapter) GetWorkerSnapshots() []metrics.WorkerSnapshot {
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

func (a poolWorkerAdapter) UpstreamUnhealthy() bool {
	return a.pool.UpstreamUnhealthy()
}

// Run drives one scenario × strategy combination. It blocks for
// scenario.Duration plus a short grace window, then returns aggregated
// metrics. The caller's ctx can abort a run early.
func (r *Runner) Run(ctx context.Context, scenario Scenario, strategy strategies.Strategy, mockCfg MockUpstreamConfig) SimulationResult {
	mock := NewMockUpstream(mockCfg)
	defer mock.Close()

	cfg := cloneConfigForSim(r.cfg, mock)

	collector := metrics.NewCollector(cfg.Metrics.PercentileWindowSize, cfg.Metrics.P99SmoothingAlpha)
	// The latency tracker's materialize loop feeds the strategy's
	// smoothed P99; run it under the outer ctx so it keeps ticking
	// throughout the run.
	collector.Latency.Start(ctx, latencyMaterializeInterval)

	pool := worker.NewWorkerPool(cfg.Workers, cfg.Health, cfg.Upstream)
	collector.SetWorkerProvider(poolWorkerAdapter{pool: pool})

	batcherCfg := batcher.NewBatcherConfig(cfg.Batching, cfg.Workers)
	adaptive := batcher.NewAdaptiveBatcher(batcherCfg, strategy, pool.BatchChans())
	adaptive.SetDispatchBlockedHook(func(p models.Priority) {
		collector.RecordDispatchBlocked(p.String())
	})

	runCtx, cancel := context.WithTimeout(ctx, scenario.Duration)
	defer cancel()

	g, gCtx := errgroup.WithContext(runCtx)
	g.Go(func() error {
		err := adaptive.Start(gCtx)
		pool.CloseBatchChans()
		return err
	})
	g.Go(func() error {
		return pool.Start(gCtx)
	})
	g.Go(func() error {
		pushStrategyMetrics(gCtx, adaptive, collector, cfg.Batching.TargetP99Ms)
		return nil
	})

	collected := newResultAccumulator()

	start := time.Now()

	var inflight sync.WaitGroup
	reqCh := scenario.Generator.Generate(runCtx)

	for req := range reqCh {
		collected.submitted.Add(1)
		inflight.Add(1)
		go r.handleRequest(req, adaptive, collector, collected, &inflight)
	}

	// Generator channel closed: runCtx expired or was cancelled. Wait
	// for every in-flight request to either produce a result or be
	// short-circuited by shutdown.
	waitWithTimeout(&inflight, 2*time.Second)

	cancel()
	_ = g.Wait()
	elapsed := time.Since(start)

	return collected.build(scenario, strategy, elapsed, adaptive)
}

// handleRequest submits one request, blocks on its result channel, and
// records the outcome into the accumulator. Mirrors the HTTP handler's
// flow so the simulator touches the same code path as production.
func (r *Runner) handleRequest(
	req *models.InferenceRequest,
	adaptive *batcher.AdaptiveBatcher,
	collector *metrics.Collector,
	out *resultAccumulator,
	wg *sync.WaitGroup,
) {
	defer wg.Done()

	collector.RecordRequest()
	if err := adaptive.Submit(req.Ctx, req); err != nil {
		out.recordError()
		collector.RecordError()
		return
	}

	select {
	case res, ok := <-req.ResultChan:
		if !ok || res == nil {
			out.recordError()
			collector.RecordError()
			return
		}
		if res.Error != nil {
			out.recordError()
			collector.RecordError()
			return
		}
		out.recordLatency(res.LatencyMs)
		collector.RecordLatency(res.LatencyMs)
	case <-req.Ctx.Done():
		out.recordError()
		collector.RecordError()
	}
}

// resultAccumulator captures per-request outcomes independent of the
// collector's Prometheus pipeline so the Runner can compute exact
// percentiles at the end of the run (the collector's percentiles are
// reservoir-sampled).
type resultAccumulator struct {
	mu        sync.Mutex
	latencies []float64
	submitted atomic.Int64
	completed atomic.Int64
	errors    atomic.Int64
}

func newResultAccumulator() *resultAccumulator {
	return &resultAccumulator{latencies: make([]float64, 0, 4096)}
}

func (r *resultAccumulator) recordLatency(ms float64) {
	r.completed.Add(1)
	r.mu.Lock()
	r.latencies = append(r.latencies, ms)
	r.mu.Unlock()
}

func (r *resultAccumulator) recordError() {
	r.errors.Add(1)
}

func (r *resultAccumulator) build(
	scenario Scenario,
	strategy strategies.Strategy,
	elapsed time.Duration,
	adaptive *batcher.AdaptiveBatcher,
) SimulationResult {
	r.mu.Lock()
	samples := make([]float64, len(r.latencies))
	copy(samples, r.latencies)
	r.mu.Unlock()

	sort.Float64s(samples)

	submitted := r.submitted.Load()
	errs := r.errors.Load()
	errRate := 0.0
	if submitted > 0 {
		errRate = float64(errs) / float64(submitted)
	}

	rps := 0.0
	if elapsed > 0 {
		rps = float64(submitted-errs) / elapsed.Seconds()
	}

	strategyName := ""
	if strategy != nil {
		strategyName = strategy.Name()
	}

	bm := adaptive.Metrics()

	return SimulationResult{
		ScenarioName:  scenario.Name,
		StrategyName:  strategyName,
		Duration:      elapsed,
		TotalRequests: submitted,
		TotalErrors:   errs,
		ErrorRate:     errRate,
		ThroughputRPS: rps,
		LatencyP50Ms:  percentile(samples, 50),
		LatencyP90Ms:  percentile(samples, 90),
		LatencyP99Ms:  percentile(samples, 99),
		AvgBatchSize:  bm.AvgBatchSize,
	}
}

// cloneConfigForSim copies cfg and overlays the simulation-specific
// upstream URL, health path, and request timeout. The receiver's cfg is
// never mutated so multiple Run calls share a single template.
func cloneConfigForSim(src *config.Config, mock *MockUpstream) *config.Config {
	dup := *src
	dup.Upstream = src.Upstream
	dup.Upstream.URL = mock.URL() + "/v1/batch"
	dup.Upstream.HealthPath = mock.HealthPath()
	if src.Upstream.RequestTimeout <= 0 || src.Upstream.RequestTimeout > simRequestTimeout {
		dup.Upstream.RequestTimeout = simRequestTimeout
	}
	return &dup
}

// pushStrategyMetrics is a trimmed port of runMetricsPush from
// cmd/server/main.go: it bridges the live P99 into StrategyMetrics so
// latency_aware's controller responds to load during the run.
func pushStrategyMetrics(ctx context.Context, b *batcher.AdaptiveBatcher, c *metrics.Collector, targetP99Ms float64) {
	tick := time.NewTicker(metricsPushInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			b.UpdateStrategyMetrics(strategies.StrategyMetrics{
				P99LatencyMs: c.Latency.P99Smoothed(),
				TargetP99Ms:  targetP99Ms,
			})
		}
	}
}

// waitWithTimeout blocks until wg is done or the timeout elapses. The
// timeout protects against stuck in-flight requests during shutdown —
// any still-pending ResultChan read will be released by the batcher's
// drain-on-stop fanout.
func waitWithTimeout(wg *sync.WaitGroup, timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

// percentile returns p-th percentile of a pre-sorted slice. Returns 0
// for an empty input rather than panicking so the runner's result
// record is well-defined even if no successes were recorded.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)) * p / 100)
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	if idx < 0 {
		idx = 0
	}
	return sorted[idx]
}

// ErrNoStrategy is returned by helpers when a caller forgot to supply a
// strategy. Declared here rather than in models/errors.go because it is
// simulation-specific.
var ErrNoStrategy = errors.New("simulation: strategy is required")
