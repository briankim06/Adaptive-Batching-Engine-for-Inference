package metrics

import (
	"sync"
	"sync/atomic"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/prometheus/client_golang/prometheus"
)

// Prometheus metric descriptors. Registered once by NewCollector.
var (
	promQueueDepth = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "batcher_queue_depth",
		Help: "Current number of requests in queue",
	})
	promBatchSize = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "batcher_batch_size",
		Help:    "Distribution of batch sizes",
		Buckets: []float64{1, 2, 4, 8, 16, 32, 64},
	})
	promRequestLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "request_latency_ms",
		Help:    "End-to-end request latency in milliseconds",
		Buckets: prometheus.ExponentialBuckets(1, 2, 12),
	})
	promRequestsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "requests_total",
		Help: "Total requests processed",
	})
	promRequestErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "requests_errors_total",
		Help: "Total requests that resulted in error",
	})
	promWorkerUtilization = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "worker_utilization",
		Help: "Worker status (0=idle, 1=busy, 2=unhealthy)",
	}, []string{"worker_id"})
	promCircuitState = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "circuit_breaker_state",
		Help: "Circuit breaker state (0=closed, 1=open, 2=half_open)",
	}, []string{"worker_id"})
	promDispatchBlockedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "batcher_dispatch_blocked_total",
		Help: "Times the batcher's send to batchChans[priority] took > 1ms (worker under-provisioning signal)",
	}, []string{"priority"})
	promUpstreamUnhealthy = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "upstream_unhealthy",
		Help: "1 if the upstream health ping has flipped unhealthy, 0 otherwise",
	})
)

// WorkerSnapshot captures a single worker's state for inclusion in a
// MetricsSnapshot. Allocated fresh each time BuildSnapshot runs (I6).
type WorkerSnapshot struct {
	ID               string `json:"id"`
	Status           int    `json:"status"`
	CircuitState     int    `json:"circuit_state"`
	BatchesProcessed int64  `json:"batches_processed"`
	TokensProcessed  int64  `json:"tokens_processed"`
}

// MetricsSnapshot is the immutable struct published via
// atomic.Pointer[MetricsSnapshot]. See invariant I6.
type MetricsSnapshot struct {
	Timestamp     time.Time        `json:"timestamp"`
	QueueDepth    int64            `json:"queue_depth"`
	LatencyP50Ms  float64          `json:"latency_p50_ms"`
	LatencyP99Ms  float64          `json:"latency_p99_ms"`
	ThroughputRPS float64          `json:"throughput_rps"`
	Workers       []WorkerSnapshot `json:"workers"`
	UpstreamOK    bool             `json:"upstream_ok"`
}

// WorkerInfoProvider is the interface the collector needs from the worker
// pool to build snapshots. Avoids importing the worker package.
type WorkerInfoProvider interface {
	GetWorkerSnapshots() []WorkerSnapshot
	UpstreamUnhealthy() bool
}

// registerOnce ensures Prometheus metrics are registered exactly once,
// even if NewCollector is called multiple times (e.g. in tests).
var registerOnce sync.Once

// Collector wraps all Prometheus metrics and the LatencyTracker. It is the
// single entry point for recording metrics throughout the system.
type Collector struct {
	Latency *LatencyTracker

	workerProvider WorkerInfoProvider

	// Internal counters for BuildSnapshot's throughput calculation.
	requestCount     atomic.Int64
	prevSnapshotTime atomic.Int64 // unix nanos
	queueDepth       atomic.Int64
}

// NewCollector creates all metrics, registers them with Prometheus, and
// creates a LatencyTracker.
func NewCollector(latencyWindowSize int, alpha float64) *Collector {
	c := &Collector{
		Latency: NewLatencyTracker(latencyWindowSize, alpha),
	}
	c.prevSnapshotTime.Store(time.Now().UnixNano())
	registerOnce.Do(func() {
		prometheus.MustRegister(
			promQueueDepth,
			promBatchSize,
			promRequestLatency,
			promRequestsTotal,
			promRequestErrors,
			promWorkerUtilization,
			promCircuitState,
			promDispatchBlockedTotal,
			promUpstreamUnhealthy,
		)
	})
	return c
}

// SetWorkerProvider wires the pool-level snapshot source used by
// BuildSnapshot. Called once during server startup.
func (c *Collector) SetWorkerProvider(p WorkerInfoProvider) {
	c.workerProvider = p
}

func (c *Collector) RecordRequest() {
	promRequestsTotal.Inc()
	c.requestCount.Add(1)
}

func (c *Collector) RecordError() {
	promRequestErrors.Inc()
}

func (c *Collector) RecordBatch(size int) {
	promBatchSize.Observe(float64(size))
}

func (c *Collector) RecordLatency(ms float64) {
	promRequestLatency.Observe(ms)
	c.Latency.Add(ms)
}

func (c *Collector) RecordDispatchBlocked(priority string) {
	promDispatchBlockedTotal.WithLabelValues(priority).Inc()
}

func (c *Collector) SetQueueDepth(depth int64) {
	promQueueDepth.Set(float64(depth))
	c.queueDepth.Store(depth)
}

func (c *Collector) SetWorkerStatus(id string, status int) {
	promWorkerUtilization.WithLabelValues(id).Set(float64(status))
}

func (c *Collector) SetCircuitState(id string, state int) {
	promCircuitState.WithLabelValues(id).Set(float64(state))
}

func (c *Collector) SetUpstreamUnhealthy(b bool) {
	if b {
		promUpstreamUnhealthy.Set(1)
	} else {
		promUpstreamUnhealthy.Set(0)
	}
}

// BuildSnapshot returns a freshly-allocated, immutable snapshot. The
// []WorkerSnapshot is always a new allocation (invariant I6).
func (c *Collector) BuildSnapshot() MetricsSnapshot {
	now := time.Now()
	prevTime := time.Unix(0, c.prevSnapshotTime.Load())
	elapsed := now.Sub(prevTime).Seconds()
	c.prevSnapshotTime.Store(now.UnixNano())

	reqs := c.requestCount.Swap(0)
	rps := 0.0
	if elapsed > 0 {
		rps = float64(reqs) / elapsed
	}

	var workers []WorkerSnapshot
	upstreamOK := true
	if c.workerProvider != nil {
		src := c.workerProvider.GetWorkerSnapshots()
		// Copy into a fresh slice so the snapshot is immutable (I6).
		workers = make([]WorkerSnapshot, len(src))
		copy(workers, src)
		upstreamOK = !c.workerProvider.UpstreamUnhealthy()
	}
	if workers == nil {
		workers = []WorkerSnapshot{}
	}

	return MetricsSnapshot{
		Timestamp:     now,
		QueueDepth:    c.queueDepth.Load(),
		LatencyP50Ms:  c.Latency.P50(),
		LatencyP99Ms:  c.Latency.P99(),
		ThroughputRPS: rps,
		Workers:       workers,
		UpstreamOK:    upstreamOK,
	}
}

// readGauge extracts the current value of a Prometheus Gauge via the dto
// write interface. Used only in tests or diagnostic paths.
func readGauge(g prometheus.Gauge) float64 {
	var m dto.Metric
	if err := g.Write(&m); err != nil {
		return 0
	}
	if m.Gauge != nil {
		return m.Gauge.GetValue()
	}
	return 0
}
