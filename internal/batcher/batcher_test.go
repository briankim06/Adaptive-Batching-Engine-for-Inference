package batcher

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/briankim06/adaptive-batching-engine/internal/batcher/strategies"
	"github.com/briankim06/adaptive-batching-engine/internal/config"
	"github.com/briankim06/adaptive-batching-engine/internal/models"
)

type stubStrategy struct {
	name        string
	timeout     time.Duration
	shouldFlush bool
}

func (s *stubStrategy) Name() string {
	if s.name == "" {
		return "stub"
	}
	return s.name
}

func (s *stubStrategy) CalculateTimeout(int, *strategies.StrategyMetrics) time.Duration {
	return s.timeout
}

func (s *stubStrategy) ShouldFlush([]*models.InferenceRequest, int) bool { return s.shouldFlush }

func newTestBatchChans(size int) [4]chan *models.Batch {
	if size < 1 {
		size = 1
	}
	var chans [4]chan *models.Batch
	for i := range chans {
		chans[i] = make(chan *models.Batch, size)
	}
	return chans
}

func newTestBatcher(t *testing.T, cfg BatcherConfig, strategy strategies.Strategy) (*AdaptiveBatcher, [4]chan *models.Batch) {
	t.Helper()
	if cfg.BatchChanSize < 1 {
		cfg.BatchChanSize = 1
	}
	chans := newTestBatchChans(cfg.BatchChanSize)
	return NewAdaptiveBatcher(cfg, strategy, chans), chans
}

func makeReq(priority models.Priority) *models.InferenceRequest {
	return models.NewInferenceRequest(context.Background(), "prompt", 0, priority, models.RequestTypeCompletion)
}

func TestNewBatcherConfigProjectsFields(t *testing.T) {
	batching := config.BatchingConfig{
		Strategy:                "fixed",
		MinBatchSize:            1,
		MaxBatchSize:            32,
		MinWaitMs:               5,
		MaxWaitMs:               100,
		QueueDepthLowThreshold:  10,
		QueueDepthHighThreshold: 100,
		QueueCapacity:           1000,
		PriorityEnabled:         true,
		TargetP99Ms:             50.0,
	}
	workers := config.WorkerConfig{Count: 4}

	cfg := NewBatcherConfig(batching, workers)
	if cfg.MinBatchSize != 1 || cfg.MaxBatchSize != 32 || cfg.QueueCapacity != 1000 || cfg.BatchChanSize != 4 {
		t.Fatalf("unexpected projection: %+v", cfg)
	}
}

func TestNewBatcherConfigBatchChanSizeFloor(t *testing.T) {
	cfg := NewBatcherConfig(config.BatchingConfig{QueueCapacity: 1}, config.WorkerConfig{Count: 0})
	if cfg.BatchChanSize != 1 {
		t.Fatalf("expected BatchChanSize floor of 1, got %d", cfg.BatchChanSize)
	}
}

func TestNewStrategySelectsKind(t *testing.T) {
	cases := []struct {
		name     string
		strategy string
		expect   string
	}{
		{"fixed", "fixed", "fixed"},
		{"queue_depth", "queue_depth", "queue_depth"},
		{"latency_aware", "latency_aware", "latency_aware"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			strat, err := NewStrategy(config.BatchingConfig{
				Strategy:                tc.strategy,
				MaxBatchSize:            8,
				MinWaitMs:               5,
				MaxWaitMs:               50,
				QueueDepthLowThreshold:  10,
				QueueDepthHighThreshold: 100,
				TargetP99Ms:             50,
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if strat.Name() != tc.expect {
				t.Fatalf("expected %s, got %s", tc.expect, strat.Name())
			}
		})
	}
}

func TestNewStrategyRejectsUnknown(t *testing.T) {
	if _, err := NewStrategy(config.BatchingConfig{Strategy: "mystery"}); err == nil {
		t.Fatal("expected error for unknown strategy")
	}
}

func TestAdaptiveBatcherSubmitAndMetrics(t *testing.T) {
	cfg := BatcherConfig{MaxBatchSize: 2, QueueCapacity: 10, BatchChanSize: 1}
	batcher, _ := newTestBatcher(t, cfg, &stubStrategy{timeout: 10 * time.Millisecond, name: "stub"})

	if err := batcher.Submit(context.Background(), makeReq(models.PriorityNormal)); err != nil {
		t.Fatalf("unexpected submit error: %v", err)
	}

	if depth := batcher.QueueDepth(); depth != 1 {
		t.Fatalf("expected queue depth 1, got %d", depth)
	}

	m := batcher.Metrics()
	if m.RequestsQueued != 1 {
		t.Fatalf("expected requests queued 1, got %d", m.RequestsQueued)
	}
	if m.BatchesFormed != 0 || m.AvgBatchSize != 0 {
		t.Fatalf("expected no batches formed yet, got %+v", m)
	}
	if m.ActiveStrategy != "stub" {
		t.Fatalf("expected ActiveStrategy=stub, got %q", m.ActiveStrategy)
	}
}

func TestAdaptiveBatcherDispatchRoutesByPriority(t *testing.T) {
	cfg := BatcherConfig{MaxBatchSize: 1, QueueCapacity: 10, BatchChanSize: 1}
	batcher, chans := newTestBatcher(t, cfg, &stubStrategy{timeout: 2 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go batcher.Start(ctx)

	if err := batcher.Submit(context.Background(), makeReq(models.PriorityCritical)); err != nil {
		t.Fatalf("unexpected submit error: %v", err)
	}

	select {
	case batch := <-chans[models.PriorityCritical]:
		if batch == nil || batch.Size() != 1 {
			t.Fatalf("expected critical-priority batch of size 1, got %v", batch)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timed out waiting for critical-priority dispatch")
	}

	cancel()
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer stopCancel()
	if err := batcher.Stop(stopCtx); err != nil {
		t.Fatalf("unexpected stop error: %v", err)
	}
}

func TestAdaptiveBatcherDrainsOnStopWithoutStart(t *testing.T) {
	// Exercises the shutdown path when the batcher was built but never
	// started. Any queued requests must still receive ErrShuttingDown so
	// HTTP handlers blocked on ResultChan are unblocked cleanly.
	// QueueCapacity of 64 ⇒ per-sub-queue cap of 4, which comfortably
	// holds the three same-trait requests below.
	cfg := BatcherConfig{MaxBatchSize: 2, QueueCapacity: 64, BatchChanSize: 1}
	batcher, _ := newTestBatcher(t, cfg, &stubStrategy{timeout: time.Second})

	var submitted []*models.InferenceRequest
	for i := 0; i < 3; i++ {
		req := makeReq(models.PriorityNormal)
		if err := batcher.Submit(context.Background(), req); err != nil {
			t.Fatalf("unexpected submit error: %v", err)
		}
		submitted = append(submitted, req)
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer stopCancel()
	if err := batcher.Stop(stopCtx); err != nil {
		t.Fatalf("unexpected stop error: %v", err)
	}

	for _, req := range submitted {
		select {
		case res := <-req.ResultChan:
			if !errors.Is(res.Error, models.ErrShuttingDown) {
				t.Fatalf("expected ErrShuttingDown on drain, got %v", res)
			}
		case <-time.After(200 * time.Millisecond):
			t.Fatalf("timed out waiting for shutdown result for %s", req.ID)
		}
	}

	if err := batcher.Submit(context.Background(), makeReq(models.PriorityNormal)); !errors.Is(err, models.ErrShuttingDown) {
		t.Fatalf("expected ErrShuttingDown on post-Stop submit, got %v", err)
	}
}

func TestAdaptiveBatcherSubmitRejectsAfterStop(t *testing.T) {
	cfg := BatcherConfig{MaxBatchSize: 1, QueueCapacity: 10, BatchChanSize: 1}
	batcher, _ := newTestBatcher(t, cfg, &stubStrategy{timeout: 5 * time.Millisecond})

	stopCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := batcher.Stop(stopCtx); err != nil {
		t.Fatalf("unexpected stop error: %v", err)
	}

	err := batcher.Submit(context.Background(), makeReq(models.PriorityNormal))
	if !errors.Is(err, models.ErrShuttingDown) {
		t.Fatalf("expected ErrShuttingDown after Stop, got %v", err)
	}
}

func TestAdaptiveBatcherSubmitPropagatesQueueFull(t *testing.T) {
	cfg := BatcherConfig{MaxBatchSize: 1, QueueCapacity: 1, BatchChanSize: 1}
	batcher, _ := newTestBatcher(t, cfg, &stubStrategy{timeout: time.Second})

	// Fill the sub-queue for (Normal, Completion, bucket 0).
	req := models.NewInferenceRequest(context.Background(), "short", 0, models.PriorityNormal, models.RequestTypeCompletion)
	if err := batcher.Submit(context.Background(), req); err != nil {
		t.Fatalf("unexpected first submit error: %v", err)
	}
	// Same traits → same sub-queue.
	err := batcher.Submit(context.Background(), models.NewInferenceRequest(context.Background(), "short2", 0, models.PriorityNormal, models.RequestTypeCompletion))
	if !errors.Is(err, models.ErrQueueFull) {
		t.Fatalf("expected ErrQueueFull on full sub-queue, got %v", err)
	}
}

func TestAdaptiveBatcherStrategyHotSwap(t *testing.T) {
	cfg := BatcherConfig{MaxBatchSize: 1, QueueCapacity: 10, BatchChanSize: 1}
	initial := &stubStrategy{name: "first", timeout: 5 * time.Millisecond}
	batcher, _ := newTestBatcher(t, cfg, initial)

	if m := batcher.Metrics(); m.ActiveStrategy != "first" {
		t.Fatalf("expected first strategy, got %q", m.ActiveStrategy)
	}

	replacement := &stubStrategy{name: "second", timeout: 5 * time.Millisecond}
	batcher.SetStrategy(replacement)

	if m := batcher.Metrics(); m.ActiveStrategy != "second" {
		t.Fatalf("expected strategy hot-swap to second, got %q", m.ActiveStrategy)
	}

	batcher.SetStrategy(nil) // should be a no-op
	if m := batcher.Metrics(); m.ActiveStrategy != "second" {
		t.Fatalf("nil SetStrategy should not change state, got %q", m.ActiveStrategy)
	}
}

func TestAdaptiveBatcherUpdateStrategyMetricsFeedsStrategy(t *testing.T) {
	cfg := BatcherConfig{MaxBatchSize: 1, QueueCapacity: 10, BatchChanSize: 1}
	rec := &metricsObservingStrategy{timeout: 10 * time.Millisecond}
	batcher, _ := newTestBatcher(t, cfg, rec)

	batcher.UpdateStrategyMetrics(strategies.StrategyMetrics{P99LatencyMs: 42, TargetP99Ms: 50})

	// Drive the hot path once; it reads the latest metrics pointer.
	batcher.nextTimeout(batcher.activeStrategy())

	if rec.observed == nil {
		t.Fatal("expected strategy to observe StrategyMetrics from UpdateStrategyMetrics")
	}
	if rec.observed.P99LatencyMs != 42 || rec.observed.TargetP99Ms != 50 {
		t.Fatalf("unexpected metrics observed: %+v", rec.observed)
	}
}

// TestAdaptiveBatcherAccumulatesWithinWindow proves the two-phase
// formBatch actually holds a batch open for the strategy's timeout
// instead of draining on every EventChan wake. We submit 8 same-key
// requests back-to-back and expect one batch ≥ 4 to form within the
// 50ms accumulation window — the pre-refactor behaviour would yield
// ~8 micro-batches of size 1.
func TestAdaptiveBatcherAccumulatesWithinWindow(t *testing.T) {
	cfg := BatcherConfig{MaxBatchSize: 16, QueueCapacity: 256, BatchChanSize: 4}
	batcher, chans := newTestBatcher(t, cfg, &stubStrategy{timeout: 50 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go batcher.Start(ctx)

	for i := 0; i < 8; i++ {
		req := models.NewInferenceRequest(context.Background(), "x", 0,
			models.PriorityNormal, models.RequestTypeCompletion)
		if err := batcher.Submit(context.Background(), req); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}

	select {
	case batch := <-chans[models.PriorityNormal]:
		if batch == nil {
			t.Fatal("expected non-nil batch")
		}
		if batch.Size() < 4 {
			t.Fatalf("expected accumulation to yield batch ≥ 4, got size %d — formBatch is not holding a window open", batch.Size())
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for accumulated batch")
	}

	cancel()
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer stopCancel()
	_ = batcher.Stop(stopCtx)
}

// TestAdaptiveBatcherPreemptsOnHigherPriority asserts that a Critical
// request arriving mid-accumulation breaks the window early: the lower-
// priority batch is flushed with what it has, and the next formBatch
// iteration dispatches the Critical item without waiting the full
// timeout. Guards I8 at batch-formation time.
func TestAdaptiveBatcherPreemptsOnHigherPriority(t *testing.T) {
	cfg := BatcherConfig{MaxBatchSize: 16, QueueCapacity: 256, BatchChanSize: 4}
	// 80ms accumulation window — long enough that the Normal batch is
	// genuinely sitting in Phase 2 when the Critical arrives at t=30ms,
	// but short enough that the Critical's own accumulation window also
	// closes well within the test deadline.
	batcher, chans := newTestBatcher(t, cfg, &stubStrategy{timeout: 80 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go batcher.Start(ctx)

	// Seed the Normal sub-queue so formBatch is sitting in accumulation.
	for i := 0; i < 2; i++ {
		req := models.NewInferenceRequest(context.Background(), "x", 0,
			models.PriorityNormal, models.RequestTypeCompletion)
		if err := batcher.Submit(context.Background(), req); err != nil {
			t.Fatalf("seed submit %d: %v", i, err)
		}
	}

	// Give the batcher a moment to enter Phase 2 for the Normal batch.
	// Short enough to stay well inside the 80ms window.
	time.Sleep(20 * time.Millisecond)

	critical := models.NewInferenceRequest(context.Background(), "c", 0,
		models.PriorityCritical, models.RequestTypeCompletion)
	if err := batcher.Submit(context.Background(), critical); err != nil {
		t.Fatalf("critical submit: %v", err)
	}

	// Budget: Normal preempts at ~t=20ms, Critical's own 80ms window
	// closes by ~t=100ms, plus slack for scheduling.
	deadline := time.After(500 * time.Millisecond)
	var gotNormal, gotCritical bool
	for !(gotNormal && gotCritical) {
		select {
		case b := <-chans[models.PriorityNormal]:
			if b == nil {
				t.Fatal("nil normal batch")
			}
			gotNormal = true
		case b := <-chans[models.PriorityCritical]:
			if b == nil {
				t.Fatal("nil critical batch")
			}
			if b.Size() != 1 {
				t.Fatalf("expected critical batch of size 1, got %d", b.Size())
			}
			gotCritical = true
		case <-deadline:
			t.Fatalf("preemption timed out (normal=%v critical=%v) — window did not break on higher-priority arrival",
				gotNormal, gotCritical)
		}
	}

	cancel()
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer stopCancel()
	_ = batcher.Stop(stopCtx)
}

type metricsObservingStrategy struct {
	timeout  time.Duration
	observed *strategies.StrategyMetrics
}

func (s *metricsObservingStrategy) Name() string { return "observer" }

func (s *metricsObservingStrategy) CalculateTimeout(_ int, m *strategies.StrategyMetrics) time.Duration {
	if m != nil {
		cp := *m
		s.observed = &cp
	}
	return s.timeout
}

func (s *metricsObservingStrategy) ShouldFlush([]*models.InferenceRequest, int) bool { return false }
