package strategies

import (
	"time"

	"github.com/yourname/adaptive-batching-engine/internal/models"
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

func (s *FixedStrategy) ShouldFlush(batch []*models.InferenceRequest, _ int) bool {
	return len(batch) >= s.maxBatchSize
}
