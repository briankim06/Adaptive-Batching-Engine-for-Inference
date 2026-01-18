# Adaptive Batching Engine — Technical Specification (Go)

> This document defines the architecture and expected behavior. Reference it when implementing components.

## Overview

A request batching system for LLM inference that dynamically optimizes the latency/throughput trade-off based on current load conditions.

---

## Project Structure

```
adaptive-batching-engine/
├── go.mod
├── go.sum
├── Makefile
├── README.md
├── SPEC.md
├── config.yaml
│
├── cmd/
│   ├── server/
│   │   └── main.go              # API server entrypoint
│   ├── simulator/
│   │   └── main.go              # Simulation runner CLI
│   └── dashboard/
│       └── main.go              # Dashboard server (serves frontend)
│
├── internal/
│   ├── config/
│   │   └── config.go            # Configuration loading (Viper)
│   │
│   ├── models/
│   │   ├── request.go           # InferenceRequest, Priority, RequestType
│   │   ├── batch.go             # Batch, BatchResult
│   │   └── errors.go            # Custom error types
│   │
│   ├── batcher/
│   │   ├── batcher.go           # Batcher interface
│   │   ├── adaptive.go          # Main AdaptiveBatcher implementation
│   │   ├── strategies/
│   │   │   ├── strategy.go      # Strategy interface
│   │   │   ├── fixed.go         # Fixed window strategy
│   │   │   ├── queue_depth.go   # Queue-depth adaptive
│   │   │   └── latency_aware.go # Latency-aware strategy
│   │   └── packing/
│   │       ├── token_grouper.go # Group by token length
│   │       └── priority_queue.go# Priority-based queuing
│   │
│   ├── worker/
│   │   ├── worker.go            # Worker interface
│   │   ├── inference.go         # Simulated inference worker
│   │   ├── pool.go              # Worker pool management
│   │   ├── balancer.go          # Load balancing strategies
│   │   └── circuit.go           # Circuit breaker
│   │
│   ├── metrics/
│   │   ├── collector.go         # Central metrics registry
│   │   ├── latency.go           # Percentile tracking (t-digest or reservoir)
│   │   ├── aggregator.go        # Time-series aggregation
│   │   └── exporter.go          # Prometheus + JSON export
│   │
│   └── api/
│       ├── server.go            # HTTP server setup (Chi/Gin/Fiber)
│       ├── handlers/
│       │   ├── inference.go     # /v1/completions, /v1/embeddings
│       │   ├── metrics.go       # /metrics endpoints
│       │   └── admin.go         # /admin/* endpoints
│       └── middleware/
│           └── tracking.go      # Request ID, logging, timing
│
├── pkg/
│   └── client/
│       └── client.go            # Go client for the API (optional)
│
├── simulation/
│   ├── workload.go              # Traffic generators
│   ├── scenarios.go             # Predefined test scenarios
│   └── runner.go                # Simulation orchestrator
│
├── dashboard/
│   ├── static/                  # HTML/JS/CSS for web dashboard
│   └── templates/
│
└── test/
    ├── integration/
    │   └── api_test.go
    └── benchmark/
        └── batcher_bench_test.go
```

---

## Dependencies

```go
// go.mod
module github.com/yourname/adaptive-batching-engine

go 1.22

require (
    github.com/go-chi/chi/v5      // HTTP router (or gin-gonic/gin)
    github.com/spf13/viper        // Configuration
    github.com/prometheus/client_golang // Prometheus metrics
    github.com/google/uuid        // UUIDs
    github.com/rs/zerolog         // Structured logging
    golang.org/x/sync             // errgroup, semaphore
)
```

---

## Data Models

### internal/models/request.go

```go
type RequestType string

const (
    RequestTypeCompletion RequestType = "completion"
    RequestTypeEmbedding  RequestType = "embedding"
)

type Priority int

const (
    PriorityLow Priority = iota
    PriorityNormal
    PriorityHigh
    PriorityCritical
)

func (p Priority) MaxWaitMs() int {
    switch p {
    case PriorityCritical: return 5
    case PriorityHigh:     return 20
    case PriorityNormal:   return 50
    case PriorityLow:      return 100
    default:               return 50
    }
}

type InferenceRequest struct {
    ID              string      `json:"id"`
    Prompt          string      `json:"prompt"`
    MaxTokens       int         `json:"max_tokens"`
    Priority        Priority    `json:"priority"`
    RequestType     RequestType `json:"request_type"`
    CreatedAt       time.Time   `json:"created_at"`
    EstimatedTokens int         `json:"estimated_tokens"`
    
    // Internal: channel to send result back to caller
    ResultChan      chan *RequestResult `json:"-"`
}

func NewInferenceRequest(prompt string, maxTokens int, priority Priority, reqType RequestType) *InferenceRequest {
    return &InferenceRequest{
        ID:              uuid.New().String(),
        Prompt:          prompt,
        MaxTokens:       maxTokens,
        Priority:        priority,
        RequestType:     reqType,
        CreatedAt:       time.Now(),
        EstimatedTokens: len(prompt)/4 + maxTokens, // Rough estimate
        ResultChan:      make(chan *RequestResult, 1),
    }
}

func (r *InferenceRequest) AgeMs() float64 {
    return float64(time.Since(r.CreatedAt).Milliseconds())
}

func (r *InferenceRequest) TokenBucket() int {
    buckets := []int{128, 512, 1024, 2048, 4096}
    for i, threshold := range buckets {
        if r.EstimatedTokens <= threshold {
            return i
        }
    }
    return len(buckets)
}
```

### internal/models/batch.go

```go
type Batch struct {
    ID           string              `json:"id"`
    Requests     []*InferenceRequest `json:"requests"`
    CreatedAt    time.Time           `json:"created_at"`
    StrategyUsed string              `json:"strategy_used"`
}

func NewBatch(requests []*InferenceRequest, strategy string) *Batch {
    return &Batch{
        ID:           uuid.New().String(),
        Requests:     requests,
        CreatedAt:    time.Now(),
        StrategyUsed: strategy,
    }
}

func (b *Batch) Size() int {
    return len(b.Requests)
}

func (b *Batch) TotalTokens() int {
    total := 0
    for _, r := range b.Requests {
        total += r.EstimatedTokens
    }
    return total
}

func (b *Batch) MaxPriority() Priority {
    max := PriorityLow
    for _, r := range b.Requests {
        if r.Priority > max {
            max = r.Priority
        }
    }
    return max
}

type BatchResult struct {
    BatchID          string        `json:"batch_id"`
    Results          []any         `json:"results"`
    ProcessingTimeMs float64       `json:"processing_time_ms"`
    WorkerID         string        `json:"worker_id"`
    CompletedAt      time.Time     `json:"completed_at"`
    Error            error         `json:"-"`
}

type RequestResult struct {
    RequestID string `json:"request_id"`
    Result    any    `json:"result"`
    Error     error  `json:"-"`
    LatencyMs float64 `json:"latency_ms"`
}
```

### internal/models/errors.go

```go
var (
    ErrQueueFull         = errors.New("request queue is full")
    ErrWorkerUnavailable = errors.New("no workers available")
    ErrCircuitOpen       = errors.New("circuit breaker is open")
    ErrBatchTimeout      = errors.New("batch processing timed out")
    ErrInvalidRequest    = errors.New("invalid inference request")
    ErrShuttingDown      = errors.New("server is shutting down")
)
```

---

## Interfaces

### internal/batcher/batcher.go

```go
type Batcher interface {
    // Submit adds a request and returns immediately
    // Caller receives result via request.ResultChan
    Submit(ctx context.Context, req *InferenceRequest) error
    
    // Start begins the batching loop (call in goroutine)
    Start(ctx context.Context) error
    
    // Stop gracefully shuts down, draining pending requests
    Stop(ctx context.Context) error
    
    // Metrics returns current batcher state
    QueueDepth() int
    Metrics() BatcherMetrics
}

type BatcherMetrics struct {
    QueueDepth     int     `json:"queue_depth"`
    BatchesFormed  int64   `json:"batches_formed"`
    RequestsQueued int64   `json:"requests_queued"`
    AvgBatchSize   float64 `json:"avg_batch_size"`
}
```

### internal/batcher/strategies/strategy.go

```go
type Strategy interface {
    // Name returns the strategy identifier
    Name() string
    
    // CalculateTimeout returns how long to wait before flushing
    // given current queue depth and metrics
    CalculateTimeout(queueDepth int, metrics *StrategyMetrics) time.Duration
    
    // ShouldFlush returns true if batch should be flushed immediately
    ShouldFlush(batch []*models.InferenceRequest, queueDepth int) bool
}

type StrategyMetrics struct {
    P99LatencyMs float64
    TargetP99Ms  float64
}
```

### internal/worker/worker.go

```go
type Worker interface {
    ID() string
    Status() WorkerStatus
    ProcessBatch(ctx context.Context, batch *models.Batch) (*models.BatchResult, error)
    HealthCheck(ctx context.Context) error
}

type WorkerStatus int

const (
    WorkerStatusIdle WorkerStatus = iota
    WorkerStatusBusy
    WorkerStatusUnhealthy
)
```

---

## Batching Strategies

### 1. Fixed Window (Baseline)
- Wait exactly `maxWaitMs` OR until `maxBatchSize` reached
- Simple FIFO via channel
- No adaptation

### 2. Queue-Depth Adaptive
Timeout varies linearly based on queue depth:

```go
func (s *QueueDepthStrategy) CalculateTimeout(queueDepth int, _ *StrategyMetrics) time.Duration {
    if queueDepth <= s.lowThreshold {
        return time.Duration(s.maxWaitMs) * time.Millisecond
    }
    if queueDepth >= s.highThreshold {
        return time.Duration(s.minWaitMs) * time.Millisecond
    }
    
    // Linear interpolation
    ratio := float64(queueDepth-s.lowThreshold) / float64(s.highThreshold-s.lowThreshold)
    waitMs := float64(s.maxWaitMs) - ratio*float64(s.maxWaitMs-s.minWaitMs)
    return time.Duration(waitMs) * time.Millisecond
}
```

Default thresholds: `low=10`, `high=100`

### 3. Latency-Aware
Adjusts based on observed p99 latency:

```go
func (s *LatencyAwareStrategy) CalculateTimeout(queueDepth int, metrics *StrategyMetrics) time.Duration {
    baseTimeout := s.baseStrategy.CalculateTimeout(queueDepth, metrics)
    
    if metrics.P99LatencyMs > metrics.TargetP99Ms*1.1 {
        // Latency too high → flush faster
        return time.Duration(float64(baseTimeout) * 0.8)
    }
    if metrics.P99LatencyMs < metrics.TargetP99Ms*0.8 {
        // Latency has headroom → can wait longer
        return time.Duration(float64(baseTimeout) * 1.2)
    }
    return baseTimeout
}
```

---

## Token Bucketing

Group requests by estimated token count to minimize padding waste:

| Bucket | Token Range | Index |
|--------|-------------|-------|
| 0      | 0-128       | 0     |
| 1      | 129-512     | 1     |
| 2      | 513-1024    | 2     |
| 3      | 1025-2048   | 3     |
| 4      | 2049-4096   | 4     |
| 5      | 4096+       | 5     |

Implementation uses separate channels per bucket.

---

## Priority Queue Behavior

| Priority | Max Wait | Behavior |
|----------|----------|----------|
| CRITICAL | 5ms      | Triggers immediate flush |
| HIGH     | 20ms     | Checked before NORMAL |
| NORMAL   | 50ms     | Default |
| LOW      | 100ms    | Checked last |

Implementation: Separate buffered channels per priority. Batcher uses `select` with priority ordering.

---

## Worker Pool

### Simulated Processing Time
```go
func (w *InferenceWorker) simulateProcessing(batch *models.Batch) time.Duration {
    baseMs := w.config.BaseLatencyMs
    perTokenMs := w.config.PerTokenLatencyMs
    variance := w.config.LatencyVariance
    
    processingMs := baseMs + perTokenMs*float64(batch.TotalTokens())
    processingMs += rand.NormFloat64() * baseMs * variance
    
    return time.Duration(processingMs) * time.Millisecond
}
```

Default values: `base=10ms`, `perToken=0.5ms`, `variance=0.1`

### Load Balancing Strategies

```go
type BalancerType string

const (
    BalancerRoundRobin  BalancerType = "round_robin"
    BalancerLeastLoaded BalancerType = "least_loaded"
    BalancerPowerOfTwo  BalancerType = "power_of_two"
)
```

- **RoundRobin**: Atomic counter mod N
- **LeastLoaded**: Track pending count per worker
- **PowerOfTwo**: `rand.Intn(n)` twice, pick lower pending

---

## Circuit Breaker

States: `CLOSED` → `OPEN` → `HALF_OPEN` → `CLOSED`

```go
type CircuitState int

const (
    CircuitClosed CircuitState = iota
    CircuitOpen
    CircuitHalfOpen
)

type CircuitBreaker struct {
    state            CircuitState
    failureCount     int
    lastFailureTime  time.Time
    failureThreshold int           // Default: 5
    recoveryTimeout  time.Duration // Default: 30s
    mu               sync.RWMutex
}

func (cb *CircuitBreaker) RecordSuccess() {
    cb.mu.Lock()
    defer cb.mu.Unlock()
    cb.failureCount = 0
    if cb.state == CircuitHalfOpen {
        cb.state = CircuitClosed
    }
}

func (cb *CircuitBreaker) RecordFailure() {
    cb.mu.Lock()
    defer cb.mu.Unlock()
    cb.failureCount++
    cb.lastFailureTime = time.Now()
    if cb.failureCount >= cb.failureThreshold {
        cb.state = CircuitOpen
    }
}

func (cb *CircuitBreaker) CanExecute() bool {
    cb.mu.Lock()
    defer cb.mu.Unlock()
    
    switch cb.state {
    case CircuitClosed:
        return true
    case CircuitOpen:
        if time.Since(cb.lastFailureTime) > cb.recoveryTimeout {
            cb.state = CircuitHalfOpen
            return true
        }
        return false
    case CircuitHalfOpen:
        return true
    }
    return false
}
```

---

## Metrics

### Prometheus Metrics

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
        Buckets: prometheus.ExponentialBuckets(1, 2, 12), // 1ms to 4s
    })
    
    requestsTotal = prometheus.NewCounter(prometheus.CounterOpts{
        Name: "requests_total",
        Help: "Total requests processed",
    })
    
    workerUtilization = prometheus.NewGaugeVec(prometheus.GaugeOpts{
        Name: "worker_utilization",
        Help: "Worker utilization (0-1)",
    }, []string{"worker_id"})
)
```

### Latency Percentile Tracking (Reservoir Sampling)

```go
type LatencyTracker struct {
    samples []float64
    count   int64
    size    int
    mu      sync.RWMutex
}

func NewLatencyTracker(size int) *LatencyTracker {
    return &LatencyTracker{
        samples: make([]float64, 0, size),
        size:    size,
    }
}

func (lt *LatencyTracker) Add(latencyMs float64) {
    lt.mu.Lock()
    defer lt.mu.Unlock()
    
    lt.count++
    if len(lt.samples) < lt.size {
        lt.samples = append(lt.samples, latencyMs)
    } else {
        // Reservoir sampling
        j := rand.Int63n(lt.count)
        if j < int64(lt.size) {
            lt.samples[j] = latencyMs
        }
    }
}

func (lt *LatencyTracker) Percentile(p float64) float64 {
    lt.mu.RLock()
    defer lt.mu.RUnlock()
    
    if len(lt.samples) == 0 {
        return 0
    }
    
    sorted := make([]float64, len(lt.samples))
    copy(sorted, lt.samples)
    sort.Float64s(sorted)
    
    idx := int(float64(len(sorted)) * p / 100)
    if idx >= len(sorted) {
        idx = len(sorted) - 1
    }
    return sorted[idx]
}

func (lt *LatencyTracker) P50() float64  { return lt.Percentile(50) }
func (lt *LatencyTracker) P90() float64  { return lt.Percentile(90) }
func (lt *LatencyTracker) P99() float64  { return lt.Percentile(99) }
func (lt *LatencyTracker) P999() float64 { return lt.Percentile(99.9) }
```

---

## API Endpoints

### POST /v1/completions
```json
// Request
{
    "prompt": "Hello, world",
    "max_tokens": 256,
    "priority": "normal"
}

// Response
{
    "id": "cmpl-xxx",
    "object": "text_completion", 
    "created": 1234567890,
    "choices": [{"text": "...", "index": 0}],
    "usage": {"prompt_tokens": 3, "completion_tokens": 50, "total_tokens": 53}
}
```

### POST /v1/embeddings
```json
// Request
{
    "input": "Text to embed",
    "priority": "normal"
}

// Response
{
    "object": "list",
    "data": [{"object": "embedding", "embedding": [0.1, 0.2, ...], "index": 0}]
}
```

### GET /metrics
Prometheus format text output

### GET /metrics/json
```json
{
    "timestamp": "2024-01-15T10:30:00Z",
    "queue_depth": 42,
    "latency_p50_ms": 15.2,
    "latency_p99_ms": 45.8,
    "throughput_rps": 150.5,
    "workers": [
        {"id": "worker-0", "status": "busy", "utilization": 0.85},
        {"id": "worker-1", "status": "idle", "utilization": 0.0}
    ]
}
```

### POST /admin/strategy/{name}
Switch batching strategy at runtime. Valid: `fixed`, `queue_depth`, `latency_aware`

---

## Configuration (config.yaml)

```yaml
server:
  host: "0.0.0.0"
  port: 8080
  read_timeout: 30s
  write_timeout: 30s

batching:
  strategy: "queue_depth"  # fixed, queue_depth, latency_aware
  min_batch_size: 1
  max_batch_size: 32
  min_wait_ms: 5
  max_wait_ms: 100
  queue_depth_low_threshold: 10
  queue_depth_high_threshold: 100
  queue_capacity: 10000
  token_bucketing_enabled: true
  priority_enabled: true

workers:
  count: 4
  base_latency_ms: 10
  per_token_latency_ms: 0.5
  latency_variance: 0.1
  max_batch_tokens: 8192

health:
  check_interval: 5s
  failure_threshold: 3
  recovery_timeout: 30s

metrics:
  collection_interval: 100ms
  percentile_window_size: 1000
  history_retention: 1h
```

---

## Concurrency Patterns

### Batcher Main Loop

```go
func (b *AdaptiveBatcher) Start(ctx context.Context) error {
    for {
        select {
        case <-ctx.Done():
            return b.drain(ctx)
        default:
            batch := b.formBatch(ctx)
            if batch != nil {
                b.dispatchChan <- batch
            }
        }
    }
}

func (b *AdaptiveBatcher) formBatch(ctx context.Context) *models.Batch {
    timeout := b.strategy.CalculateTimeout(b.queueDepth(), b.getMetrics())
    timer := time.NewTimer(timeout)
    defer timer.Stop()
    
    var requests []*models.InferenceRequest
    
    for {
        select {
        case req := <-b.priorityQueue.Critical():
            requests = append(requests, req)
            if b.strategy.ShouldFlush(requests, b.queueDepth()) {
                return models.NewBatch(requests, b.strategy.Name())
            }
        case req := <-b.priorityQueue.High():
            requests = append(requests, req)
        case req := <-b.priorityQueue.Normal():
            requests = append(requests, req)
        case req := <-b.priorityQueue.Low():
            requests = append(requests, req)
        case <-timer.C:
            if len(requests) > 0 {
                return models.NewBatch(requests, b.strategy.Name())
            }
            return nil
        case <-ctx.Done():
            return nil
        }
        
        if len(requests) >= b.config.MaxBatchSize {
            return models.NewBatch(requests, b.strategy.Name())
        }
    }
}
```

### Worker Pool Dispatch

```go
func (p *WorkerPool) Start(ctx context.Context) error {
    g, ctx := errgroup.WithContext(ctx)
    
    // Start workers
    for _, w := range p.workers {
        w := w
        g.Go(func() error {
            return w.Run(ctx)
        })
    }
    
    // Dispatch loop
    g.Go(func() error {
        for {
            select {
            case batch := <-p.batchChan:
                worker := p.balancer.Select(p.workers)
                if worker == nil || !p.circuits[worker.ID()].CanExecute() {
                    // Retry or error handling
                    continue
                }
                worker.Submit(batch)
            case <-ctx.Done():
                return nil
            }
        }
    })
    
    return g.Wait()
}
```

---

## Simulation Workloads

| Scenario | Pattern | Description |
|----------|---------|-------------|
| steady_state | Constant 100 RPS | Baseline performance |
| ramp_up | 10 → 500 RPS over 60s | Scaling behavior |
| spike_test | 100 RPS with 10x spike | Burst handling |
| mixed_priority | 80% normal, 15% high, 5% critical | Priority handling |
| long_tail | Bimodal: 70% short, 30% long requests | Token bucketing effectiveness |
