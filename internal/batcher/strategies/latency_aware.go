package strategies

import (
	"time"

	"github.com/yourname/adaptive-batching-engine/internal/models"
)

const (
	minLatencyAwareTimeout = 1 * time.Millisecond
	maxLatencyAwareTimeout = 5 * time.Second
)

type LatencyAwareStrategy struct {
	base Strategy
}

func NewLatencyAwareStrategy(base Strategy) *LatencyAwareStrategy {
	return &LatencyAwareStrategy{
		base: base,
	}
}

func (s *LatencyAwareStrategy) Name() string {
	return "latency_aware"
}

func (s *LatencyAwareStrategy) CalculateTimeout(queueDepth int, metrics *StrategyMetrics) time.Duration {
	if s.base == nil {
		return 0
	}

	baseTimeout := s.base.CalculateTimeout(queueDepth, metrics)
	adjusted := baseTimeout

	if metrics != nil && metrics.TargetP99Ms > 0 {
		if metrics.P99LatencyMs > metrics.TargetP99Ms*1.1 {
			adjusted = time.Duration(float64(baseTimeout) * 0.8)
		} else if metrics.P99LatencyMs < metrics.TargetP99Ms*0.8 {
			adjusted = time.Duration(float64(baseTimeout) * 1.2)
		}
	}

	if adjusted < minLatencyAwareTimeout {
		return minLatencyAwareTimeout
	}
	if adjusted > maxLatencyAwareTimeout {
		return maxLatencyAwareTimeout
	}
	return adjusted
}

func (s *LatencyAwareStrategy) ShouldFlush(batch []*models.InferenceRequest, queueDepth int) bool {
	if s.base == nil {
		return false
	}
	return s.base.ShouldFlush(batch, queueDepth)
}
