# Phase 4 — Metrics

Observability primitives: reservoir-sampled latency tracker with async
P99 materialisation and EWMA smoothing, a Prometheus collector with all
instrumentation, a ring-buffer time-series aggregator, and the exporter
that bridges to `/metrics` and JSON.

Tasks: 4.1 Latency Tracker · 4.2 Prometheus Metrics · 4.3 Time-Series Aggregator · 4.4 Metrics Exporter

Shared references:
- [shared/configuration.md](./shared/configuration.md) — `metrics:` section including `p99_smoothing_alpha`.
- [shared/invariants.md](./shared/invariants.md) — especially [I6 snapshot immutability](./shared/invariants.md#i6-snapshot-immutability-r8c) and [I7 smoothed P99](./shared/invariants.md#i7-strategy-consumes-smoothed-p99-r6-hardening).

Depends on: Phase 1.
Consumed by: Phases 2 (strategy reads P99Smoothed), 3 (circuit/status reporting), 5 (handlers + WS).

---

## Latency Tracker (Reservoir Sampling + Async Materialization)

Maintains a fixed-size reservoir of latency samples. As new samples
arrive, they replace existing ones with decreasing probability, ensuring
the reservoir remains a statistically representative sample. Percentile
values are **materialised off the hot path** by a background ticker and
exposed to readers via atomics, so the strategy layer reading `P99()`
on every `formBatch` iteration pays an `O(1)` atomic load rather than
an `O(N log N)` sort.

```go
type LatencyTracker struct {
    samples []float64
    count   int64
    size    int
    mu      sync.RWMutex

    // EWMA smoothing state (R6 hardening). Raw reservoir percentiles
    // alone are too noisy to feed the latency-aware controller; GPU
    // latency responds non-linearly to queue/batch variation.
    alpha float64
    // seeded=0 until the first materialisation; see below.
    seeded atomic.Bool

    // Materialised percentiles (math.Float64bits). Readers use atomics,
    // so the hot path never contends with Add / the ticker.
    p50, p90, p99, p999 atomic.Uint64
    // Smoothed P99 (EWMA) for the strategy consumer.
    p99Smoothed atomic.Uint64
}

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
// Default tick interval: 100ms.
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
            lt.mu.RLock()
            snap := make([]float64, len(lt.samples))
            copy(snap, lt.samples)
            lt.mu.RUnlock()

            if len(snap) == 0 {
                continue
            }
            sort.Float64s(snap)
            raw99 := pctile(snap, 99)
            lt.p50.Store(math.Float64bits(pctile(snap, 50)))
            lt.p90.Store(math.Float64bits(pctile(snap, 90)))
            lt.p99.Store(math.Float64bits(raw99))
            lt.p999.Store(math.Float64bits(pctile(snap, 99.9)))

            // EWMA: seed on first sample, otherwise α·new + (1−α)·prev.
            if !lt.seeded.Load() {
                lt.p99Smoothed.Store(math.Float64bits(raw99))
                lt.seeded.Store(true)
            } else {
                prev := math.Float64frombits(lt.p99Smoothed.Load())
                smoothed := lt.alpha*raw99 + (1-lt.alpha)*prev
                lt.p99Smoothed.Store(math.Float64bits(smoothed))
            }
        }
    }
}

func (lt *LatencyTracker) P50() float64         { return math.Float64frombits(lt.p50.Load()) }
func (lt *LatencyTracker) P90() float64         { return math.Float64frombits(lt.p90.Load()) }
func (lt *LatencyTracker) P99() float64         { return math.Float64frombits(lt.p99.Load()) }
func (lt *LatencyTracker) P999() float64        { return math.Float64frombits(lt.p999.Load()) }
func (lt *LatencyTracker) P99Smoothed() float64 { return math.Float64frombits(lt.p99Smoothed.Load()) }

func (lt *LatencyTracker) Reset() { /* lock, clear samples, reset count, zero all atomics, reset seeded */ }
```

### Task 4.1 — Latency Tracker

```
Implement internal/metrics/latency.go exactly as specified above.

NewLatencyTracker(windowSize int, alpha float64) *LatencyTracker
  - Allocates the reservoir.
  - Stores alpha (typically cfg.Metrics.P99SmoothingAlpha, default 0.3).
  - Seeds all percentile atomics to math.Float64bits(0).
  - Leaves `seeded` false so the first materialisation cycle bootstraps
    the EWMA to the raw P99 rather than smoothing toward 0.

Add(ms float64)  — reservoir-sampling update under Lock.
Start(ctx, interval time.Duration) — launches the materialisation ticker
  goroutine (default interval 100ms).
materializeLoop — sorts a snapshot of samples each tick; stores the four
  raw percentiles atomically; updates P99Smoothed via EWMA (seed on
  first tick, α·raw + (1−α)·prev thereafter).
P50/P90/P99/P999/P99Smoothed() — O(1) atomic loads via math.Float64frombits.
Reset() — clears samples under Lock, zeros all percentile atomics, and
  resets `seeded` to false so the next materialisation re-seeds the EWMA.

pctile(sorted []float64, p float64) float64 — private helper that
  interpolates: idx := int(float64(len(sorted)) * p / 100); clamp to
  len-1; return sorted[idx].

Strategy consumption: the metrics-push loop in cmd/server/main.go
populates strategy.StrategyMetrics.P99LatencyMs from P99Smoothed(), NOT
from P99(). Dashboards continue to display the raw percentiles.

Thread-safety: Add contends only with the ticker's RLock-then-copy; the
ticker itself contends only with Add; readers on the hot path (P99(),
P99Smoothed()) are fully lock-free.
```

---

## Prometheus Metrics

```go
var (
    queueDepth = prometheus.NewGauge(prometheus.GaugeOpts{
        Name: "batcher_queue_depth",
        Help: "Current number of requests in queue",
    })
    batchSize = prometheus.NewHistogram(prometheus.HistogramOpts{
        Name:    "batcher_batch_size",
        Help:    "Distribution of batch sizes",
        Buckets: []float64{1, 2, 4, 8, 16, 32, 64},
    })
    requestLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
        Name:    "request_latency_ms",
        Help:    "End-to-end request latency in milliseconds",
        Buckets: prometheus.ExponentialBuckets(1, 2, 12), // 1ms to 4096ms
    })
    requestsTotal = prometheus.NewCounter(prometheus.CounterOpts{
        Name: "requests_total",
        Help: "Total requests processed",
    })
    requestErrors = prometheus.NewCounter(prometheus.CounterOpts{
        Name: "requests_errors_total",
        Help: "Total requests that resulted in error",
    })
    workerUtilization = prometheus.NewGaugeVec(prometheus.GaugeOpts{
        Name: "worker_utilization",
        Help: "Worker status (0=idle, 1=busy, 2=unhealthy)",
    }, []string{"worker_id"})
    circuitState = prometheus.NewGaugeVec(prometheus.GaugeOpts{
        Name: "circuit_breaker_state",
        Help: "Circuit breaker state (0=closed, 1=open, 2=half_open)",
    }, []string{"worker_id"})
    dispatchBlockedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
        Name: "batcher_dispatch_blocked_total",
        Help: "Times the batcher's send to batchChans[priority] took > 1ms (worker under-provisioning signal)",
    }, []string{"priority"})
    upstreamUnhealthy = prometheus.NewGauge(prometheus.GaugeOpts{
        Name: "upstream_unhealthy",
        Help: "1 if the upstream health ping has flipped unhealthy, 0 otherwise",
    })
)
```

The `Collector` struct wraps these and provides recording methods. The
dispatch-blocked counter is incremented from the batcher's `Start` loop
(see [02-batching.md Task 2.7](./02-batching.md#task-27--adaptive-batcher));
the upstream-unhealthy gauge is mirrored from the pool's
`upstreamUnhealthy` flag by the snapshot publisher
([05-api.md Task 5.6](./05-api.md#task-56--websocket-handler)).

### Task 4.2 — Prometheus Metrics

```
Implement internal/metrics/collector.go:

Collector struct holding references to all Prometheus metrics above, plus a
  *LatencyTracker.

NewCollector(latencyWindowSize int, alpha float64) *Collector
  - Creates and registers all metrics with prometheus.DefaultRegisterer.
  - Creates a LatencyTracker(latencyWindowSize, alpha).

Methods:
  RecordRequest()                 — increments requestsTotal
  RecordError()                   — increments requestErrors
  RecordBatch(size int)           — observes batchSize
  RecordLatency(ms float64)       — observes requestLatency, adds to LatencyTracker
  RecordDispatchBlocked(priority Priority) — increments dispatchBlockedTotal{priority}
  SetQueueDepth(depth int64)      — sets queueDepth gauge
  SetWorkerStatus(id string, status WorkerStatus) — sets workerUtilization
  SetCircuitState(id string, state CircuitState)  — sets circuitState
  SetUpstreamUnhealthy(b bool)    — sets upstreamUnhealthy gauge (0/1)
  BuildSnapshot() MetricsSnapshot — returns a freshly-allocated, immutable
                                    snapshot (see copy-semantics note below)

MetricsSnapshot struct:
  Timestamp      time.Time         `json:"timestamp"`
  QueueDepth     int64             `json:"queue_depth"`
  LatencyP50Ms   float64           `json:"latency_p50_ms"`
  LatencyP99Ms   float64           `json:"latency_p99_ms"`
  ThroughputRPS  float64           `json:"throughput_rps"`   (requestsTotal delta / interval)
  Workers        []WorkerSnapshot  `json:"workers"`           (freshly allocated; see below)
  UpstreamOK     bool              `json:"upstream_ok"`

Copy-semantics (required for R8c safety; see I6):
  BuildSnapshot() MUST allocate a new []WorkerSnapshot and copy each
  WorkerSnapshot value in — never return a slice that aliases a mutable
  backing array owned by the collector or the pool. Once returned, the
  snapshot is shared between goroutines via atomic.Pointer (Task 5.6)
  and must not be mutated by anyone. Violating this caveat re-introduces
  the exact race that R8c is designed to eliminate.
```

---

## Time-Series Aggregator (Ring Buffer)

Stores historical data points for the dashboard's charts. Backed by
fixed-size ring buffers — after startup the aggregator does **zero
allocations** regardless of traffic, and memory usage is capped at a
constant bound per metric.

- **Short ring**: 1-second resolution for the last 5 minutes
  (`[300]DataPoint`).
- **Long ring**: 1-minute resolution for the last hour
  (`[60]DataPoint`), downsampled from the short ring as an
  **arithmetic mean**.
- A background goroutine runs every 60 seconds, averages the 60 most
  recent short-ring entries whose timestamps fall inside that minute,
  and writes one point into the long ring. Nothing is ever deleted —
  older entries are simply overwritten as the head pointer wraps.

```go
type DataPoint struct {
    Timestamp time.Time `json:"timestamp"`
    Value     float64   `json:"value"`
}

type timeSeries struct {
    short     [300]DataPoint // 5 min at 1s resolution
    shortHead int            // next write index
    long      [60]DataPoint  // 1 h at 1m resolution
    longHead  int
    mu        sync.RWMutex
}

func (ts *timeSeries) Add(p DataPoint) {
    ts.mu.Lock()
    ts.short[ts.shortHead] = p
    ts.shortHead = (ts.shortHead + 1) % len(ts.short)
    ts.mu.Unlock()
}

type TimeSeriesAggregator struct {
    series map[string]*timeSeries // metric name → rings
    mu     sync.RWMutex           // guards the map only
}
```

Per-metric `timeSeries` values are created lazily on the first `Add`
for a given metric name. The aggregator's map-level mutex only guards
map lookup/insertion; reads and writes to a given series are
synchronised by the series's own `mu`.

### Task 4.3 — Time-Series Aggregator

```
Implement internal/metrics/aggregator.go:

DataPoint, timeSeries, and TimeSeriesAggregator structs exactly as shown
above. Rings are fixed-size arrays, not slices.

NewTimeSeriesAggregator() *TimeSeriesAggregator
  - No retention argument: retention is now a compile-time constant of
    the ring lengths (300 short × 1s = 5 min; 60 long × 1m = 1 h).

Add(metricName string, value float64)
  - Lazy-creates the metric's timeSeries under map-level Lock on first
    use; subsequent Adds take only the per-series Lock.
  - Writes a DataPoint into short[shortHead] and advances shortHead.
  - O(1), zero allocation after the first Add per metric.

GetHistory(metricName string, duration time.Duration) []DataPoint
  - Walk backward from shortHead for the requested duration, skipping
    zero-value DataPoints (pre-populated slots).
  - Fall back to the long ring for durations > 5 minutes.
  - Returns newest first. Always returns a freshly-allocated slice.

StartDownsampler(ctx context.Context)
  - Every 60s: for each series, compute arithmetic mean of the last
    60 short entries whose timestamps fall inside the past minute,
    write one DataPoint into long[longHead], advance longHead.
  - Exits on ctx.Done().

Thread-safety: aggregator-level mutex guards only the series map.
Per-series mutex guards that series's two rings.
```

---

## Metrics Exporter

### Task 4.4 — Metrics Exporter

```
Implement internal/metrics/exporter.go:

PrometheusHandler() http.Handler
  — returns promhttp.Handler() for /metrics endpoint.

JSONSnapshot(collector *Collector) ([]byte, error)
  — marshals collector.BuildSnapshot() to JSON.

CSVExport(agg *TimeSeriesAggregator, metricName string, duration time.Duration) ([]byte, error)
  — returns CSV bytes with columns: timestamp, value.
```

---

## What Phase 4 delivers

At the end of Phase 4, all observability primitives are implemented and
unit-testable. They wire into `main.go` (Task 5.7) and `/metrics`,
`/metrics/json`, `/metrics/history` (Task 5.4). Tests live in
[08-testing.md Task 8.7](./08-testing.md#task-87--unit-tests-latency-tracker-ewma).
