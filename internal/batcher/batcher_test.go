package batcher

import (
	"context"
	"testing"
	"time"

	"github.com/yourname/adaptive-batching-engine/internal/batcher/strategies"
	"github.com/yourname/adaptive-batching-engine/internal/config"
	"github.com/yourname/adaptive-batching-engine/internal/models"
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

func (s *stubStrategy) ShouldFlush([]*models.InferenceRequest, int) bool {
	return s.shouldFlush
}

func TestNewBatcherConfig(t *testing.T) {
	input := config.BatchingConfig{
		Strategy:                "fixed",
		MinBatchSize:            1,
		MaxBatchSize:            32,
		MinWaitMs:               5,
		MaxWaitMs:               100,
		QueueDepthLowThreshold:  10,
		QueueDepthHighThreshold: 100,
		QueueCapacity:           1000,
		TokenBucketingEnabled:   true,
		PriorityEnabled:         false,
	}

	cfg := NewBatcherConfig(input)
	if cfg.Strategy != input.Strategy ||
		cfg.MinBatchSize != input.MinBatchSize ||
		cfg.MaxBatchSize != input.MaxBatchSize ||
		cfg.MinWaitMs != input.MinWaitMs ||
		cfg.MaxWaitMs != input.MaxWaitMs ||
		cfg.QueueDepthLowThreshold != input.QueueDepthLowThreshold ||
		cfg.QueueDepthHighThreshold != input.QueueDepthHighThreshold ||
		cfg.QueueCapacity != input.QueueCapacity ||
		cfg.TokenBucketingEnabled != input.TokenBucketingEnabled ||
		cfg.PriorityEnabled != input.PriorityEnabled {
		t.Error("batcher config did not map all fields correctly")
	}
}

func TestAdaptiveBatcherSubmitAndMetrics(t *testing.T) {
	cfg := BatcherConfig{
		MaxBatchSize:  2,
		MaxWaitMs:     50,
		QueueCapacity: 10,
	}
	batcher := NewAdaptiveBatcher(cfg, &stubStrategy{timeout: 10 * time.Millisecond}, make(chan *models.Batch, 1))

	if err := batcher.Submit(context.Background(), &models.InferenceRequest{Priority: models.PriorityNormal}); err != nil {
		t.Fatalf("unexpected submit error: %v", err)
	}

	if batcher.QueueDepth() != 1 {
		t.Errorf("expected queue depth 1, got %d", batcher.QueueDepth())
	}

	metrics := batcher.Metrics()
	if metrics.RequestsQueued != 1 {
		t.Errorf("expected requests queued 1, got %d", metrics.RequestsQueued)
	}
	if metrics.BatchesFormed != 0 {
		t.Errorf("expected batches formed 0, got %d", metrics.BatchesFormed)
	}
	if metrics.AvgBatchSize != 0 {
		t.Errorf("expected avg batch size 0, got %f", metrics.AvgBatchSize)
	}
}

func TestAdaptiveBatcherFormBatchMaxSize(t *testing.T) {
	cfg := BatcherConfig{
		MaxBatchSize:  2,
		MaxWaitMs:     100,
		QueueCapacity: 10,
	}
	strategy := &stubStrategy{timeout: time.Second}
	batcher := NewAdaptiveBatcher(cfg, strategy, make(chan *models.Batch, 1))

	batcher.priorityQueue.Submit(&models.InferenceRequest{Priority: models.PriorityNormal})
	batcher.priorityQueue.Submit(&models.InferenceRequest{Priority: models.PriorityNormal})

	batch := batcher.formBatch(context.Background())
	if batch == nil {
		t.Fatal("expected batch, got nil")
	}
	if batch.Size() != 2 {
		t.Errorf("expected batch size 2, got %d", batch.Size())
	}
	if batch.StrategyUsed != strategy.Name() {
		t.Errorf("expected strategy %s, got %s", strategy.Name(), batch.StrategyUsed)
	}
}

func TestAdaptiveBatcherCriticalFlush(t *testing.T) {
	cfg := BatcherConfig{
		MaxBatchSize:  5,
		MaxWaitMs:     100,
		QueueCapacity: 10,
	}
	batcher := NewAdaptiveBatcher(cfg, &stubStrategy{timeout: time.Second}, make(chan *models.Batch, 1))

	batcher.priorityQueue.Submit(&models.InferenceRequest{Priority: models.PriorityCritical})

	batch := batcher.formBatch(context.Background())
	if batch == nil {
		t.Fatal("expected batch, got nil")
	}
	if batch.Size() != 1 {
		t.Errorf("expected batch size 1, got %d", batch.Size())
	}
	if batch.Requests[0].Priority != models.PriorityCritical {
		t.Errorf("expected critical request, got %v", batch.Requests[0].Priority)
	}
}

func TestAdaptiveBatcherStartDispatchStop(t *testing.T) {
	cfg := BatcherConfig{
		MaxBatchSize:  1,
		MaxWaitMs:     50,
		QueueCapacity: 10,
	}
	dispatchChan := make(chan *models.Batch, 1)
	batcher := NewAdaptiveBatcher(cfg, &stubStrategy{timeout: 5 * time.Millisecond}, dispatchChan)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := batcher.Start(ctx); err != nil {
		t.Fatalf("unexpected start error: %v", err)
	}

	if err := batcher.Submit(context.Background(), &models.InferenceRequest{Priority: models.PriorityNormal}); err != nil {
		t.Fatalf("unexpected submit error: %v", err)
	}

	select {
	case batch := <-dispatchChan:
		if batch == nil || batch.Size() != 1 {
			t.Fatalf("expected batch size 1, got %v", batch)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timed out waiting for dispatch")
	}

	cancel()
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer stopCancel()

	if err := batcher.Stop(stopCtx); err != nil {
		t.Fatalf("unexpected stop error: %v", err)
	}
}

func TestAdaptiveBatcherDrainBatchOrder(t *testing.T) {
	cfg := BatcherConfig{
		MaxBatchSize:  2,
		MaxWaitMs:     100,
		QueueCapacity: 10,
	}
	batcher := NewAdaptiveBatcher(cfg, &stubStrategy{timeout: time.Second}, make(chan *models.Batch, 1))

	batcher.priorityQueue.Submit(&models.InferenceRequest{Priority: models.PriorityLow})
	batcher.priorityQueue.Submit(&models.InferenceRequest{Priority: models.PriorityCritical})

	batch := batcher.drainBatch()
	if batch == nil {
		t.Fatal("expected drained batch, got nil")
	}
	if batch.Size() != 2 {
		t.Fatalf("expected batch size 2, got %d", batch.Size())
	}
	if batch.Requests[0].Priority != models.PriorityCritical {
		t.Errorf("expected critical first, got %v", batch.Requests[0].Priority)
	}
}
