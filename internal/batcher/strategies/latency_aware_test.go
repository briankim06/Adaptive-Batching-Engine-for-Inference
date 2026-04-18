package strategies

import (
	"math"
	"sync"
	"testing"
	"time"

	"github.com/briankim06/adaptive-batching-engine/internal/models"
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

func (s *stubStrategy) CalculateTimeout(int, *StrategyMetrics) time.Duration { return s.timeout }

func (s *stubStrategy) ShouldFlush([]*models.InferenceRequest, int) bool { return s.shouldFlush }

func TestLatencyAwareInitialMultiplierIsOne(t *testing.T) {
	base := &stubStrategy{timeout: 100 * time.Millisecond}
	strategy := NewLatencyAwareStrategy(base, 0.05, 100)

	timeout := strategy.CalculateTimeout(0, nil)
	if timeout != 100*time.Millisecond {
		t.Fatalf("expected base timeout with initial multiplier 1.0, got %v", timeout)
	}
	if strategy.Multiplier() != 1.0 {
		t.Fatalf("expected initial multiplier 1.0, got %v", strategy.Multiplier())
	}
}

func TestLatencyAwareProportionalStep(t *testing.T) {
	base := &stubStrategy{timeout: 100 * time.Millisecond}
	kP := 0.5 // large gain so a single step is easy to assert
	strategy := NewLatencyAwareStrategy(base, kP, 100)

	// Measured P99 is 20% above target → errRatio = 0.2.
	// newMult = 1.0 - 0.5*0.2 = 0.9 → timeout = 100ms*0.9 = 90ms.
	got := strategy.CalculateTimeout(0, &StrategyMetrics{P99LatencyMs: 120, TargetP99Ms: 100})
	want := 90 * time.Millisecond
	if got != want {
		t.Fatalf("expected %v after proportional step, got %v (mult=%v)", want, got, strategy.Multiplier())
	}
	if diff := math.Abs(strategy.Multiplier() - 0.9); diff > 1e-9 {
		t.Fatalf("expected multiplier 0.9, got %v", strategy.Multiplier())
	}
}

func TestLatencyAwareConvergesTowardsTarget(t *testing.T) {
	base := &stubStrategy{timeout: 100 * time.Millisecond}
	strategy := NewLatencyAwareStrategy(base, 0.05, 100)

	// Sustained over-target should move multiplier downwards each step.
	for i := 0; i < 20; i++ {
		_ = strategy.CalculateTimeout(0, &StrategyMetrics{P99LatencyMs: 120, TargetP99Ms: 100})
	}
	if strategy.Multiplier() >= 1.0 {
		t.Fatalf("expected multiplier to fall below 1.0 after sustained over-target, got %v", strategy.Multiplier())
	}
}

func TestLatencyAwareClampsMultiplier(t *testing.T) {
	base := &stubStrategy{timeout: 100 * time.Millisecond}
	strategy := NewLatencyAwareStrategy(base, 10.0, 100)

	// Huge kP + big error should trip the lower clamp at 0.5.
	_ = strategy.CalculateTimeout(0, &StrategyMetrics{P99LatencyMs: 200, TargetP99Ms: 100})
	if strategy.Multiplier() != 0.5 {
		t.Fatalf("expected lower clamp 0.5, got %v", strategy.Multiplier())
	}

	// Reset and exercise upper clamp.
	strategy = NewLatencyAwareStrategy(base, 10.0, 100)
	_ = strategy.CalculateTimeout(0, &StrategyMetrics{P99LatencyMs: 10, TargetP99Ms: 100})
	if strategy.Multiplier() != 2.0 {
		t.Fatalf("expected upper clamp 2.0, got %v", strategy.Multiplier())
	}
}

func TestLatencyAwareNoMetricsLeavesMultiplier(t *testing.T) {
	base := &stubStrategy{timeout: 100 * time.Millisecond}
	strategy := NewLatencyAwareStrategy(base, 0.05, 100)

	_ = strategy.CalculateTimeout(0, nil)
	_ = strategy.CalculateTimeout(0, &StrategyMetrics{P99LatencyMs: 0, TargetP99Ms: 100})
	_ = strategy.CalculateTimeout(0, &StrategyMetrics{P99LatencyMs: 120, TargetP99Ms: 0})

	if strategy.Multiplier() != 1.0 {
		t.Fatalf("expected multiplier untouched without both P99 and target, got %v", strategy.Multiplier())
	}
}

func TestLatencyAwareShouldFlushDelegates(t *testing.T) {
	base := &stubStrategy{shouldFlush: true}
	strategy := NewLatencyAwareStrategy(base, 0.05, 100)

	if !strategy.ShouldFlush(nil, 0) {
		t.Fatal("expected ShouldFlush to delegate to base strategy")
	}
}

func TestLatencyAwareNilBase(t *testing.T) {
	strategy := NewLatencyAwareStrategy(nil, 0.05, 100)

	if strategy.CalculateTimeout(0, nil) != 0 {
		t.Fatal("expected zero timeout when base strategy is nil")
	}
	if strategy.ShouldFlush(nil, 0) {
		t.Fatal("expected ShouldFlush false when base strategy is nil")
	}
}

// TestLatencyAwareConvergenceDownward exercises the spec'd downward
// convergence: with sustained P99 = 2× target, multiplier monotonically
// decreases and clamps at 0.5.
func TestLatencyAwareConvergenceDownward(t *testing.T) {
	base := &stubStrategy{timeout: 100 * time.Millisecond}
	strategy := NewLatencyAwareStrategy(base, 0.05, 50)

	prev := strategy.Multiplier()
	if prev != 1.0 {
		t.Fatalf("expected initial multiplier 1.0, got %v", prev)
	}

	monotonic := true
	var clampedAt int
	for i := 0; i < 500; i++ {
		_ = strategy.CalculateTimeout(0, &StrategyMetrics{P99LatencyMs: 100, TargetP99Ms: 50})
		curr := strategy.Multiplier()
		// Strictly decreasing until clamp; equality once clamped.
		if curr > prev+1e-12 {
			monotonic = false
			break
		}
		if curr == 0.5 && clampedAt == 0 {
			clampedAt = i + 1
		}
		prev = curr
	}
	if !monotonic {
		t.Fatalf("multiplier was not monotonically non-increasing under sustained over-target")
	}
	if got := strategy.Multiplier(); got != 0.5 {
		t.Fatalf("expected multiplier clamped at 0.5, got %v", got)
	}

	// Returned timeout approaches base * 0.5 = 50ms.
	timeout := strategy.CalculateTimeout(0, &StrategyMetrics{P99LatencyMs: 100, TargetP99Ms: 50})
	if timeout != 50*time.Millisecond {
		t.Fatalf("expected timeout 50ms after lower clamp, got %v", timeout)
	}
}

// TestLatencyAwareConvergenceUpward exercises the upward branch: P99 =
// 0.5× target → multiplier rises and clamps at 2.0.
func TestLatencyAwareConvergenceUpward(t *testing.T) {
	base := &stubStrategy{timeout: 100 * time.Millisecond}
	strategy := NewLatencyAwareStrategy(base, 0.05, 50)

	prev := strategy.Multiplier()
	monotonic := true
	for i := 0; i < 500; i++ {
		_ = strategy.CalculateTimeout(0, &StrategyMetrics{P99LatencyMs: 25, TargetP99Ms: 50})
		curr := strategy.Multiplier()
		if curr < prev-1e-12 {
			monotonic = false
			break
		}
		prev = curr
	}
	if !monotonic {
		t.Fatalf("multiplier was not monotonically non-decreasing under sustained under-target")
	}
	if got := strategy.Multiplier(); got != 2.0 {
		t.Fatalf("expected multiplier clamped at 2.0, got %v", got)
	}
	timeout := strategy.CalculateTimeout(0, &StrategyMetrics{P99LatencyMs: 25, TargetP99Ms: 50})
	if timeout != 200*time.Millisecond {
		t.Fatalf("expected timeout 200ms after upper clamp, got %v", timeout)
	}
}

// TestLatencyAwareStability: when measured P99 == target, errRatio is 0
// so the multiplier stays glued to its starting value (1.0).
func TestLatencyAwareStability(t *testing.T) {
	base := &stubStrategy{timeout: 100 * time.Millisecond}
	strategy := NewLatencyAwareStrategy(base, 0.05, 50)

	for i := 0; i < 200; i++ {
		_ = strategy.CalculateTimeout(0, &StrategyMetrics{P99LatencyMs: 50, TargetP99Ms: 50})
	}
	if diff := math.Abs(strategy.Multiplier() - 1.0); diff > 1e-9 {
		t.Fatalf("expected multiplier ~1.0 under at-target conditions, got %v (diff=%v)", strategy.Multiplier(), diff)
	}
}

func TestLatencyAwareConcurrentCalculateTimeout(t *testing.T) {
	base := &stubStrategy{timeout: 100 * time.Millisecond}
	strategy := NewLatencyAwareStrategy(base, 0.01, 100)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = strategy.CalculateTimeout(0, &StrategyMetrics{P99LatencyMs: 120, TargetP99Ms: 100})
			}
		}()
	}
	wg.Wait()

	mult := strategy.Multiplier()
	if mult < 0.5 || mult > 2.0 {
		t.Fatalf("multiplier escaped clamp under concurrency: %v", mult)
	}
}
