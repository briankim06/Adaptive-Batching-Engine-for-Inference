# Invariants

Cross-cutting contracts that span phases. Every phase file assumes these
hold; violating any of them from inside a single phase will silently break
behaviour that the tests in [../08-testing.md](../08-testing.md) depend on.

Read this before implementing anything.

---

## I1. Context propagation (R3)

Every `InferenceRequest` carries a `context.Context` in its `Ctx` field,
set from `r.Context()` by the HTTP handler (see [05-api.md Task 5.3](../05-api.md#task-53--inference-handlers)).

- The handler passes `ctx` as the first argument to `NewInferenceRequest`.
- The batcher's `Submit` stores the request with its context intact.
- Workers honour `req.Ctx` at two checkpoints:
  - **Pre-send scan**: before marshalling the batch, drop any request
    whose `Ctx.Err() != nil` and write a cancellation `RequestResult`
    via non-blocking send.
  - **Post-response check**: before writing each per-request result,
    re-check `Ctx.Err()` and drop silently if cancelled.

The `Ctx` field is tagged `json:"-"` — **never** serialise it to the wire.

## I2. Non-blocking `ResultChan` sends

Every write to `req.ResultChan` uses the non-blocking pattern:

```go
select {
case req.ResultChan <- result:
default:
    // handler abandoned — drop
}
```

`ResultChan` is buffered with capacity 1. The non-blocking send is
mandatory because the HTTP handler may have already returned (e.g. context
timeout fired, client disconnect). A blocking send would leak the worker
goroutine permanently.

## I3. Circuit breaker scope (R7)

`CircuitBreaker.RecordSuccess()` and `RecordFailure()` fire **only** for
transport-level outcomes in the ProxyWorker:

- Connection error, `net.Error`, context deadline → `RecordFailure()`.
- HTTP status ≥ 500 → `RecordFailure()`.
- HTTP status < 500 (successful round-trip) → `RecordSuccess()`, even if
  the body contains per-request error fields.

Per-request errors from a 200 response body are **request-level** and
propagate via individual `ResultChan`s. They never touch the breaker.

Upstream reachability is a separate concern tracked by the pool-level
`upstreamUnhealthy` flag maintained by a dedicated ping goroutine. See
[03-workers.md Task 3.4](../03-workers.md#task-34--worker-pool).

## I4. Pooled batch encoding (R1 hardening)

**Never** call `json.Marshal` directly for a batch body. A full
32-request batch with long prompts can exceed several MB, and per-batch
allocation at steady state will thrash the GC.

Use the shared `sync.Pool[*bytes.Buffer]` allocated by `WorkerPool` and
pass it to every `ProxyWorker`:

```go
buf := w.bufPool.Get().(*bytes.Buffer)
buf.Reset()
defer w.bufPool.Put(buf)
if err := json.NewEncoder(buf).Encode(scrubbed); err != nil { ... }

httpReq, _ := http.NewRequestWithContext(reqCtx, http.MethodPost, w.upstream.URL, bytes.NewReader(buf.Bytes()))
httpReq.Header.Set("Content-Type", "application/json")
httpReq.ContentLength = int64(buf.Len())
```

The `defer w.bufPool.Put(buf)` MUST run **after** `httpClient.Do` returns
— `http.Client` reads from the reader throughout the POST. Set
`Content-Length` explicitly so upstream servers that preallocate decode
buffers can do so without consuming the body first.

## I5. Upstream context composition (R1/R3 hardening)

The `reqCtx` passed to `http.NewRequestWithContext` must be derived such
that it cancels when **any** of:

- The worker's `ctx` ends (shutdown).
- `upstream.RequestTimeout` elapses (`context.WithTimeout`).
- **Every** per-request `Ctx.Done()` has fired (so a batch whose every
  client has disconnected is not kept alive by the upstream server).

The last condition is implemented by a `spawnAllCancelledWatcher`
goroutine that waits on each request's `Ctx.Done()` in turn and calls
`cancel()` once all have fired. The caller MUST `defer stopWatch()` to
clean up the watcher on the happy path. See
[03-workers.md Task 3.3](../03-workers.md#task-33--proxy-worker) for the
exact helper.

## I6. Snapshot immutability (R8c)

`Collector.BuildSnapshot()` returns a `MetricsSnapshot` that is published
via `atomic.Pointer[MetricsSnapshot]` to the WebSocket handler and
potentially read by many clients concurrently.

- `BuildSnapshot()` MUST allocate a fresh `[]WorkerSnapshot` and copy each
  worker's state in by value. **Never** return a slice header aliasing
  mutable backing storage (e.g. the collector's internal `[]WorkerSnapshot`
  or the pool's `[]Worker`).
- Once a snapshot is stored in the atomic pointer, nothing is permitted
  to mutate it. All readers load it via `SnapshotPublisher.Current()` and
  either marshal it or read fields lock-free.

Violating this re-introduces the exact torn-read race the atomic pointer
is designed to eliminate.

## I7. Strategy consumes smoothed P99 (R6 hardening)

`LatencyAwareStrategy.CalculateTimeout` reads
`StrategyMetrics.P99LatencyMs`. The metrics-push loop in `main.go`
populates this field from `LatencyTracker.P99Smoothed()`, **never** from
the raw `P99()`. See
[04-metrics.md Task 4.1](../04-metrics.md#task-41--latency-tracker) for
the EWMA implementation and
[05-api.md Task 5.7](../05-api.md#task-57--main-server-entry-point) for
the wiring.

Dashboards and debugging tools continue to read `P99()` for the raw
series — both accessors coexist.

## I8. Priority-split dispatch

The batcher and pool communicate through four priority-indexed channels
`batchChans [4]chan *models.Batch`, **not** a single shared `batchChan`.

- `AdaptiveBatcher.Start` sends each formed batch to
  `batchChans[batch.Priority]`.
- `ProxyWorker.Run` reads via a nested-select-with-default in strict
  priority order (Critical → High → Normal → Low).
- `WorkerPool.BatchChans()` returns the four channels for wiring; the
  pool also owns `CloseBatchChans()` for shutdown.

Do not collapse these back to a single channel. Doing so re-introduces
the priority inversion between already-formed batches that this design
eliminates.

## I9. Token bucketing is always on

Token bucketing is not a knob; it is the segregation key of the
`QueueMatrix` (priority × type × token bucket). There is no
`token_bucketing_enabled` config option. There is no `TokenGrouper`
utility. Post-accumulation splitting does not exist in this design —
every batch drained from a sub-queue is homogeneous by construction.

## I10. Backpressure propagates end-to-end

The only way to shed load is **at the HTTP boundary via 503**. There are
three surface points:

1. `pool.AllWorkersUnhealthy()` → 503 at handler entry, before any
   queue slot is allocated.
2. `ErrQueueFull` from `QueueMatrix.Submit` → 503 (per-sub-queue
   capacity is derived from `queue_capacity / 16`).
3. Sustained `batcher_dispatch_blocked_total{priority}` increments —
   backpressure is propagating but not yet shedding; over-provisioning
   time-window hint.

Never silently drop requests anywhere else.

---

## Changelog note

These invariants consolidate the eight R1–R8 revisions from the original
v2 design plus four Chunk 7 hardenings:

- R1 Proxy Worker → I1, I3, I4, I5
- R2 Pull Model → I8, I10
- R3 Context Propagation → I1, I5
- R4 QueueMatrix → I9, I10
- R5 Event-Driven Drain → I8
- R6 PID Latency Strategy → I7
- R7 Circuit Breaker Isolation → I3
- R8 Observability Safety → I6
- Chunk 7: priority-split dispatch → I8
- Chunk 7: EWMA pre-filter → I7
- Chunk 7: context watcher → I5
- Chunk 7: pooled JSON buffers → I4
