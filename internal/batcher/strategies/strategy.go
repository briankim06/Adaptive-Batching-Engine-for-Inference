package strategies

import (
	"time"

	"github.com/yourname/adaptive-batching-engine/internal/models"
)

type Strategy interface {
	Name() string
	CalculateTimeout(queueDepth int, metrics *StrategyMetrics) time.Duration
	ShouldFlush(batch []*models.InferenceRequest, queueDepth int) bool
}

type StrategyMetrics struct {
	P99LatencyMs float64
	TargetP99Ms  float64
}

type StrategyConfig struct {
	MinBatchSize            int
	MaxBatchSize            int
	MinWaitMs               int
	MaxWaitMs               int
	QueueDepthLowThreshold  int
	QueueDepthHighThreshold int
}
