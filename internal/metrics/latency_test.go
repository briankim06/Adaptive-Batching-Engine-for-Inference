package metrics

import (
	"context"
	"math"
	"sync"
	"testing"
	"time"
)

func TestLatencyTrackerInitialState(t *testing.T) {
	lt := NewLatencyTracker(100, 0.3)
	if lt.P50() != 0 || lt.P90() != 0 || lt.P99() != 0 || lt.P999() != 0 {
		t.Fatal("expected all percentiles to be 0 initially")
	}
	if lt.P99Smoothed() != 0 {
		t.Fatal("expected P99Smoothed to be 0 initially")
	}
	if lt.SampleCount() != 0 {
		t.Fatal("expected sample count 0 initially")
	}
}

func TestLatencyTrackerAddAndMaterialize(t *testing.T) {
	lt := NewLatencyTracker(1000, 0.3)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for i := 1; i <= 100; i++ {
		lt.Add(float64(i))
	}

	lt.Start(ctx, 10*time.Millisecond)
	time.Sleep(50 * time.Millisecond)

	p50 := lt.P50()
	p99 := lt.P99()
	if p50 < 40 || p50 > 60 {
		t.Fatalf("expected P50 ~50, got %v", p50)
	}
	if p99 < 90 {
		t.Fatalf("expected P99 >= 90, got %v", p99)
	}
}

func TestLatencyTrackerEWMASeeding(t *testing.T) {
	lt := NewLatencyTracker(100, 0.3)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for i := 0; i < 50; i++ {
		lt.Add(100.0)
	}

	lt.Start(ctx, 10*time.Millisecond)
	time.Sleep(50 * time.Millisecond)

	// First materialisation seeds P99Smoothed to the raw P99.
	smoothed := lt.P99Smoothed()
	if smoothed < 90 || smoothed > 110 {
		t.Fatalf("expected P99Smoothed ~100 after seeding, got %v", smoothed)
	}
}

func TestLatencyTrackerEWMAConverges(t *testing.T) {
	lt := NewLatencyTracker(100, 0.5) // high alpha = fast convergence
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Seed with 100ms latencies.
	for i := 0; i < 50; i++ {
		lt.Add(100.0)
	}
	lt.Start(ctx, 10*time.Millisecond)
	time.Sleep(30 * time.Millisecond)

	// Now add much higher latencies.
	for i := 0; i < 100; i++ {
		lt.Add(200.0)
	}
	time.Sleep(80 * time.Millisecond)

	smoothed := lt.P99Smoothed()
	if smoothed < 150 {
		t.Fatalf("expected P99Smoothed to converge toward 200, got %v", smoothed)
	}
}

func TestLatencyTrackerReset(t *testing.T) {
	lt := NewLatencyTracker(100, 0.3)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for i := 0; i < 50; i++ {
		lt.Add(100.0)
	}
	lt.Start(ctx, 10*time.Millisecond)
	time.Sleep(50 * time.Millisecond)

	if lt.P99() == 0 {
		t.Fatal("expected non-zero P99 before reset")
	}

	lt.Reset()

	if lt.P99() != 0 {
		t.Fatalf("expected P99 to be 0 after reset, got %v", lt.P99())
	}
	if lt.P99Smoothed() != 0 {
		t.Fatalf("expected P99Smoothed to be 0 after reset, got %v", lt.P99Smoothed())
	}
	if lt.SampleCount() != 0 {
		t.Fatalf("expected sample count 0 after reset, got %d", lt.SampleCount())
	}
}

func TestLatencyTrackerReservoirSampling(t *testing.T) {
	lt := NewLatencyTracker(10, 0.3) // small reservoir
	for i := 0; i < 1000; i++ {
		lt.Add(float64(i))
	}
	if lt.SampleCount() != 1000 {
		t.Fatalf("expected count 1000, got %d", lt.SampleCount())
	}
	// The reservoir should be full at exactly size=10.
	lt.mu.RLock()
	n := len(lt.samples)
	lt.mu.RUnlock()
	if n != 10 {
		t.Fatalf("expected reservoir size 10, got %d", n)
	}
}

func TestLatencyTrackerConcurrency(t *testing.T) {
	lt := NewLatencyTracker(100, 0.3)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lt.Start(ctx, 5*time.Millisecond)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				lt.Add(float64(j))
				_ = lt.P99()
				_ = lt.P99Smoothed()
			}
		}()
	}
	wg.Wait()
}

// TestEWMARejectsSpike guards I7: a single-cycle p99 spike must not
// propagate to the smoothed series. With alpha = 0.3 and seed 50ms, a
// raw p99 of 500ms lands at 50 + 0.3*(500-50) = 185ms. After the spike
// subsides back to 50ms, the smoothed value decays back toward 50.
func TestEWMARejectsSpike(t *testing.T) {
	lt := NewLatencyTracker(100, 0.3)

	// Seed the smoothed series at 50ms.
	lt.mu.Lock()
	for i := 0; i < 100; i++ {
		lt.samples = append(lt.samples, 50.0)
	}
	lt.count = 100
	lt.mu.Unlock()

	lt.materialize()
	if seeded := lt.P99Smoothed(); math.Abs(seeded-50) > 0.001 {
		t.Fatalf("expected seeded P99Smoothed = 50, got %v", seeded)
	}

	// Inject a single cycle with raw p99 = 500ms.
	lt.mu.Lock()
	for i := range lt.samples {
		lt.samples[i] = 500.0
	}
	lt.mu.Unlock()

	lt.materialize()
	afterSpike := lt.P99Smoothed()
	want := 50 + 0.3*(500-50) // 185
	if math.Abs(afterSpike-want) > 0.001 {
		t.Fatalf("expected P99Smoothed = %v after one spike, got %v", want, afterSpike)
	}
	if lt.P99() < 499 {
		t.Fatalf("expected raw P99 to reflect spike (~500), got %v", lt.P99())
	}

	// Spike subsides: raw p99 back at 50. Smoothed decays toward 50.
	lt.mu.Lock()
	for i := range lt.samples {
		lt.samples[i] = 50.0
	}
	lt.mu.Unlock()

	lt.materialize()
	afterDecay := lt.P99Smoothed()
	wantDecay := 0.3*50 + 0.7*afterSpike // = 15 + 129.5 = 144.5
	if math.Abs(afterDecay-wantDecay) > 0.001 {
		t.Fatalf("expected decayed P99Smoothed = %v, got %v", wantDecay, afterDecay)
	}
	if afterDecay >= afterSpike {
		t.Fatalf("expected decay toward 50 (%v < %v)", afterDecay, afterSpike)
	}
}

// TestP99vsP99Smoothed guards I7: raw P99 tracks the reservoir exactly,
// while P99Smoothed shows strictly less variance under a sawtooth input.
func TestP99vsP99Smoothed(t *testing.T) {
	lt := NewLatencyTracker(100, 0.3)

	sawtooth := []float64{50, 150, 50, 150, 50, 150, 50, 150, 50, 150, 50, 150}

	var rawSeries, smoothedSeries []float64
	for _, v := range sawtooth {
		lt.mu.Lock()
		lt.samples = lt.samples[:0]
		for i := 0; i < 100; i++ {
			lt.samples = append(lt.samples, v)
		}
		lt.count = 100
		lt.mu.Unlock()

		lt.materialize()
		rawSeries = append(rawSeries, lt.P99())
		smoothedSeries = append(smoothedSeries, lt.P99Smoothed())
	}

	// Raw P99 must reflect the sawtooth exactly.
	for i, want := range sawtooth {
		if math.Abs(rawSeries[i]-want) > 0.001 {
			t.Fatalf("raw P99[%d] = %v, want %v", i, rawSeries[i], want)
		}
	}

	// Compare variance: skip the first sample (seeding), which aliases raw.
	rawVar := variance(rawSeries[1:])
	smoothedVar := variance(smoothedSeries[1:])
	if smoothedVar >= rawVar {
		t.Fatalf("expected smoothed variance (%v) < raw variance (%v)", smoothedVar, rawVar)
	}
}

func variance(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	mean := sum / float64(len(xs))
	var ss float64
	for _, x := range xs {
		d := x - mean
		ss += d * d
	}
	return ss / float64(len(xs))
}

func TestPctile(t *testing.T) {
	tests := []struct {
		name     string
		sorted   []float64
		p        float64
		expected float64
	}{
		{"empty", nil, 99, 0},
		{"single", []float64{42}, 50, 42},
		{"p50 of 10", []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 50, 6},
		{"p99 of 100", func() []float64 {
			s := make([]float64, 100)
			for i := range s {
				s[i] = float64(i + 1)
			}
			return s
		}(), 99, 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := pctile(tt.sorted, tt.p)
			if math.Abs(got-tt.expected) > 1 {
				t.Fatalf("expected ~%v, got %v", tt.expected, got)
			}
		})
	}
}
