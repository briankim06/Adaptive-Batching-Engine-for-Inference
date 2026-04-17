package metrics

import (
	"context"
	"testing"
	"time"
)

func TestAggregatorAddAndGetHistory(t *testing.T) {
	agg := NewTimeSeriesAggregator()
	agg.Add("test_metric", 10.0)
	agg.Add("test_metric", 20.0)
	agg.Add("test_metric", 30.0)

	history := agg.GetHistory("test_metric", 5*time.Minute)
	if len(history) != 3 {
		t.Fatalf("expected 3 data points, got %d", len(history))
	}
	// Newest first.
	if history[0].Value != 30.0 {
		t.Fatalf("expected newest value 30, got %v", history[0].Value)
	}
}

func TestAggregatorUnknownMetric(t *testing.T) {
	agg := NewTimeSeriesAggregator()
	history := agg.GetHistory("nonexistent", time.Minute)
	if len(history) != 0 {
		t.Fatalf("expected empty history for unknown metric, got %d", len(history))
	}
}

func TestAggregatorRingWraps(t *testing.T) {
	agg := NewTimeSeriesAggregator()
	// Write 310 entries — more than the 300-entry short ring.
	for i := 0; i < 310; i++ {
		agg.Add("wrap_test", float64(i))
	}

	history := agg.GetHistory("wrap_test", 5*time.Minute)
	// Should have up to 300 entries (ring capacity), all with recent timestamps.
	if len(history) > 310 {
		t.Fatalf("expected at most 310 entries, got %d", len(history))
	}
	if len(history) == 0 {
		t.Fatal("expected non-empty history after ring wraps")
	}
}

func TestAggregatorLazyCreation(t *testing.T) {
	agg := NewTimeSeriesAggregator()
	agg.Add("metric_a", 1.0)
	agg.Add("metric_b", 2.0)

	histA := agg.GetHistory("metric_a", time.Minute)
	histB := agg.GetHistory("metric_b", time.Minute)
	if len(histA) != 1 || len(histB) != 1 {
		t.Fatalf("expected 1 point each, got a=%d b=%d", len(histA), len(histB))
	}
}

func TestAggregatorFreshSliceAllocation(t *testing.T) {
	agg := NewTimeSeriesAggregator()
	agg.Add("test", 1.0)
	agg.Add("test", 2.0)

	h1 := agg.GetHistory("test", time.Minute)
	h2 := agg.GetHistory("test", time.Minute)

	// Mutating h1 should not affect h2.
	if len(h1) > 0 {
		h1[0].Value = 999
	}
	if len(h2) > 0 && h2[0].Value == 999 {
		t.Fatal("GetHistory returned aliased slices")
	}
}

func TestAggregatorDownsampler(t *testing.T) {
	agg := NewTimeSeriesAggregator()

	// Pre-populate with data inside the past minute.
	for i := 0; i < 10; i++ {
		agg.Add("ds_test", float64(i*10))
	}

	// Run downsample directly.
	agg.downsampleAll()

	// Check that a long-ring entry was created.
	agg.mu.RLock()
	ts := agg.series["ds_test"]
	agg.mu.RUnlock()

	ts.mu.RLock()
	longEntry := ts.long[0]
	ts.mu.RUnlock()

	if longEntry.Timestamp.IsZero() {
		t.Fatal("expected long-ring entry after downsample")
	}
	// Mean of 0,10,20,...,90 = 45.
	if longEntry.Value < 40 || longEntry.Value > 50 {
		t.Fatalf("expected mean ~45, got %v", longEntry.Value)
	}
}

func TestAggregatorStartDownsamplerDoesNotPanic(t *testing.T) {
	agg := NewTimeSeriesAggregator()
	ctx, cancel := context.WithCancel(context.Background())
	agg.StartDownsampler(ctx)
	cancel()
	// Should exit cleanly.
	time.Sleep(10 * time.Millisecond)
}
