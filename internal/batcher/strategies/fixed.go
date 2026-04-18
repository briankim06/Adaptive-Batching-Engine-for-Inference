package strategies

import (
	"time"

	"github.com/briankim06/adaptive-batching-engine/internal/models"
)

type FixedStrategy struct {
	maxWaitMs    int
	maxBatchSize int
}

func NewFixedStrategy(maxWaitMs, maxBatchSize int) *FixedStrategy {
	return &FixedStrategy{
		maxWaitMs:    maxWaitMs,
		maxBatchSize: maxBatchSize,
	}
}

func (s *FixedStrategy) Name() string {
	return "fixed"
}

func (s *FixedStrategy) CalculateTimeout(_ int, _ *StrategyMetrics) time.Duration {
	return time.Duration(s.maxWaitMs) * time.Millisecond
}

// ShouldFlush returns true when the batch is full OR matrix-wide pressure
// reaches maxBatchSize — at that point sibling sub-queues have enough
// aggregate work that holding the formation loop for this partial batch
// would starve them. queueDepth is the QueueMatrix-wide total.
func (s *FixedStrategy) ShouldFlush(batch []*models.InferenceRequest, queueDepth int) bool {
	return len(batch) >= s.maxBatchSize || queueDepth >= s.maxBatchSize
}
