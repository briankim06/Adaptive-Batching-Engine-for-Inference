# Phase 8 — Testing

Unit tests, integration tests, and benchmarks. The integration suite
drives the real gateway against the mock upstream from Phase 7 so every
test exercises the production code path over a loopback HTTP socket.

Tasks: 8.1 Models · 8.2 Strategies · 8.3 Circuit Breaker · 8.4 QueueMatrix · 8.5 Integration · 8.6 Benchmarks · 8.7 Latency Tracker EWMA

Shared references:
- [shared/invariants.md](./shared/invariants.md) — many tests exist specifically to guard invariants; each test group below cites the relevant Ix.

Depends on: Phases 1–7 for the code under test.

---

## Task 8.1 — Unit Tests: Models

Guards [I1](./shared/invariants.md#i1-context-propagation-r3).

```
Create internal/models/request_test.go:

TestTokenBucket:
  - 50 tokens → bucket 0
  - 200 tokens → bucket 1
  - 1000 tokens → bucket 2
  - 3000 tokens → bucket 4
  - 10000 tokens → bucket 5

TestPriorityMaxWait:
  - PriorityCritical → 5
  - PriorityHigh → 20
  - PriorityNormal → 50
  - PriorityLow → 100

TestNewInferenceRequest:
  - ID is non-empty
  - ResultChan is buffered with cap 1
  - EstimatedTokens = len(prompt)/4 + maxTokens
  - Ctx is the same context passed to the constructor (reference equality)

TestInferenceRequestCtxCancellation:
  - Build a request with context.WithCancel; cancel the parent.
  - req.Ctx.Err() returns context.Canceled immediately.
  - Marshal the request to JSON; verify the Ctx field is NOT serialised
    (json:"-" tag holds).

TestBatchMethods:
  - Size() returns len(Requests)
  - TotalTokens() sums EstimatedTokens
  - MaxPriority() returns highest priority in batch
```

---

## Task 8.2 — Unit Tests: Strategies

Guards [I7](./shared/invariants.md#i7-strategy-consumes-smoothed-p99-r6-hardening).

```
Create internal/batcher/strategies/strategy_test.go:

TestFixedStrategy:
  - CalculateTimeout always returns maxWaitMs regardless of queueDepth
  - ShouldFlush returns true at maxBatchSize, false below

TestQueueDepthStrategy:
  - queueDepth=0 → returns maxWaitMs
  - queueDepth=lowThreshold → returns maxWaitMs
  - queueDepth=highThreshold → returns minWaitMs
  - queueDepth midpoint → returns midpoint timeout (within 1ms tolerance)

TestLatencyAwareStrategy (PID controller, EWMA-fed):
  - Construct with a stub base strategy whose CalculateTimeout always
    returns 100ms, kP=0.05, targetP99Ms=50.
  - All TestConvergence* / TestStability cases populate
    StrategyMetrics.P99LatencyMs with values that simulate the OUTPUT of
    LatencyTracker.P99Smoothed(), not raw reservoir samples. This matches
    the production wiring and keeps the PID tests decoupled from EWMA.

  TestInitialMultiplier:
    - With no metrics and a fresh strategy, CalculateTimeout returns
      exactly the base timeout (multiplier seeded to 1.0).
    - nil metrics leaves the multiplier unchanged.

  TestConvergenceDownward:
    - Call CalculateTimeout repeatedly with P99LatencyMs=100 (2× target).
    - After many iterations the multiplier must monotonically decrease
      and eventually clamp at 0.5.
    - The returned timeout approaches base * 0.5 = 50ms.

  TestConvergenceUpward:
    - Call CalculateTimeout repeatedly with P99LatencyMs=25 (0.5× target).
    - Multiplier monotonically increases and clamps at 2.0.
    - Returned timeout approaches base * 2.0 = 200ms.

  TestStability:
    - Alternate P99LatencyMs between 50 (= target) and 50.
    - Multiplier must stay within a small epsilon of 1.0 — errRatio is 0
      so the multiplier is unchanged.

  TestInvalidMetrics:
    - P99LatencyMs <= 0 OR TargetP99Ms <= 0 → multiplier is NOT updated
      (guard clauses hold).

  TestConcurrentCalculateTimeout:
    - 100 goroutines call CalculateTimeout concurrently with varying
      metrics. No data races (run with -race). Multiplier stays clamped
      in [0.5, 2.0] throughout.

  TestShouldFlushDelegates:
    - ShouldFlush calls base.ShouldFlush verbatim.
```

---

## Task 8.3 — Unit Tests: Circuit Breaker

Guards [I3](./shared/invariants.md#i3-circuit-breaker-scope-r7).

```
Create internal/worker/circuit_test.go:

TestCircuitBreakerClosedOnSuccess:
  - Create breaker, record 10 successes → state is Closed
  - CanExecute returns true

TestCircuitBreakerOpensOnFailures:
  - Record failureThreshold failures → state is Open
  - CanExecute returns false

TestCircuitBreakerRecovery:
  - Open the breaker
  - Wait > recoveryTimeout
  - CanExecute returns true (transitions to HalfOpen)
  - Record success → state is Closed

TestCircuitBreakerHalfOpenFailure:
  - Get to HalfOpen state
  - Record failure → state goes back to Open

TestCircuitBreakerConcurrency:
  - 100 goroutines calling RecordSuccess/RecordFailure/CanExecute concurrently
  - No data races (run with -race)

TestCircuitBreakerPartialFailureScope:
  - Stand up a stub upstream via httptest.NewServer that returns
    HTTP 200 with a body containing per-request error fields.
  - Run the ProxyWorker's process() loop against it (directly or via a
    test helper that exercises the decode path).
  - After N successful round-trips, circuit state stays Closed — the
    per-request errors must NOT have called RecordFailure().
  - Contrast with a second stub that returns 5xx: N failures trip the
    breaker as expected. This guards the R7 invariant that application-
    level partial failures are not a transport-level signal.
```

---

## Task 8.4 — Unit Tests: QueueMatrix

Guards [I8](./shared/invariants.md#i8-priority-split-dispatch),
[I9](./shared/invariants.md#i9-token-bucketing-is-always-on),
[I10](./shared/invariants.md#i10-backpressure-propagates-end-to-end).

```
Create internal/batcher/packing/queue_matrix_test.go:

TestSubmitRoutesToCorrectSubQueue:
  - For each (priority, requestType, tokenBucket) triple, build a
    request with matching fields, Submit it, then inspect the matrix's
    queues array and verify only queues[p][rt][b] contains the request
    and every other sub-queue is empty.
  - Exercise all 4 priorities × 2 types × 6 buckets = 48 triples.

TestDrainPriorityOrder:
  - Submit 10 Low + 10 Critical requests concurrently from multiple
    goroutines.
  - After the barrier, call Drain(maxBatchSize=100).
  - Every returned batch consists entirely of Critical requests until
    all Critical requests are drained (no "select roulette" — strict
    priority). Only then do subsequent Drain calls return Low.
  - Repeat with Normal + High to cover every adjacent pair.

TestDrainHomogeneity:
  - Interleave submissions of mixed (type, bucket) pairs within a single
    priority.
  - Every batch returned by Drain is homogeneous in both fields — no
    post-accumulation splitting needed.

TestSubmitFullSubQueue:
  - Construct a QueueMatrix with totalCapacity=16 (perQueueCapacity=1).
  - Submit two requests with identical (priority, type, bucket) →
    second returns ErrQueueFull.
  - A third request with a DIFFERENT bucket succeeds — proves per-sub-
    queue isolation.
  - Depth() reflects only the successful submissions (ErrQueueFull must
    not increment depth).

TestEventChanCoalesces:
  - Submit 100 requests serially.
  - Assert len(qm.EventChan()) ≤ 1 at any observation point — the
    buffer-1 + non-blocking-send pattern coalesces all wakes.
  - After a single <-qm.EventChan() drain, subsequent Submits must wake
    it again.

TestEventChanNeverLeaks:
  - Spawn 1000 goroutines that each Submit and then exit.
  - Verify no goroutines are blocked inside Submit after the barrier
    (use runtime.NumGoroutine() with a reasonable ceiling check).

TestConcurrentSubmitDrain:
  - 100 producer goroutines submitting, 1 consumer goroutine draining.
  - Total drained count equals total submitted count (modulo ErrQueueFull
    returns). No data races under -race.

TestDepthAtomicity:
  - Submit 500 requests concurrently; Depth() read mid-run is always
    within [0, 500] and matches the final count after the barrier.

TestClose:
  - Close() then a non-blocking receive from every sub-queue returns
    ok=false (channel closed).
```

---

## Task 8.5 — Integration Tests

Guards [I1](./shared/invariants.md#i1-context-propagation-r3),
[I2](./shared/invariants.md#i2-non-blocking-resultchan-sends),
[I3](./shared/invariants.md#i3-circuit-breaker-scope-r7),
[I8](./shared/invariants.md#i8-priority-split-dispatch),
[I10](./shared/invariants.md#i10-backpressure-propagates-end-to-end).

```
Create test/integration/api_test.go:

Every test spins up the MockUpstream from simulation/mock_upstream.go so
the ProxyWorker actually POSTs over HTTP (no code-path divergence from
production). The API server under test is wired the same way as
cmd/server/main.go: collector, aggregator, pool, batcher, publisher,
Server — all via the full constructor chain.

Test harness helper newTestStack(t, opts) returns:
  - *httptest.Server wrapping the API routes
  - *MockUpstream (to adjust latency / failure behaviour mid-test)
  - cleanup func (closes both, cancels contexts)

TestCompletionEndpoint:
  - POST /v1/completions with a valid body
  - Verify 200, response.choices[0].text is non-empty
  - Verify response.id starts with "cmpl-"

TestEmbeddingEndpoint:
  - POST /v1/embeddings with a valid body
  - Verify 200 and a float64 array of length 128 (mock upstream default)

TestQueueFullReturns503:
  - Configure queue_capacity=16 (perQueueCapacity=1 → 48 slots total but
    only ONE per (priority, type, bucket)) and mock upstream with a
    2s artificial latency.
  - Fire 200 identical requests (same priority/type/bucket) from 200
    goroutines; expect ErrQueueFull → HTTP 503 for the vast majority.
  - At least one returns 200.

TestAllWorkersUnhealthyReturns503:
  - Configure mock upstream with failureRate=1.0 so /health always 500s.
  - Wait for 2 × health.check_interval to let the pool's ping loop flip
    upstreamUnhealthy.
  - POST /v1/completions; expect immediate HTTP 503 with
    type="capacity" — no queuing, no queue timeout.
  - Flip mock upstream back to healthy; after the next ping interval,
    subsequent requests receive 200.

TestClientCancelDropsRequest (R3):
  - Configure mock upstream with a 1s artificial latency.
  - POST /v1/completions with a request context cancelled after 100ms
    (via http.Client.Transport cancellation or httptest.NewRequest +
    WithContext).
  - Client sees context.Canceled / connection close.
  - Server side: handler returns 504 (or aborts), and the worker's
    post-response context check causes the result to be dropped silently
    — no panic from writing to a closed ResultChan.
  - Verify via mock upstream stats that at MOST one upstream round-trip
    occurred (pre-send scan may drop it if the ctx expired before the
    worker pulled the batch).
  - No goroutine leaks (runtime.NumGoroutine() delta < small threshold).

TestPriorityDispatchNoInversion (Chunk 7):
  - Configure the mock upstream with a 500ms artificial latency and
    workers.count=1 so batchChans[priority] fill predictably.
  - Submit in this order, back-to-back: 10 Low, then 10 Critical.
  - The Low batch formed first must ship first (single worker has it),
    but the SECOND-shipped batch must be Critical — not Low — even
    though Low arrivals are 10× more numerous. Assert this by capturing
    the x-request-priority header the mock upstream records per POST
    (add the header in the ProxyWorker for testability).
  - Verifies that routing to four priority-indexed batchChans prevents
    already-formed Low batches from FIFO-queuing ahead of Critical.

TestPartialFailureDoesNotTripBreaker (R7):
  - Configure mock upstream to return 200 with per-request error fields
    for every response.
  - Submit 100 completions; every client response is 500 (result.Error
    populated).
  - GET /admin/workers: every circuit breaker is still Closed (failure
    count = 0). The breaker does not fire on application-level errors.

TestMetricsEndpoint:
  - GET /metrics returns Prometheus text format.
  - Contains batcher_queue_depth, batcher_dispatch_blocked_total,
    upstream_unhealthy, worker_utilization, circuit_breaker_state.

TestJSONMetricsEndpoint:
  - GET /metrics/json returns valid JSON including all MetricsSnapshot
    fields: queue_depth, latency_p50_ms, latency_p99_ms, throughput_rps,
    workers[], upstream_ok.

TestStrategySwitch:
  - POST /admin/strategy with {"strategy": "fixed"} → 200.
  - GET /admin/config (or observe batcher.Metrics().ActiveStrategy) to
    confirm the switch.
  - Send traffic; latency_aware state (multiplier) must reset/restart
    when switched back — i.e. a fresh LatencyAwareStrategy instance,
    not a leaked one.

TestHealthEndpoint:
  - GET /health returns 200 with {"status": "ok"}.
```

---

## Task 8.6 — Benchmarks

```
Create test/benchmark/batcher_bench_test.go:

BenchmarkQueueMatrixSubmit:
  - Create a QueueMatrix with totalCapacity=100000.
  - b.RunParallel: concurrent Submit() calls with varying (priority,
    type, bucket) triples.
  - A background goroutine drains eventChan + Drain() repeatedly so
    capacity never fills.
  - Measures per-Submit latency and allocations.

BenchmarkBatcherSubmit:
  - Create an AdaptiveBatcher with large QueueCapacity and four
    priority-indexed batchChans.
  - b.RunParallel: Submit() from many goroutines; background goroutines
    drain each of the four batchChans.
  - Measures end-to-end Submit throughput (QueueMatrix + eventChan wake).

BenchmarkBatchFormation:
  - Pre-fill the QueueMatrix with N requests across multiple sub-queues.
  - Time formBatch() calls (event-driven wake + Drain).

BenchmarkLatencyTrackerAdd:
  - b.RunParallel: concurrent Add() calls.
  - Measures contention on the write Lock against the materialiser's
    periodic RLock-then-copy.

BenchmarkLatencyTrackerP99:
  - Pre-fill tracker with 10000 samples and Start the materialiser.
  - Sleep ~120ms so the atomics are populated.
  - Benchmark P99() calls — expected to be a lock-free atomic load
    (orders of magnitude faster than the v1 sort-on-every-call).
```

---

## Task 8.7 — Unit Tests: Latency Tracker EWMA

Guards [I7](./shared/invariants.md#i7-strategy-consumes-smoothed-p99-r6-hardening).

```
Create internal/metrics/latency_test.go.

TestEWMASeedsToFirstSample:
  - New tracker with alpha=0.3.
  - Add 100 samples of value 42, Start with a short interval, wait for
    the first materialisation. P99Smoothed() == 42 (within float epsilon).

TestEWMAConverges:
  - Seeded at 10ms. Feed 100 consecutive materialisation cycles where
    raw p99 = 100ms.
  - P99Smoothed() monotonically approaches 100ms and lands within 0.5ms
    after enough cycles to characterise α=0.3 convergence.

TestEWMARejectsSpike:
  - Seeded at 50ms. Inject a single materialisation cycle with raw
    p99 = 500ms. P99Smoothed() moves only by α*(500-50) = 135ms to 185ms
    — NOT jumping all the way to 500.
  - Next cycle with raw p99 back at 50: P99Smoothed() decays back toward
    50 exponentially.

TestResetClearsEWMA:
  - After populating, Reset(). seeded is false; P99Smoothed() returns 0.
  - Next materialisation re-seeds to the new raw p99, not a smoothed
    blend with the pre-reset value.

TestP99vsP99Smoothed:
  - Feed a sawtooth pattern of samples.
  - P99() must reflect the sawtooth exactly (raw percentile).
  - P99Smoothed() must show strictly less variance than P99() — the EWMA
    is doing its job.
```

---

## Verification commands

```bash
# After each phase
go build ./...
go test ./... -v -race
go vet ./...

# Run the server
go run cmd/server/main.go

# Run the dashboard (separate terminal)
go run cmd/dashboard/main.go

# Run simulations
go run cmd/simulator/main.go --scenarios=steady_state,spike_test \
    --strategies=fixed,queue_depth,latency_aware --output=table

# Benchmarks
go test ./test/benchmark/... -bench=. -benchmem
```

---

## What Phase 8 delivers

A full test suite: unit tests for every component, integration tests
that drive the gateway end-to-end through the mock upstream, and
benchmarks that characterise Submit throughput and percentile-read
performance. After Phase 8, the system is production-ready modulo
whatever upstream-specific wire-format adjustments the target inference
server may require.
