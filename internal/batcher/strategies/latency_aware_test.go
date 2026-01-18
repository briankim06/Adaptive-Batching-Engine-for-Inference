package strategies

import (
	"testing"
	"time"

	"github.com/yourname/adaptive-batching-engine/internal/models"
)

type stubStrategy struct {
	timeout     time.Duration
	shouldFlush bool
	name        string
}

func (s *stubStrategy) Name() string {
	if s.name == "" {
		return "stub"
	}
	return s.name
}

func (s *stubStrategy) CalculateTimeout(int, *StrategyMetrics) time.Duration {
	return s.timeout
}

func (s *stubStrategy) ShouldFlush([]*models.InferenceRequest, int) bool {
	return s.shouldFlush
}

func TestLatencyAwareStrategyAdjustsTimeout(t *testing.T) {
	base := &stubStrategy{timeout: 100 * time.Millisecond}
	strategy := NewLatencyAwareStrategy(base)

	timeout := strategy.CalculateTimeout(0, &StrategyMetrics{P99LatencyMs: 120, TargetP99Ms: 100})
	expected := 80 * time.Millisecond
	if timeout != expected {
		t.Errorf("expected timeout %v, got %v", expected, timeout)
	}

	timeout = strategy.CalculateTimeout(0, &StrategyMetrics{P99LatencyMs: 70, TargetP99Ms: 100})
	expected = 120 * time.Millisecond
	if timeout != expected {
		t.Errorf("expected timeout %v, got %v", expected, timeout)
	}
}

func TestLatencyAwareStrategyNoMetricsUsesBase(t *testing.T) {
	base := &stubStrategy{timeout: 100 * time.Millisecond}
	strategy := NewLatencyAwareStrategy(base)

	timeout := strategy.CalculateTimeout(0, nil)
	if timeout != 100*time.Millisecond {
		t.Errorf("expected base timeout, got %v", timeout)
	}

	timeout = strategy.CalculateTimeout(0, &StrategyMetrics{P99LatencyMs: 120, TargetP99Ms: 0})
	if timeout != 100*time.Millisecond {
		t.Errorf("expected base timeout, got %v", timeout)
	}
}

func TestLatencyAwareStrategyClampsTimeout(t *testing.T) {
	base := &stubStrategy{timeout: 1 * time.Millisecond}
	strategy := NewLatencyAwareStrategy(base)

	timeout := strategy.CalculateTimeout(0, &StrategyMetrics{P99LatencyMs: 200, TargetP99Ms: 100})
	if timeout != 1*time.Millisecond {
		t.Errorf("expected min clamp to 1ms, got %v", timeout)
	}

	base.timeout = 5 * time.Second
	timeout = strategy.CalculateTimeout(0, &StrategyMetrics{P99LatencyMs: 10, TargetP99Ms: 100})
	if timeout != 5*time.Second {
		t.Errorf("expected max clamp to 5s, got %v", timeout)
	}
}

func TestLatencyAwareStrategyShouldFlushDelegates(t *testing.T) {
	base := &stubStrategy{timeout: 10 * time.Millisecond, shouldFlush: true}
	strategy := NewLatencyAwareStrategy(base)

	if !strategy.ShouldFlush(nil, 0) {
		t.Error("expected ShouldFlush to delegate to base strategy")
	}
}

func TestLatencyAwareStrategyNilBase(t *testing.T) {
	strategy := NewLatencyAwareStrategy(nil)

	if strategy.CalculateTimeout(0, nil) != 0 {
		t.Error("expected zero timeout when base strategy is nil")
	}
	if strategy.ShouldFlush(nil, 0) {
		t.Error("expected ShouldFlush false when base strategy is nil")
	}
}
