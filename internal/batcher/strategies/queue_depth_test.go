package strategies

import (
	"testing"
	"time"

	"github.com/briankim06/adaptive-batching-engine/internal/models"
)

func TestQueueDepthStrategyCalculateTimeout(t *testing.T) {
	strategy := NewQueueDepthStrategy(10, 100, 5, 100, 8)

	tests := []struct {
		name       string
		queueDepth int
		expected   time.Duration
	}{
		{"empty queue", 0, 100 * time.Millisecond},
		{"below low threshold", 5, 100 * time.Millisecond},
		{"at low threshold", 10, 100 * time.Millisecond},
		{"at high threshold", 100, 5 * time.Millisecond},
		{"above high threshold", 150, 5 * time.Millisecond},
		{"midpoint", 55, time.Duration(52) * time.Millisecond},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			timeout := strategy.CalculateTimeout(tt.queueDepth, nil)
			if timeout != tt.expected {
				t.Errorf("expected timeout %v, got %v", tt.expected, timeout)
			}
		})
	}
}

func TestQueueDepthStrategyShouldFlushAtMaxBatchSize(t *testing.T) {
	strategy := NewQueueDepthStrategy(10, 100, 5, 100, 3)

	if strategy.ShouldFlush(make([]*models.InferenceRequest, 2), 0) {
		t.Error("expected ShouldFlush false below maxBatchSize")
	}
	if !strategy.ShouldFlush(make([]*models.InferenceRequest, 3), 0) {
		t.Error("expected ShouldFlush true at maxBatchSize")
	}
	if !strategy.ShouldFlush(make([]*models.InferenceRequest, 4), 0) {
		t.Error("expected ShouldFlush true above maxBatchSize")
	}
}
