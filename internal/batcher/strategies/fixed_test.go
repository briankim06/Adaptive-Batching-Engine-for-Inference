package strategies

import (
	"testing"
	"time"

	"github.com/yourname/adaptive-batching-engine/internal/models"
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
	strategy := NewFixedStrategy(25, 3)

	if strategy.ShouldFlush(make([]*models.InferenceRequest, 2), 0) {
		t.Error("expected ShouldFlush false for batch size 2")
	}
	if !strategy.ShouldFlush(make([]*models.InferenceRequest, 3), 0) {
		t.Error("expected ShouldFlush true for batch size 3")
	}
}
