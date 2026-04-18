package metrics

import (
	"context"
	"math"
	"math/rand"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// LatencyTracker maintains a fixed-size reservoir of latency samples.
// Percentile values are materialised off the hot path by a background
// ticker and exposed via atomics, so the strategy layer pays O(1).
// See docs/spec/04-metrics.md §4.1 and invariant I7.
type LatencyTracker struct {
	samples []float64
	count   int64
	size    int
	mu      sync.RWMutex

	alpha  float64
	seeded atomic.Bool

	p50, p90, p99, p999 atomic.Uint64
	p99Smoothed          atomic.Uint64
}

// NewLatencyTracker allocates the reservoir. alpha is the EWMA smoothing
// factor (typically config.Metrics.P99SmoothingAlpha, default 0.3).
func NewLatencyTracker(windowSize int, alpha float64) *LatencyTracker {
	if windowSize < 1 {
		windowSize = 1
	}
	lt := &LatencyTracker{
		samples: make([]float64, 0, windowSize),
		size:    windowSize,
		alpha:   alpha,
	}
	lt.p50.Store(math.Float64bits(0))
	lt.p90.Store(math.Float64bits(0))
	lt.p99.Store(math.Float64bits(0))
	lt.p999.Store(math.Float64bits(0))
	lt.p99Smoothed.Store(math.Float64bits(0))
	return lt
}

// Add records a latency sample via reservoir sampling.
func (lt *LatencyTracker) Add(latencyMs float64) {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	lt.count++
	if len(lt.samples) < lt.size {
		lt.samples = append(lt.samples, latencyMs)
	} else {
		j := rand.Int63n(lt.count)
		if j < int64(lt.size) {
			lt.samples[j] = latencyMs
		}
	}
}

// Start launches the materialisation loop. Must be called exactly once.
func (lt *LatencyTracker) Start(ctx context.Context, interval time.Duration) {
	go lt.materializeLoop(ctx, interval)
}

func (lt *LatencyTracker) materializeLoop(ctx context.Context, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			lt.materialize()
		}
	}
}

func (lt *LatencyTracker) materialize() {
	lt.mu.RLock()
	snap := make([]float64, len(lt.samples))
	copy(snap, lt.samples)
	lt.mu.RUnlock()

	if len(snap) == 0 {
		return
	}
	sort.Float64s(snap)
	raw99 := pctile(snap, 99)
	lt.p50.Store(math.Float64bits(pctile(snap, 50)))
	lt.p90.Store(math.Float64bits(pctile(snap, 90)))
	lt.p99.Store(math.Float64bits(raw99))
	lt.p999.Store(math.Float64bits(pctile(snap, 99.9)))

	if !lt.seeded.Load() {
		lt.p99Smoothed.Store(math.Float64bits(raw99))
		lt.seeded.Store(true)
	} else {
		prev := math.Float64frombits(lt.p99Smoothed.Load())
		smoothed := lt.alpha*raw99 + (1-lt.alpha)*prev
		lt.p99Smoothed.Store(math.Float64bits(smoothed))
	}
}

func (lt *LatencyTracker) P50() float64         { return math.Float64frombits(lt.p50.Load()) }
func (lt *LatencyTracker) P90() float64         { return math.Float64frombits(lt.p90.Load()) }
func (lt *LatencyTracker) P99() float64         { return math.Float64frombits(lt.p99.Load()) }
func (lt *LatencyTracker) P999() float64        { return math.Float64frombits(lt.p999.Load()) }
func (lt *LatencyTracker) P99Smoothed() float64 { return math.Float64frombits(lt.p99Smoothed.Load()) }

// SampleCount returns the total number of samples that have been added
// (including those that were probabilistically discarded by the reservoir).
func (lt *LatencyTracker) SampleCount() int64 {
	lt.mu.RLock()
	defer lt.mu.RUnlock()
	return lt.count
}

// Reset clears samples, zeros all percentile atomics, and resets seeded
// so the next materialisation re-seeds the EWMA.
func (lt *LatencyTracker) Reset() {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	lt.samples = lt.samples[:0]
	lt.count = 0
	lt.p50.Store(math.Float64bits(0))
	lt.p90.Store(math.Float64bits(0))
	lt.p99.Store(math.Float64bits(0))
	lt.p999.Store(math.Float64bits(0))
	lt.p99Smoothed.Store(math.Float64bits(0))
	lt.seeded.Store(false)
}

// pctile returns the p-th percentile of a pre-sorted slice.
func pctile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)) * p / 100)
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
