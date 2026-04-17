# Adaptive Batching Engine — Technical Specification (Go)

A request batching gateway for LLM inference that dynamically optimizes the
latency/throughput trade-off based on current load conditions. Requests enter
via an OpenAI-compatible HTTP API, are grouped into batches by an adaptive
strategy layer, and are dispatched to an upstream inference server by proxy
workers. A real-time dashboard exposes system behavior as it runs. A
standalone simulator CLI drives load against the gateway via an in-process
mock upstream for strategy comparison and benchmarking.

The Go binary is designed to run on a cheap CPU node handling connection
holding, priority queuing, and dynamic batching; batches are routed to an
autoscaling GPU cluster running a standard inference server behind an HTTP
bulk endpoint.

---

## How this spec is organised

The spec is split by implementation phase. Each phase file is self-contained
for the tasks it covers; shared reference material (data models,
configuration schema, cross-cutting invariants) lives under `shared/` and is
linked from every phase that depends on it.

**If you are a coding agent:** load this `README.md` first, then the
specific phase file for your task, then `shared/invariants.md`. Shared
references (`shared/data-models.md`, `shared/configuration.md`) should be
loaded on demand when the task references types or config keys.

### Phase index

| Phase | File | Tasks | What it delivers |
|-------|------|-------|------------------|
| 1. Setup      | [01-setup.md](./01-setup.md)           | 1.1–1.3 | Buildable project with config and models |
| 2. Batching   | [02-batching.md](./02-batching.md)     | 2.1–2.7 | QueueMatrix + strategies (PID latency-aware) + event-driven batcher |
| 3. Workers    | [03-workers.md](./03-workers.md)       | 3.1–3.4 | Pull-model pool with ProxyWorker, per-worker circuit, shared upstream health ping |
| 4. Metrics    | [04-metrics.md](./04-metrics.md)       | 4.1–4.4 | Async P99, ring-buffer aggregator, Prometheus exporter |
| 5. API        | [05-api.md](./05-api.md)               | 5.1–5.7 | Running HTTP server with ctx-aware handlers and SnapshotPublisher WS |
| 6. Dashboard  | [06-dashboard.md](./06-dashboard.md)   | 6.1–6.2 | Real-time monitoring UI |
| 7. Simulation | [07-simulation.md](./07-simulation.md) | 7.1–7.4 | Load testing via in-process mock upstream |
| 8. Testing    | [08-testing.md](./08-testing.md)       | 8.1–8.7 | Unit, integration, and benchmark tests (+ verification commands) |

### Shared reference

- [shared/invariants.md](./shared/invariants.md) — cross-cutting contracts every phase depends on. **Load this whenever you implement anything.**
- [shared/data-models.md](./shared/data-models.md) — `InferenceRequest`, `Batch`, `RequestResult`, sentinel errors.
- [shared/configuration.md](./shared/configuration.md) — `config.yaml` schema, all nested config structs, validation rules.

After Phase 5, the system is fully runnable end-to-end via
`go run cmd/server/main.go` against any upstream that speaks the bulk batch
protocol — including the simulator's in-process mock upstream for local
development.

---

## Architecture

### System Diagram

```
                         ┌──────────────────────────────────────────────────┐
                         │                  API Server                      │
  HTTP POST              │  /v1/completions   /v1/embeddings                │
  ───────────────────►   │       │                  │                       │
                         │       ▼                  ▼                       │
                         │   ┌──────────────────────────┐                   │
                         │   │   InferenceHandler        │                   │
                         │   │   • parse request + ctx   │                   │
                         │   │   • create InferenceReq   │                   │
                         │   │   • submit to batcher     │                   │
                         │   │   • block on ResultChan   │◄──── response ──► │
                         │   └────────────┬─────────────┘                   │
                         └────────────────┼─────────────────────────────────┘
                                          │
                      Submit(ctx, req)    │  + wake eventChan
                                          ▼
                         ┌────────────────────────────────────┐
                         │         AdaptiveBatcher             │
                         │                                     │
                         │   QueueMatrix [4][2][6] sub-queues  │
                         │   (priority × type × token bucket)  │
                         │                                     │
                         │   formBatch() wakes on:             │
                         │     • eventChan  • timer  • ctx     │
                         │   drains one sub-queue in strict    │
                         │   priority order; batch is          │
                         │   homogeneous by construction.      │
                         │                                     │
                         │   ──► batchChans[p]  (4 channels,    │
                         │       one per priority, each buf=N)  │
                         └────────────────┬───────────────────┘
                                          │
                 pull via nested-select-with-default
                 (Critical > High > Normal > Low)
                                          ▼
                         ┌─────────────────────────────────────┐
                         │            WorkerPool                │
                         │                                      │
                         │  ┌──────────────┐ ┌──────────────┐  │
                         │  │ ProxyWorker 0│ │ ProxyWorker 1│  │
                         │  │ priority-select next batch       │
                         │  │ if unhealthy | !circuit: fail    │
                         │  │ pre-send ctx scan                │
                         │  │ POST upstream bulk endpoint      │
                         │  │ post-response ctx check          │
                         │  │ fan out to ResultChans           │
                         │  └──────┬───────┘ └──────┬───────┘  │
                         │         │                │           │
                         │  Circuit (per worker) + Health ping  │
                         └─────────┼────────────────┼──────────┘
                                   │                │
                                   ▼                ▼
                         ┌──────────────────────────────────────┐
                         │     Upstream Inference Server        │
                         │  POST /v1/batch  →  []BatchResult    │
                         └──────────────────────────────────────┘
```

### Request Lifecycle

This is the authoritative description of a request's journey. Every
component must conform to this flow.

1. **HTTP handler** receives POST, parses JSON into a `CompletionRequest` or
   `EmbeddingRequest`, validates fields. If `pool.AllWorkersUnhealthy()`
   returns true, the handler responds **HTTP 503** immediately without
   queuing.

2. Handler calls `NewInferenceRequest(ctx, ...)` passing `r.Context()`. The
   constructor assigns a UUID, estimates token count, creates a **buffered
   `ResultChan` (cap 1)**, stores the context on the request, and sets
   `RequestType` to either `completion` or `embedding`.

3. Handler calls `batcher.Submit(ctx, req)`. Submit is **non-blocking**: it
   routes the request into the exact sub-queue of the `QueueMatrix` keyed
   by `(req.Priority, req.RequestType, req.TokenBucket())`, then wakes the
   batcher via a non-blocking send on `eventChan`. If the target sub-queue
   is full, Submit returns `ErrQueueFull` immediately and the handler
   responds **HTTP 503**.

4. Handler blocks on `req.ResultChan` with a **context timeout** (default:
   server write timeout minus 1s). If the timeout fires before a result
   arrives, handler responds **HTTP 504** and the request is abandoned.
   The cancelled context is still referenced by the enqueued request so
   that workers can drop it cheaply.

5. **formBatch goroutine** (running inside AdaptiveBatcher) blocks on
   exactly three channels: `eventChan` (a submission occurred), `timer.C`
   (the strategy-calculated wait expired), or `ctx.Done()` (shutdown). On
   wake, it walks the `QueueMatrix` in strict priority order — Critical,
   High, Normal, Low — and drains the first non-empty sub-queue up to
   `maxBatchSize`. The returned batch is homogeneous by
   `(RequestType, TokenBucket)` by construction; no post-accumulation
   splitting is needed.

6. The formed `*Batch` is sent to `batchChans[batch.Priority]` — one of
   four priority-indexed channels, each buffered to `workers.count`.
   Because a batch is homogeneous by construction, its priority is known
   at the point of dispatch. If the send takes longer than 1ms,
   `batcher_dispatch_blocked_total` is incremented with a `priority`
   label; sustained non-zero values indicate worker under-provisioning.

7. **Any free `ProxyWorker`** pulls the next batch via a nested-select-
   with-default across the four priority channels: Critical checked first,
   then High, Normal, Low. Because the things being raced are already-
   priority-sorted *batches* (not individual requests), select-roulette is
   not a concern at this layer — at worst two contemporaneously-ready
   batches of the same priority go to different workers in arbitrary
   order, which is fine. If `unhealthy.Load()` or `circuit.CanExecute()`
   is false, the worker writes `ErrWorkerUnavailable` to every
   `req.ResultChan` in the batch and continues.

8. **Pre-send context scan.** The worker iterates `batch.Requests`; for
   each request where `req.Ctx.Err() != nil`, it writes a cancellation
   result (non-blocking send) and removes the request from the outgoing
   batch. If the scrubbed batch is empty, the worker skips the upstream
   call entirely.

9. Worker marshals the scrubbed batch into a bulk JSON body (via a
   pooled `*bytes.Buffer`), `POST`s to the upstream inference server
   through an `http.Request` whose context is cancelled if (a) the
   worker's context ends, (b) the upstream timeout fires, or (c) every
   per-request `Ctx.Done()` has fired. Transport errors and 5xx
   responses call `circuit.RecordFailure()` and fan out
   `ErrWorkerUnavailable`. A 200 containing per-request error fields is
   treated as request-level errors only — the breaker is **not** tripped.

10. **Post-response context check.** Before writing each result to
    `req.ResultChan`, the worker re-checks `req.Ctx.Err()`; if the
    client disappeared during the upstream round-trip, the result is
    dropped silently. All sends to `ResultChan` are non-blocking.

11. The HTTP handler (blocked at step 4) receives the `RequestResult`,
    formats it as an OpenAI-compatible JSON response, and returns
    **HTTP 200**.

---

## Invariants

These rules span phases and are the things most easily violated when a
phase is implemented in isolation. Before merging any phase, verify that
none of these are broken by your changes.

See [shared/invariants.md](./shared/invariants.md) for the full list with
rationale and references. A condensed checklist:

- **Context propagation.** `InferenceRequest.Ctx` is set by the HTTP
  handler and respected at the worker's pre-send and post-response
  checkpoints. Never ignore it.
- **Circuit breaker scope.** `RecordSuccess`/`RecordFailure` fire only on
  transport outcomes (connection errors, 5xx, successful non-5xx).
  Per-request error fields in a 200 body are request-level.
- **Non-blocking ResultChan sends.** Every write to `req.ResultChan` uses
  `select { case req.ResultChan <- r: default: }`. The handler may have
  left.
- **Pooled batch encoding.** Never call `json.Marshal` for a batch body.
  Use the shared `sync.Pool[*bytes.Buffer]` owned by `WorkerPool` and
  defer the `Put` **after** `httpClient.Do` returns.
- **Snapshot immutability.** `Collector.BuildSnapshot()` must allocate a
  fresh `[]WorkerSnapshot`. The `atomic.Pointer[MetricsSnapshot]`
  contract assumes the pointee is never mutated after publication.
- **Strategy consumes smoothed P99.** `LatencyAwareStrategy` reads
  `LatencyTracker.P99Smoothed()`, never raw `P99()`.
- **Priority-split dispatch.** Formed batches are routed to
  `batchChans[priority]`; workers drain via nested-select-with-default
  in priority order. Do not collapse back to a single `batchChan`.
- **Token bucketing is always on.** Segregation happens at Submit via
  the QueueMatrix; there is no `token_bucketing_enabled` knob to flip.

---

## Appendix — Verification commands

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

## Project structure

```
adaptive-batching-engine/
├── go.mod
├── go.sum
├── Makefile
├── README.md
├── config.yaml
├── docs/
│   ├── spec/                       # This spec
│   └── …                           # Per-component implementation notes
│
├── cmd/
│   ├── server/main.go              # API server entrypoint
│   ├── simulator/main.go           # Simulation runner CLI
│   └── dashboard/main.go           # Static dashboard server
│
├── internal/
│   ├── config/config.go            # Configuration loading (Viper)
│   │
│   ├── models/
│   │   ├── request.go              # InferenceRequest, Priority, RequestType
│   │   ├── batch.go                # Batch, BatchResult
│   │   └── errors.go               # Sentinel errors
│   │
│   ├── batcher/
│   │   ├── batcher.go              # Batcher interface
│   │   ├── adaptive.go             # AdaptiveBatcher
│   │   ├── strategies/
│   │   │   ├── strategy.go
│   │   │   ├── fixed.go
│   │   │   ├── queue_depth.go
│   │   │   └── latency_aware.go    # PID controller
│   │   └── packing/
│   │       └── queue_matrix.go     # Pre-segregated priority/type/bucket sub-queues
│   │
│   ├── worker/
│   │   ├── worker.go               # Worker interface
│   │   ├── proxy.go                # ProxyWorker (forwards to upstream)
│   │   ├── pool.go                 # Worker pool management
│   │   └── circuit.go              # Circuit breaker
│   │
│   ├── metrics/
│   │   ├── collector.go            # Central metrics registry
│   │   ├── latency.go              # Reservoir + async P99 + EWMA
│   │   ├── aggregator.go           # Ring-buffer time series
│   │   └── exporter.go             # Prometheus + JSON export
│   │
│   └── api/
│       ├── server.go
│       ├── handlers/
│       │   ├── inference.go        # /v1/completions, /v1/embeddings
│       │   ├── metrics.go          # /metrics endpoints
│       │   ├── admin.go            # /admin/* endpoints
│       │   └── ws.go               # WebSocket + SnapshotPublisher
│       └── middleware/tracking.go  # Request ID, logging, timing
│
├── dashboard/
│   └── static/index.html           # Single-page dashboard
│
├── simulation/
│   ├── workload.go                 # Traffic generators
│   ├── scenarios.go                # Predefined test scenarios
│   ├── mock_upstream.go            # In-process mock inference server
│   └── runner.go                   # Simulation orchestrator
│
└── test/
    ├── integration/api_test.go
    └── benchmark/batcher_bench_test.go
```

---

## Dependencies

```go
// go.mod
module github.com/yourname/adaptive-batching-engine

go 1.22

require (
    github.com/go-chi/chi/v5              // HTTP router
    github.com/spf13/viper                // Configuration
    github.com/prometheus/client_golang   // Prometheus metrics
    github.com/google/uuid                // UUIDs
    github.com/rs/zerolog                 // Structured logging
    golang.org/x/sync                     // errgroup
    github.com/gorilla/websocket          // Dashboard WebSocket
)
```
