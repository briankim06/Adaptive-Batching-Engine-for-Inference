package metrics

import (
	"context"
	"sync"
	"time"
)

// DataPoint is a timestamped metric value stored in the ring buffers.
type DataPoint struct {
	Timestamp time.Time `json:"timestamp"`
	Value     float64   `json:"value"`
}

// timeSeries holds the short (5 min @ 1 s) and long (1 h @ 1 min) ring
// buffers for a single metric.
type timeSeries struct {
	short     [300]DataPoint // 5 min at 1 s resolution
	shortHead int
	long      [60]DataPoint // 1 h at 1 min resolution
	longHead  int
	mu        sync.RWMutex
}

func (ts *timeSeries) Add(p DataPoint) {
	ts.mu.Lock()
	ts.short[ts.shortHead] = p
	ts.shortHead = (ts.shortHead + 1) % len(ts.short)
	ts.mu.Unlock()
}

// TimeSeriesAggregator stores historical data points for the dashboard.
// Backed by fixed-size ring buffers — zero allocations after startup and
// memory is bounded.
type TimeSeriesAggregator struct {
	series map[string]*timeSeries
	mu     sync.RWMutex
}

// NewTimeSeriesAggregator creates the aggregator. Retention is a
// compile-time constant of the ring lengths (300 short × 1s = 5 min;
// 60 long × 1m = 1 h).
func NewTimeSeriesAggregator() *TimeSeriesAggregator {
	return &TimeSeriesAggregator{
		series: make(map[string]*timeSeries),
	}
}

// Add records a value for the named metric. The timeSeries is lazy-created
// on first use.
func (a *TimeSeriesAggregator) Add(metricName string, value float64) {
	ts := a.getOrCreate(metricName)
	ts.Add(DataPoint{
		Timestamp: time.Now(),
		Value:     value,
	})
}

// GetHistory returns data points for the named metric over the requested
// duration. Returns newest first. Always returns a freshly-allocated slice.
func (a *TimeSeriesAggregator) GetHistory(metricName string, duration time.Duration) []DataPoint {
	a.mu.RLock()
	ts, ok := a.series[metricName]
	a.mu.RUnlock()
	if !ok {
		return []DataPoint{}
	}

	cutoff := time.Now().Add(-duration)
	const shortWindow = 5 * time.Minute

	ts.mu.RLock()
	defer ts.mu.RUnlock()

	if duration <= shortWindow {
		return readRingBackward(ts.short[:], ts.shortHead, cutoff)
	}

	// For durations > 5 min, combine short and long rings.
	shortPoints := readRingBackward(ts.short[:], ts.shortHead, cutoff)
	longPoints := readRingBackward(ts.long[:], ts.longHead, cutoff)

	// Merge: shortPoints (newest first) then longPoints (newest first),
	// filtering out any long points that overlap with the short window.
	shortCutoff := time.Now().Add(-shortWindow)
	var result []DataPoint
	result = append(result, shortPoints...)
	for _, p := range longPoints {
		if !p.Timestamp.IsZero() && p.Timestamp.Before(shortCutoff) {
			result = append(result, p)
		}
	}
	return result
}

// StartDownsampler launches the 60-second downsampling loop. For each
// series it computes the arithmetic mean of recent short-ring entries
// whose timestamps fall inside the past minute, then writes one point
// into the long ring.
func (a *TimeSeriesAggregator) StartDownsampler(ctx context.Context) {
	go a.downsampleLoop(ctx)
}

func (a *TimeSeriesAggregator) downsampleLoop(ctx context.Context) {
	tick := time.NewTicker(60 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			a.downsampleAll()
		}
	}
}

func (a *TimeSeriesAggregator) downsampleAll() {
	a.mu.RLock()
	names := make([]string, 0, len(a.series))
	for name := range a.series {
		names = append(names, name)
	}
	a.mu.RUnlock()

	cutoff := time.Now().Add(-time.Minute)
	for _, name := range names {
		a.mu.RLock()
		ts := a.series[name]
		a.mu.RUnlock()

		ts.mu.Lock()
		sum := 0.0
		count := 0
		for i := 0; i < len(ts.short); i++ {
			p := ts.short[i]
			if !p.Timestamp.IsZero() && p.Timestamp.After(cutoff) {
				sum += p.Value
				count++
			}
		}
		if count > 0 {
			ts.long[ts.longHead] = DataPoint{
				Timestamp: time.Now(),
				Value:     sum / float64(count),
			}
			ts.longHead = (ts.longHead + 1) % len(ts.long)
		}
		ts.mu.Unlock()
	}
}

func (a *TimeSeriesAggregator) getOrCreate(name string) *timeSeries {
	a.mu.RLock()
	ts, ok := a.series[name]
	a.mu.RUnlock()
	if ok {
		return ts
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if ts, ok = a.series[name]; ok {
		return ts
	}
	ts = &timeSeries{}
	a.series[name] = ts
	return ts
}

// readRingBackward walks a ring buffer backward from head, returning
// points with timestamps after cutoff. Result is newest first.
func readRingBackward(ring []DataPoint, head int, cutoff time.Time) []DataPoint {
	n := len(ring)
	var result []DataPoint
	for i := 0; i < n; i++ {
		idx := (head - 1 - i + n) % n
		p := ring[idx]
		if p.Timestamp.IsZero() {
			continue
		}
		if p.Timestamp.Before(cutoff) {
			break
		}
		result = append(result, p)
	}
	return result
}
