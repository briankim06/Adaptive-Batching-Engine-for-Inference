package strategies

import (
	"time"

	"github.com/briankim06/adaptive-batching-engine/internal/models"
)

// QueueDepthStrategy scales the formBatch wait linearly with observed
// queue depth: empty queue ⇒ favor throughput (wait long), full queue ⇒
// favor latency (flush fast).
type QueueDepthStrategy struct {
	lowThreshold  int
	highThreshold int
	minWaitMs     int
	maxWaitMs     int
	maxBatchSize  int
}

// NewQueueDepthStrategy constructs the strategy. All five parameters are
// required; see docs/spec/02-batching.md §2.3.
func NewQueueDepthStrategy(lowThreshold, highThreshold, minWaitMs, maxWaitMs, maxBatchSize int) *QueueDepthStrategy {
	return &QueueDepthStrategy{
		lowThreshold:  lowThreshold,
		highThreshold: highThreshold,
		minWaitMs:     minWaitMs,
		maxWaitMs:     maxWaitMs,
		maxBatchSize:  maxBatchSize,
	}
}

func (s *QueueDepthStrategy) Name() string { return "queue_depth" }

func (s *QueueDepthStrategy) CalculateTimeout(queueDepth int, _ *StrategyMetrics) time.Duration {
	if queueDepth <= s.lowThreshold {
		return time.Duration(s.maxWaitMs) * time.Millisecond
	}
	if queueDepth >= s.highThreshold {
		return time.Duration(s.minWaitMs) * time.Millisecond
	}
	if s.highThreshold <= s.lowThreshold {
		return time.Duration(s.minWaitMs) * time.Millisecond
	}

	ratio := float64(queueDepth-s.lowThreshold) / float64(s.highThreshold-s.lowThreshold)
	waitMs := float64(s.maxWaitMs) - ratio*float64(s.maxWaitMs-s.minWaitMs)
	if waitMs < 0 {
		waitMs = 0
	}
	return time.Duration(waitMs) * time.Millisecond
}

func (s *QueueDepthStrategy) ShouldFlush(batch []*models.InferenceRequest, _ int) bool {
	return len(batch) >= s.maxBatchSize
}
