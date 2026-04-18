package strategies

import (
	"testing"
	"time"

	"github.com/briankim06/adaptive-batching-engine/internal/models"
)

func TestFixedStrategyCalculateTimeout(t *testing.T) {
	strategy := NewFixedStrategy(25, 3)

	timeout := strategy.CalculateTimeout(0, nil)
	expected := 25 * time.Millisecond
	if timeout != expected {
		t.Errorf("expected timeout %v, got %v", expected, timeout)
	}
}

func TestFixedStrategyShouldFlush(t *testing.T) {
	const maxBatch = 3
	strategy := NewFixedStrategy(25, maxBatch)

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
