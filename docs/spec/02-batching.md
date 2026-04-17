# Phase 2 — Batching Layer

The request intake and batch-formation pipeline: four adaptive strategies,
a pre-segregated priority queue, and the event-driven batcher that drains
it into four priority-indexed dispatch channels.

Tasks: 2.1 Strategy Interface · 2.2 Fixed · 2.3 Queue-Depth · 2.4 Latency-Aware (PID) · 2.5 QueueMatrix · 2.6 Batcher Interface · 2.7 Adaptive Batcher

Shared references:
- [shared/data-models.md](./shared/data-models.md) — `InferenceRequest`, `Batch`, `Priority`, `TokenBucket()`.
- [shared/invariants.md](./shared/invariants.md) — especially [I7 smoothed P99](./shared/invariants.md#i7-strategy-consumes-smoothed-p99-r6-hardening), [I8 priority-split dispatch](./shared/invariants.md#i8-priority-split-dispatch), [I9 token bucketing always on](./shared/invariants.md#i9-token-bucketing-is-always-on), [I10 backpressure](./shared/invariants.md#i10-backpressure-propagates-end-to-end).

Depends on: Phase 1.
Consumed by: Phase 3 (workers pull from `batchChans`), Phase 5 (HTTP handlers call `Submit`).

---

## Strategy Interface

```go
// internal/batcher/strategies/strategy.go

type Strategy interface {
    // Name returns the strategy identifier (e.g. "fixed", "queue_depth").
    Name() string

    // CalculateTimeout returns how long formBatch should wait before flushing
    // the current accumulation, given the current queue depth and live metrics.
    CalculateTimeout(queueDepth int, metrics *StrategyMetrics) time.Duration

    // ShouldFlush returns true if the batch should be dispatched immediately
    // (e.g. batch is full, or a critical-priority request was added).
    ShouldFlush(batch []*models.InferenceRequest, queueDepth int) bool
}

type StrategyMetrics struct {
    P99LatencyMs float64
    TargetP99Ms  float64
}

type StrategyConfig struct {
    MinBatchSize int
    MaxBatchSize int
    MinWaitMs    int
    MaxWaitMs    int
}
```

### Task 2.1 — Strategy Interface

```
Create internal/batcher/strategies/strategy.go with the Strategy interface,
StrategyMetrics, and StrategyConfig structs exactly as shown above.
```

---

## Fixed Window Strategy

The simplest strategy. Always waits the same amount of time, always
flushes at the same size. Exists as a baseline for comparison.

- `CalculateTimeout` always returns `maxWaitMs`.
- `ShouldFlush` returns true when `len(batch) >= maxBatchSize`.

### Task 2.2 — Fixed Window Strategy

```
Implement internal/batcher/strategies/fixed.go:

FixedStrategy struct with maxWaitMs and maxBatchSize fields.
NewFixedStrategy(maxWaitMs, maxBatchSize int) constructor.
Name() returns "fixed".
CalculateTimeout ignores arguments, returns maxWaitMs as a Duration.
ShouldFlush returns true when len(batch) >= maxBatchSize.
```

---

## Queue-Depth Adaptive Strategy

Timeout varies linearly based on how full the queue is. When the queue
is empty, we wait longer (favor throughput by building bigger batches).
When the queue is full, we flush fast (favor latency by clearing
backlog).

```go
func (s *QueueDepthStrategy) CalculateTimeout(queueDepth int, _ *StrategyMetrics) time.Duration {
    if queueDepth <= s.lowThreshold {
        return time.Duration(s.maxWaitMs) * time.Millisecond
    }
    if queueDepth >= s.highThreshold {
        return time.Duration(s.minWaitMs) * time.Millisecond
    }

    // Linear interpolation between thresholds
    ratio := float64(queueDepth - s.lowThreshold) / float64(s.highThreshold - s.lowThreshold)
    waitMs := float64(s.maxWaitMs) - ratio*float64(s.maxWaitMs - s.minWaitMs)
    return time.Duration(waitMs) * time.Millisecond
}
```

`ShouldFlush`: same as fixed (batch size ≥ maxBatchSize).

### Task 2.3 — Queue-Depth Strategy

```
Implement internal/batcher/strategies/queue_depth.go:

QueueDepthStrategy struct with lowThreshold, highThreshold, minWaitMs,
maxWaitMs, maxBatchSize fields.
NewQueueDepthStrategy constructor taking all five parameters.
Name() returns "queue_depth".
CalculateTimeout implements the linear interpolation formula above exactly.
ShouldFlush returns true when len(batch) >= maxBatchSize.
```

---

## Latency-Aware Strategy

A decorator that wraps any base strategy and scales its timeout by a
stateful multiplier tracking observed p99 latency against a target. A
stateless ±20% bump ping-pongs around the target because it has no memory
of prior adjustments — the strategy oscillates between 0.8× and 1.2×
rather than settling on the correct wait time. A proportional controller
converges.

```go
type LatencyAwareStrategy struct {
    baseStrategy      Strategy
    currentMultiplier atomic.Uint64 // math.Float64bits of the live multiplier
    kP                float64       // proportional gain (default 0.05)
    targetP99Ms       float64
}

func (s *LatencyAwareStrategy) CalculateTimeout(qd int, metrics *StrategyMetrics) time.Duration {
    mult := math.Float64frombits(s.currentMultiplier.Load())
    if metrics != nil && metrics.P99LatencyMs > 0 && metrics.TargetP99Ms > 0 {
        errRatio := (metrics.P99LatencyMs - metrics.TargetP99Ms) / metrics.TargetP99Ms
        mult -= s.kP * errRatio
        mult = clamp(mult, 0.5, 2.0)
        s.currentMultiplier.Store(math.Float64bits(mult))
    }
    base := s.baseStrategy.CalculateTimeout(qd, metrics)
    return time.Duration(float64(base) * mult)
}
```

**Tuning notes:**

- `metrics.P99LatencyMs` MUST be pre-smoothed with an EWMA before it
  reaches this controller — see [I7 invariant](./shared/invariants.md#i7-strategy-consumes-smoothed-p99-r6-hardening).
  GPU latency has a non-linear, lagged response to queue depth and batch
  size; feeding the controller a raw, reservoir-sampled P99 will yo-yo the
  multiplier. The smoothing is implemented in `LatencyTracker`
  ([04-metrics.md Task 4.1](./04-metrics.md#task-41--latency-tracker)) and
  exposed via `P99Smoothed()`; the metrics-push loop populates
  `StrategyMetrics.P99LatencyMs` from that accessor, not from the raw
  `P99()`.
- Start with only the P term. `kP = 0.05` moves the multiplier by ~5% per
  standard-deviation miss. Add I and D terms later only if sustained
  offset or oscillation shows up in practice, and never add I before you
  have evidence of a persistent bias — integral windup is a distinct
  instability class.
- Clamp the multiplier to `[0.5, 2.0]` so a burst of outlier latencies
  can't drive the wait time to zero or blow it up unboundedly.

`ShouldFlush` delegates unchanged to the base strategy.

### Task 2.4 — Latency-Aware Strategy

```
Implement internal/batcher/strategies/latency_aware.go:

LatencyAwareStrategy struct exactly as shown above:
  - baseStrategy Strategy
  - currentMultiplier atomic.Uint64  (seeded to math.Float64bits(1.0))
  - kP float64
  - targetP99Ms float64

NewLatencyAwareStrategy(base Strategy, kP, targetP99Ms float64) constructor.
  Seed currentMultiplier to math.Float64bits(1.0).

Name() returns "latency_aware".
CalculateTimeout implements the proportional controller exactly as shown:
  1. Atomic-load the current multiplier via math.Float64frombits.
  2. If metrics has positive P99LatencyMs and TargetP99Ms, compute
     errRatio, update mult, clamp to [0.5, 2.0], atomic-store.
  3. Return baseStrategy.CalculateTimeout(qd, metrics) * mult as a
     time.Duration.

ShouldFlush delegates to baseStrategy.ShouldFlush.

Thread-safety: all strategy calls are concurrent-safe by virtue of the
atomic multiplier; no mutex needed.
```

---

## QueueMatrix

The QueueMatrix is the intake buffer for the batcher. It pre-segregates
requests at the entry gate by `(Priority, RequestType, TokenBucket())`
into a three-dimensional matrix of small buffered channels. Because every
request is routed into the exact sub-queue for its traits, any slice
drained from a single sub-queue is homogeneous by construction — no
post-accumulation splitting is needed. This realises invariant
[I9](./shared/invariants.md#i9-token-bucketing-is-always-on).

```go
type QueueMatrix struct {
    // queues[priority][requestType][tokenBucket]
    queues           [4][2][6]chan *models.InferenceRequest
    perQueueCapacity int
    depth            atomic.Int64
    eventChan        chan struct{} // buffer 1; wakes formBatch on Submit
}
```

The matrix has 4 × 2 × 6 = 48 sub-queues. Per-queue capacity is small —
`perQueueCapacity = ceil(queue_capacity / 16)` with a floor of 1 — so the
_total_ buffered capacity across the matrix stays bounded. What matters
is system-wide memory, not any single sub-queue.

**Submit** routes each request to the exact sub-queue keyed by
`(req.Priority, req.RequestType, req.TokenBucket())` via a non-blocking
send. On success, it increments `depth` and wakes the batcher:

```go
select {
case q.eventChan <- struct{}{}:
default: // already pending, skip
}
```

The buffer-1 + non-blocking send is critical: it coalesces multiple
arrivals into a single wake-up without leaking goroutines or losing
events.

If the target sub-queue is full, Submit returns `ErrQueueFull`; neither
`depth` nor `eventChan` is touched.

**Drain(maxBatchSize)** walks the matrix in strict priority order —
Critical → High → Normal → Low, and within each priority across
`RequestType` × `TokenBucket` iteration order. The first non-empty
sub-queue is drained up to `maxBatchSize` via non-blocking receives and
returned. Drain is sequential and non-blocking — a Critical request
submitted at the same instant as a Low request is always drained first
on the next wake-up, eliminating the "select roulette" that Go's random
case-picking causes under concurrent submission.

**Other accessors:**

- `EventChan() <-chan struct{}` — exposed to `formBatch`.
- `Depth() int64` — atomic load, O(1).
- `Close()` — closes every sub-queue channel.

### Task 2.5 — QueueMatrix

```
Implement internal/batcher/packing/queue_matrix.go exactly as described
above.

The public API is:
  NewQueueMatrix(totalCapacity int) *QueueMatrix
    - perQueueCapacity = (totalCapacity + 15) / 16, floored at 1.
    - Allocates all 48 channels with that buffer size.
    - Allocates eventChan as chan struct{} with buffer 1.

  Submit(req *models.InferenceRequest) error
    - Non-blocking send into
        queues[req.Priority][typeIndex(req.RequestType)][req.TokenBucket()].
    - On success: depth.Add(1), then non-blocking send on eventChan.
    - On full: returns ErrQueueFull without touching depth or eventChan.

  Drain(maxBatchSize int) (requests []*models.InferenceRequest,
                            priority models.Priority,
                            rt models.RequestType,
                            bucket int)
    - Walk queues[p][rt][b] in priority-descending order, then in
      (rt, bucket) iteration order.
    - For the first non-empty sub-queue, drain up to maxBatchSize via
      non-blocking receives; decrement depth by len(requests); return
      the slice plus its (priority, requestType, bucket) metadata.
    - If no sub-queue has anything, returns an empty slice.

  EventChan() <-chan struct{}
  Depth() int64
  Close()

Helper: typeIndex(rt models.RequestType) int
  completion → 0, embedding → 1.

Thread-safety: all sub-queue channels are safe for concurrent use. The
eventChan coalescing pattern is implemented via non-blocking select with
default. No extra mutex is needed.

A simpler two-level variant — segregating only by (RequestType, TokenBucket)
with priority enforced via a small mutex-protected slice per sub-queue — is
acceptable as a future optimisation if profiling shows measurable overhead
from the sparse 48-channel layout. Implement the full matrix first.
```

---

## Batcher Interface

```go
// internal/batcher/batcher.go

type Batcher interface {
    // Submit routes a request into the QueueMatrix sub-queue for its
    // (Priority, RequestType, TokenBucket) and wakes formBatch.
    // Non-blocking. Returns ErrQueueFull if the target sub-queue is at
    // capacity. Returns ErrShuttingDown if Stop has been called.
    Submit(ctx context.Context, req *models.InferenceRequest) error

    // Start begins the formBatch loop. Blocks until ctx is cancelled.
    Start(ctx context.Context) error

    // Stop signals shutdown. In-flight batches complete; queued requests
    // receive ErrShuttingDown on their ResultChans.
    Stop(ctx context.Context) error

    // QueueDepth returns the current total items across the QueueMatrix.
    QueueDepth() int64

    // Metrics returns a snapshot of batcher state.
    Metrics() BatcherMetrics

    // SetStrategy atomically swaps the active batching strategy.
    SetStrategy(s strategies.Strategy)

    // UpdateStrategyMetrics publishes the latest live metrics
    // (smoothed P99, target P99) for the strategy's next
    // CalculateTimeout call. Non-blocking, atomic swap.
    UpdateStrategyMetrics(m strategies.StrategyMetrics)
}

type BatcherMetrics struct {
    QueueDepth     int64   `json:"queue_depth"`
    BatchesFormed  int64   `json:"batches_formed"`
    RequestsQueued int64   `json:"requests_queued"`
    AvgBatchSize   float64 `json:"avg_batch_size"`
    ActiveStrategy string  `json:"active_strategy"`
}

type BatcherConfig struct {
    MaxBatchSize   int
    MinBatchSize   int
    QueueCapacity  int // total across the QueueMatrix; sub-queue cap is derived
    BatchChanSize  int // per-priority capacity of batchChans[i]; default: workers.count
}
```

### Task 2.6 — Batcher Interface

```
Create internal/batcher/batcher.go with the Batcher interface, BatcherMetrics,
and BatcherConfig structs exactly as shown above.

Note: token bucketing is always on (segregation happens at Submit time via
the QueueMatrix), so there is no TokenBucketingEnabled knob. Priority is
always enforced by the drain order for the same reason.
```

---

## Adaptive Batcher

The main orchestrator. Owns the `QueueMatrix`, runs the `formBatch`
loop, and sends each formed batch into one of four priority-indexed
`batchChans` that every `ProxyWorker` reads from. Batches are
homogeneous by construction, and priority is carried through dispatch
via the channel index — no post-accumulation splitting runs inside the
batcher.

**Strategy hot-swap** uses `atomic.Pointer[strategies.Strategy]` so the
`formBatch` hot path never locks.

**formBatch loop (event-driven):**

```go
func (b *AdaptiveBatcher) formBatch(ctx context.Context) (*models.Batch, models.Priority) {
    strategy := b.strategy.Load()
    timeout := strategy.CalculateTimeout(int(b.qm.Depth()), b.getMetrics())
    timer := time.NewTimer(timeout)
    defer timer.Stop()

    select {
    case <-b.qm.EventChan():
        // A submission occurred; fall through to drain.
    case <-timer.C:
        // Wait budget exhausted; drain whatever's there (may be empty).
    case <-ctx.Done():
        return nil, 0
    }

    reqs, priority, _, _ := b.qm.Drain(b.config.MaxBatchSize)
    if len(reqs) == 0 {
        return nil, 0
    }
    return models.NewBatch(reqs, strategy.Name()), priority
}
```

Because `QueueMatrix.Drain` walks sub-queues in strict priority order,
the returned slice is guaranteed to be both strictly priority-prioritised
and homogeneous by `(RequestType, TokenBucket)`. The nested priority-
ordered select pattern used in the v1 design — which was vulnerable to
Go's random case selection ("select roulette") — is gone.

`strategy.ShouldFlush` is consulted implicitly: `Drain(maxBatchSize)`
caps the batch size at `MaxBatchSize`, which is the only dimension the
v1 `ShouldFlush` checked. It remains part of the `Strategy` interface
for future use (e.g. flushing when a critical-priority request arrives
mid-accumulation in a future design).

**Dispatch:**

```go
func (b *AdaptiveBatcher) Start(ctx context.Context) error {
    for {
        select {
        case <-ctx.Done():
            return b.drain()
        default:
        }
        batch, priority := b.formBatch(ctx)
        if batch == nil {
            continue
        }
        start := time.Now()
        select {
        case b.batchChans[priority] <- batch:
        case <-ctx.Done():
            return b.drain()
        }
        if time.Since(start) > time.Millisecond {
            metrics.DispatchBlockedTotal.WithLabelValues(priority.String()).Inc()
        }
        b.batchesFormed.Add(1)
        b.totalRequestsInBatches.Add(int64(batch.Size()))
    }
}
```

Why four channels rather than one: if a Low batch is already queued in a
single shared channel and a Critical batch is formed next, the Critical
batch would sit FIFO behind the Low batch until a worker pulled it —
priority inversion between *formed* batches (the formation side already
prioritises correctly via QueueMatrix). Routing to one of four priority-
indexed channels lets workers select in strict priority order on the
receive side. See [I8](./shared/invariants.md#i8-priority-split-dispatch).

Each channel is buffered to `workers.count` — brief bursts are absorbed
per priority class, and sustained backpressure on any one channel
propagates back into the `QueueMatrix` so clients see 503 at Submit time
rather than queuing requests that will never dispatch.

**Drain on shutdown:**

When `ctx` is cancelled, `drain()` closes the `QueueMatrix` sub-queues
and sends `ErrShuttingDown` to the `ResultChan` of every remaining
request via non-blocking sends.

### Task 2.7 — Adaptive Batcher

```
Implement internal/batcher/adaptive.go:

AdaptiveBatcher struct with:
  - strategy: atomic.Pointer[strategies.Strategy]
  - qm: *packing.QueueMatrix
  - config: BatcherConfig
  - batchChans: [4]chan *models.Batch  (priority-indexed; owned by pool)
  - metrics tracking: batchesFormed (atomic int64),
    requestsQueued (atomic int64), totalRequestsInBatches (atomic int64)

NewAdaptiveBatcher(cfg BatcherConfig, strategy strategies.Strategy,
    batchChans [4]chan *models.Batch) *AdaptiveBatcher
  - Creates the QueueMatrix via NewQueueMatrix(cfg.QueueCapacity).

Submit(ctx, req) — calls qm.Submit(req); on success, increments
  requestsQueued; propagates ErrQueueFull and ErrShuttingDown.

Start(ctx) — runs the dispatch loop shown above:
  - (batch, priority) := formBatch() → send to batchChans[priority] →
    increment counters.
  - Record metrics.DispatchBlockedTotal.WithLabelValues(priority.String())
    when the send blocks > 1ms.
  - Exit via drain() when ctx is cancelled.

Stop(ctx) — cancel the internal context, await drain.
QueueDepth() — delegates to qm.Depth().
Metrics() — returns BatcherMetrics from atomic counters.
SetStrategy(s) — stores via atomic.Pointer.
UpdateStrategyMetrics(m) — stores m via atomic.Pointer[strategies.StrategyMetrics];
  getMetrics() loads it (returning nil before the first update, which
  CalculateTimeout already handles).

formBatch() (*Batch, Priority) — implement the event-driven select
  shown above; returns both the batch and the priority reported by
  QueueMatrix.Drain so the caller can route it.
drain() — close QueueMatrix, iterate remaining sub-queues, non-blocking
  send ErrShuttingDown on each request's ResultChan.
```

---

## What Phase 2 delivers

At the end of Phase 2, all strategies, the QueueMatrix, the Batcher
interface, and the AdaptiveBatcher are implemented. Nothing is wired end-
to-end yet (workers come in Phase 3), but the batcher can be unit-tested
in isolation by feeding it a stub `batchChans` array and observing
drains. Tests for this phase live in
[08-testing.md Task 8.2 and 8.4](./08-testing.md).
