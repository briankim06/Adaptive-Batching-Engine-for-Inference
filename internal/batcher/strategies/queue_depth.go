package strategies

import (
	"time"

	"github.com/yourname/adaptive-batching-engine/internal/models"
)

type QueueDepthStrategy struct {
	lowThreshold  int
	highThreshold int
	minWaitMs     int
	maxWaitMs     int
}

func NewQueueDepthStrategy(lowThreshold, highThreshold, minWaitMs, maxWaitMs int) *QueueDepthStrategy {
	return &QueueDepthStrategy{
		lowThreshold:  lowThreshold,
		highThreshold: highThreshold,
		minWaitMs:     minWaitMs,
		maxWaitMs:     maxWaitMs,
	}
}

func (s *QueueDepthStrategy) Name() string {
	return "queue_depth"
}

func (s *QueueDepthStrategy) CalculateTimeout(queueDepth int, _ *StrategyMetrics) time.Duration {
	if queueDepth <= 0 {
		return time.Duration(s.maxWaitMs) * time.Millisecond
	}
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

func (s *QueueDepthStrategy) ShouldFlush(_ []*models.InferenceRequest, _ int) bool {
	return false
}
