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
