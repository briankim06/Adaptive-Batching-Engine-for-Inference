package batcher

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yourname/adaptive-batching-engine/internal/batcher/packing"
	"github.com/yourname/adaptive-batching-engine/internal/batcher/strategies"
	"github.com/yourname/adaptive-batching-engine/internal/models"
)

type AdaptiveBatcher struct {
	cfg           BatcherConfig
	strategy      strategies.Strategy
	priorityQueue *packing.PriorityQueue
	dispatchChan  chan *models.Batch

	strategyMu sync.RWMutex
	startOnce  sync.Once
	stopOnce   sync.Once
	stopCh     chan struct{}
	stoppedCh  chan struct{}
	started    int32
	stopping   int32

	batchesFormed  int64
	requestsQueued int64
	totalBatchSize int64
}

func NewAdaptiveBatcher(cfg BatcherConfig, strategy strategies.Strategy, dispatchChan chan *models.Batch) *AdaptiveBatcher {
	if cfg.QueueCapacity <= 0 {
		cfg.QueueCapacity = 1
	}
	if cfg.MaxBatchSize <= 0 {
		cfg.MaxBatchSize = 1
	}
	return &AdaptiveBatcher{
		cfg:           cfg,
		strategy:      strategy,
		priorityQueue: packing.NewPriorityQueue(cfg.QueueCapacity),
		dispatchChan:  dispatchChan,
		stopCh:        make(chan struct{}),
		stoppedCh:     make(chan struct{}),
	}
}

func (b *AdaptiveBatcher) Submit(ctx context.Context, req *models.InferenceRequest) error {
	if req == nil {
		return models.ErrInvalidRequest
	}
	if atomic.LoadInt32(&b.stopping) == 1 {
		return models.ErrShuttingDown
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	b.priorityQueue.Submit(req)
	atomic.AddInt64(&b.requestsQueued, 1)
	return nil
}

func (b *AdaptiveBatcher) Start(ctx context.Context) error {
	b.startOnce.Do(func() {
		atomic.StoreInt32(&b.started, 1)
		go b.run(ctx)
	})
	return nil
}

func (b *AdaptiveBatcher) Stop(ctx context.Context) error {
	atomic.StoreInt32(&b.stopping, 1)
	b.stopOnce.Do(func() {
		close(b.stopCh)
	})

	if atomic.LoadInt32(&b.started) == 0 {
		return nil
	}

	select {
	case <-b.stoppedCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *AdaptiveBatcher) QueueDepth() int {
	return b.priorityQueue.Depth()
}

func (b *AdaptiveBatcher) Metrics() BatcherMetrics {
	batchesFormed := atomic.LoadInt64(&b.batchesFormed)
	totalBatchSize := atomic.LoadInt64(&b.totalBatchSize)
	avgBatchSize := 0.0
	if batchesFormed > 0 {
		avgBatchSize = float64(totalBatchSize) / float64(batchesFormed)
	}
	return BatcherMetrics{
		QueueDepth:     b.QueueDepth(),
		BatchesFormed:  batchesFormed,
		RequestsQueued: atomic.LoadInt64(&b.requestsQueued),
		AvgBatchSize:   avgBatchSize,
	}
}

func (b *AdaptiveBatcher) SetStrategy(strategy strategies.Strategy) {
	if strategy == nil {
		return
	}
	b.strategyMu.Lock()
	b.strategy = strategy
	b.strategyMu.Unlock()
}

func (b *AdaptiveBatcher) run(ctx context.Context) {
	defer close(b.stoppedCh)
	for {
		select {
		case <-ctx.Done():
			b.drain(ctx)
			return
		case <-b.stopCh:
			b.drain(ctx)
			return
		default:
		}

		batch := b.formBatch(ctx)
		if batch == nil {
			continue
		}
		if !b.dispatch(ctx, batch) {
			return
		}
	}
}

func (b *AdaptiveBatcher) dispatch(ctx context.Context, batch *models.Batch) bool {
	select {
	case b.dispatchChan <- batch:
		atomic.AddInt64(&b.batchesFormed, 1)
		atomic.AddInt64(&b.totalBatchSize, int64(batch.Size()))
		return true
	case <-ctx.Done():
		return false
	case <-b.stopCh:
		return false
	}
}

func (b *AdaptiveBatcher) formBatch(ctx context.Context) *models.Batch {
	timeout := b.calculateTimeout()
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	var requests []*models.InferenceRequest
	criticalCh := b.priorityQueue.Critical()
	highCh := b.priorityQueue.High()
	normalCh := b.priorityQueue.Normal()
	lowCh := b.priorityQueue.Low()

	for {
		select {
		case req, ok := <-criticalCh:
			if !ok {
				criticalCh = nil
				continue
			}
			if req != nil {
				requests = append(requests, req)
			}
			if len(requests) > 0 {
				return models.NewBatch(requests, b.strategyName())
			}
		case req, ok := <-highCh:
			if !ok {
				highCh = nil
				continue
			}
			if req != nil {
				requests = append(requests, req)
			}
		case req, ok := <-normalCh:
			if !ok {
				normalCh = nil
				continue
			}
			if req != nil {
				requests = append(requests, req)
			}
		case req, ok := <-lowCh:
			if !ok {
				lowCh = nil
				continue
			}
			if req != nil {
				requests = append(requests, req)
			}
		case <-timer.C:
			if len(requests) > 0 {
				return models.NewBatch(requests, b.strategyName())
			}
			return nil
		case <-ctx.Done():
			return nil
		case <-b.stopCh:
			if len(requests) > 0 {
				return models.NewBatch(requests, b.strategyName())
			}
			return nil
		}

		if b.shouldFlush(requests) {
			return models.NewBatch(requests, b.strategyName())
		}

		if len(requests) >= b.cfg.MaxBatchSize {
			return models.NewBatch(requests, b.strategyName())
		}
	}
}

func (b *AdaptiveBatcher) shouldFlush(requests []*models.InferenceRequest) bool {
	if len(requests) == 0 {
		return false
	}
	strategy := b.getStrategy()
	if strategy == nil {
		return false
	}
	return strategy.ShouldFlush(requests, b.QueueDepth())
}

func (b *AdaptiveBatcher) calculateTimeout() time.Duration {
	strategy := b.getStrategy()
	if strategy == nil {
		return time.Duration(b.cfg.MaxWaitMs) * time.Millisecond
	}
	timeout := strategy.CalculateTimeout(b.QueueDepth(), b.strategyMetrics())
	if timeout <= 0 {
		return time.Duration(b.cfg.MaxWaitMs) * time.Millisecond
	}
	return timeout
}

func (b *AdaptiveBatcher) strategyMetrics() *strategies.StrategyMetrics {
	return &strategies.StrategyMetrics{}
}

func (b *AdaptiveBatcher) strategyName() string {
	strategy := b.getStrategy()
	if strategy == nil {
		return ""
	}
	return strategy.Name()
}

func (b *AdaptiveBatcher) getStrategy() strategies.Strategy {
	b.strategyMu.RLock()
	defer b.strategyMu.RUnlock()
	return b.strategy
}

func (b *AdaptiveBatcher) drain(ctx context.Context) {
	for {
		batch := b.drainBatch()
		if batch == nil {
			return
		}
		if !b.dispatch(ctx, batch) {
			return
		}
	}
}

func (b *AdaptiveBatcher) drainBatch() *models.Batch {
	var requests []*models.InferenceRequest
	for len(requests) < b.cfg.MaxBatchSize {
		req, ok := b.priorityQueue.TryReceive()
		if !ok {
			break
		}
		if req != nil {
			requests = append(requests, req)
		}
	}
	if len(requests) == 0 {
		return nil
	}
	return models.NewBatch(requests, b.strategyName())
}
