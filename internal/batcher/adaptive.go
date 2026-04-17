package batcher

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/briankim06/adaptive-batching-engine/internal/batcher/packing"
	"github.com/briankim06/adaptive-batching-engine/internal/batcher/strategies"
	"github.com/briankim06/adaptive-batching-engine/internal/models"
)

const (
	// dispatchBlockedThreshold matches docs/spec/02-batching.md §2.7:
	// when a priority-indexed send takes longer than this, record a
	// dispatch-blocked increment so operators can see backpressure.
	dispatchBlockedThreshold = time.Millisecond

	// minFormBatchWait guards the formBatch loop from a zero/negative
	// timeout that would spin. A 1ms floor still lets the event channel
	// drive flushes immediately on Submit.
	minFormBatchWait = time.Millisecond
)

// AdaptiveBatcher owns the QueueMatrix and runs the formBatch loop. It
// dispatches homogeneous batches onto one of four priority-indexed
// channels owned by the worker pool.
type AdaptiveBatcher struct {
	cfg           BatcherConfig
	qm            *packing.QueueMatrix
	strategy      atomic.Pointer[strategies.Strategy]
	metrics       atomic.Pointer[strategies.StrategyMetrics]
	batchChans    [4]chan *models.Batch
	dispatchHook  atomic.Pointer[dispatchHook]

	startOnce sync.Once
	stopOnce  sync.Once
	stopCh    chan struct{}
	stoppedCh chan struct{}
	started   atomic.Bool
	stopping  atomic.Bool

	batchesFormed          atomic.Int64
	requestsQueued         atomic.Int64
	totalRequestsInBatches atomic.Int64
}

// dispatchHook is wrapped in an atomic.Pointer so it can be rebound after
// construction without a mutex.
type dispatchHook struct {
	fn func(priority models.Priority)
}

// NewAdaptiveBatcher wires the QueueMatrix and priority-indexed batch
// channels. The caller (WorkerPool in Phase 3) owns batchChans.
func NewAdaptiveBatcher(cfg BatcherConfig, strategy strategies.Strategy, batchChans [4]chan *models.Batch) *AdaptiveBatcher {
	if cfg.MaxBatchSize < 1 {
		cfg.MaxBatchSize = 1
	}
	if cfg.QueueCapacity < 1 {
		cfg.QueueCapacity = 1
	}
	b := &AdaptiveBatcher{
		cfg:        cfg,
		qm:         packing.NewQueueMatrix(cfg.QueueCapacity),
		batchChans: batchChans,
		stopCh:     make(chan struct{}),
		stoppedCh:  make(chan struct{}),
	}
	if strategy != nil {
		b.strategy.Store(&strategy)
	}
	return b
}

// SetStrategy atomically swaps the active strategy. A nil argument is a
// no-op to avoid racing a reader onto a zero interface.
func (b *AdaptiveBatcher) SetStrategy(s strategies.Strategy) {
	if s == nil {
		return
	}
	b.strategy.Store(&s)
}

// UpdateStrategyMetrics publishes the latest live metrics (e.g. smoothed
// P99) for the strategy's next CalculateTimeout call.
func (b *AdaptiveBatcher) UpdateStrategyMetrics(m strategies.StrategyMetrics) {
	b.metrics.Store(&m)
}

// SetDispatchBlockedHook registers an optional callback invoked when a
// priority-indexed send blocks longer than dispatchBlockedThreshold. It
// is a seam for the Phase 4 metrics wiring to bind
// batcher_dispatch_blocked_total without coupling this package to it.
func (b *AdaptiveBatcher) SetDispatchBlockedHook(fn func(priority models.Priority)) {
	if fn == nil {
		b.dispatchHook.Store(nil)
		return
	}
	b.dispatchHook.Store(&dispatchHook{fn: fn})
}

func (b *AdaptiveBatcher) QueueDepth() int64 { return b.qm.Depth() }

func (b *AdaptiveBatcher) Metrics() BatcherMetrics {
	batches := b.batchesFormed.Load()
	totalReq := b.totalRequestsInBatches.Load()
	avg := 0.0
	if batches > 0 {
		avg = float64(totalReq) / float64(batches)
	}
	return BatcherMetrics{
		QueueDepth:     b.qm.Depth(),
		BatchesFormed:  batches,
		RequestsQueued: b.requestsQueued.Load(),
		AvgBatchSize:   avg,
		ActiveStrategy: b.activeStrategyName(),
	}
}

func (b *AdaptiveBatcher) Submit(ctx context.Context, req *models.InferenceRequest) error {
	if req == nil {
		return models.ErrInvalidRequest
	}
	if b.stopping.Load() {
		return models.ErrShuttingDown
	}
	if ctx != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
	if err := b.qm.Submit(req); err != nil {
		return err
	}
	b.requestsQueued.Add(1)
	return nil
}

func (b *AdaptiveBatcher) Start(ctx context.Context) error {
	started := false
	b.startOnce.Do(func() {
		b.started.Store(true)
		started = true
	})
	if !started {
		return nil
	}
	b.run(ctx)
	return nil
}

func (b *AdaptiveBatcher) Stop(ctx context.Context) error {
	b.stopping.Store(true)
	b.stopOnce.Do(func() { close(b.stopCh) })
	if !b.started.Load() {
		// Never ran the dispatch loop; still have to fan shutdown out
		// to anything sitting in the QueueMatrix so handlers don't hang
		// waiting on their ResultChans.
		b.drain()
		return nil
	}
	select {
	case <-b.stoppedCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *AdaptiveBatcher) run(ctx context.Context) {
	defer close(b.stoppedCh)
	for {
		if b.shouldExit(ctx) {
			b.drain()
			return
		}
		batch, priority := b.formBatch(ctx)
		if batch == nil {
			continue
		}
		if !b.dispatch(ctx, batch, priority) {
			b.failBatch(batch)
			b.drain()
			return
		}
	}
}

func (b *AdaptiveBatcher) shouldExit(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	case <-b.stopCh:
		return true
	default:
		return false
	}
}

// formBatch waits on eventChan / timer / ctx, then drains the highest-
// priority non-empty sub-queue. The returned batch is homogeneous by
// construction and carries the priority reported by QueueMatrix.Drain.
func (b *AdaptiveBatcher) formBatch(ctx context.Context) (*models.Batch, models.Priority) {
	strategy := b.activeStrategy()
	timeout := b.nextTimeout(strategy)

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-b.qm.EventChan():
	case <-timer.C:
	case <-ctx.Done():
		return nil, 0
	}

	reqs, priority, _, _ := b.qm.Drain(b.cfg.MaxBatchSize)
	if len(reqs) == 0 {
		return nil, 0
	}
	name := ""
	if strategy != nil {
		name = strategy.Name()
	}
	return models.NewBatch(reqs, name), priority
}

func (b *AdaptiveBatcher) dispatch(ctx context.Context, batch *models.Batch, priority models.Priority) bool {
	idx := dispatchIndex(priority)
	ch := b.batchChans[idx]
	if ch == nil {
		// Intentional: tests may pass a zero-valued batchChans entry for
		// a priority they won't exercise. Treat as fatal dispatch failure.
		return false
	}
	start := time.Now()
	select {
	case ch <- batch:
		if time.Since(start) > dispatchBlockedThreshold {
			if hook := b.dispatchHook.Load(); hook != nil && hook.fn != nil {
				hook.fn(priority)
			}
		}
		b.batchesFormed.Add(1)
		b.totalRequestsInBatches.Add(int64(batch.Size()))
		return true
	case <-ctx.Done():
		return false
	case <-b.stopCh:
		return false
	}
}

func (b *AdaptiveBatcher) activeStrategy() strategies.Strategy {
	if sp := b.strategy.Load(); sp != nil {
		return *sp
	}
	return nil
}

func (b *AdaptiveBatcher) activeStrategyName() string {
	if s := b.activeStrategy(); s != nil {
		return s.Name()
	}
	return ""
}

func (b *AdaptiveBatcher) nextTimeout(strategy strategies.Strategy) time.Duration {
	if strategy == nil {
		return minFormBatchWait
	}
	timeout := strategy.CalculateTimeout(int(b.qm.Depth()), b.currentMetrics())
	if timeout < minFormBatchWait {
		return minFormBatchWait
	}
	return timeout
}

func (b *AdaptiveBatcher) currentMetrics() *strategies.StrategyMetrics {
	if mp := b.metrics.Load(); mp != nil {
		return mp
	}
	return nil
}

// drain fans ErrShuttingDown out to every request still sitting in the
// QueueMatrix and then closes it. All sends are non-blocking so workers
// never hang on handlers that have already returned.
func (b *AdaptiveBatcher) drain() {
	remaining := b.qm.DrainAll()
	for _, req := range remaining {
		b.notifyShuttingDown(req)
	}
	b.qm.Close()
}

func (b *AdaptiveBatcher) failBatch(batch *models.Batch) {
	if batch == nil {
		return
	}
	for _, req := range batch.Requests {
		b.notifyShuttingDown(req)
	}
}

func (b *AdaptiveBatcher) notifyShuttingDown(req *models.InferenceRequest) {
	if req == nil || req.ResultChan == nil {
		return
	}
	select {
	case req.ResultChan <- &models.RequestResult{RequestID: req.ID, Error: models.ErrShuttingDown}:
	default:
	}
}

func dispatchIndex(p models.Priority) int {
	idx := int(p)
	if idx < 0 || idx >= 4 {
		return int(models.PriorityNormal)
	}
	return idx
}
