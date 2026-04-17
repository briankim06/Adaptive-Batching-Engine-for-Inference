package strategies

import (
	"math"
	"sync/atomic"
	"time"

	"github.com/briankim06/adaptive-batching-engine/internal/models"
)

// LatencyAwareStrategy decorates a base strategy with a proportional
// controller over observed smoothed P99 latency. See
// docs/spec/02-batching.md §2.4 and invariant I7 for rationale.
type LatencyAwareStrategy struct {
	baseStrategy      Strategy
	currentMultiplier atomic.Uint64 // math.Float64bits of the live multiplier
	kP                float64
	targetP99Ms       float64
}

// NewLatencyAwareStrategy wraps base with a proportional controller.
// kP is the proportional gain (spec recommends 0.05) and targetP99Ms is
// the control-loop setpoint — it is recorded for introspection; the live
// controller reads TargetP99Ms from StrategyMetrics so the runtime can
// update it without restarting the strategy.
func NewLatencyAwareStrategy(base Strategy, kP, targetP99Ms float64) *LatencyAwareStrategy {
	s := &LatencyAwareStrategy{
		baseStrategy: base,
		kP:           kP,
		targetP99Ms:  targetP99Ms,
	}
	s.currentMultiplier.Store(math.Float64bits(1.0))
	return s
}

func (s *LatencyAwareStrategy) Name() string { return "latency_aware" }

// CalculateTimeout applies one proportional step then returns base*mult.
// The atomic multiplier means this is safe to call from concurrent
// goroutines without a mutex.
func (s *LatencyAwareStrategy) CalculateTimeout(queueDepth int, metrics *StrategyMetrics) time.Duration {
	mult := math.Float64frombits(s.currentMultiplier.Load())
	if metrics != nil && metrics.P99LatencyMs > 0 && metrics.TargetP99Ms > 0 {
		errRatio := (metrics.P99LatencyMs - metrics.TargetP99Ms) / metrics.TargetP99Ms
		mult -= s.kP * errRatio
		mult = clampMultiplier(mult, 0.5, 2.0)
		s.currentMultiplier.Store(math.Float64bits(mult))
	}
	if s.baseStrategy == nil {
		return 0
	}
	base := s.baseStrategy.CalculateTimeout(queueDepth, metrics)
	return time.Duration(float64(base) * mult)
}

func (s *LatencyAwareStrategy) ShouldFlush(batch []*models.InferenceRequest, queueDepth int) bool {
	if s.baseStrategy == nil {
		return false
	}
	return s.baseStrategy.ShouldFlush(batch, queueDepth)
}

// Multiplier exposes the current controller state for tests and metrics.
func (s *LatencyAwareStrategy) Multiplier() float64 {
	return math.Float64frombits(s.currentMultiplier.Load())
}

func clampMultiplier(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
