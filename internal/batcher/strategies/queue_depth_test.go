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

func TestQueueDepthStrategyShouldFlush(t *testing.T) {
	const maxBatch = 3
	strategy := NewQueueDepthStrategy(10, 100, 5, 100, maxBatch)

	tests := []struct {
		name       string
		batchSize  int
		queueDepth int
		want       bool
	}{
		{"isolated partial batch holds window", 2, 0, false},
		{"isolated partial batch ignores sub-threshold depth", 2, maxBatch - 1, false},
		{"aggregate pressure yields partial batch", 2, maxBatch, true},
		{"full batch flushes with no depth", maxBatch, 0, true},
		{"full batch flushes with high depth", maxBatch, 1000, true},
		{"over-full batch flushes", maxBatch + 1, 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := strategy.ShouldFlush(make([]*models.InferenceRequest, tt.batchSize), tt.queueDepth)
			if got != tt.want {
				t.Errorf("ShouldFlush(batch=%d, depth=%d) = %v, want %v",
					tt.batchSize, tt.queueDepth, got, tt.want)
			}
		})
	}
}
