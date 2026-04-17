# Phase 5 — API Layer

HTTP server, middleware, inference handlers, metrics/admin/health
endpoints, WebSocket for the dashboard, and the `main.go` entry point
that wires the whole system together.

Tasks: 5.1 HTTP Server · 5.2 Middleware · 5.3 Inference Handlers · 5.4 Metrics Handlers · 5.5 Admin Handlers · 5.6 WebSocket Handler · 5.7 Main

Shared references:
- [shared/data-models.md](./shared/data-models.md) — `InferenceRequest`, `Priority`, `RequestType`.
- [shared/configuration.md](./shared/configuration.md) — `server:`, `dashboard:`, `metrics:` sections.
- [shared/invariants.md](./shared/invariants.md) — especially [I1 ctx propagation](./shared/invariants.md#i1-context-propagation-r3), [I6 snapshot immutability](./shared/invariants.md#i6-snapshot-immutability-r8c), [I7 smoothed P99](./shared/invariants.md#i7-strategy-consumes-smoothed-p99-r6-hardening), [I10 backpressure](./shared/invariants.md#i10-backpressure-propagates-end-to-end).

Depends on: Phases 1–4.
Consumed by: Phase 6 (Dashboard frontend connects to `/ws`), Phase 7 (Simulation Runner mirrors the wiring).

---

## HTTP Server

```go
// internal/api/server.go

type Server struct {
    router    chi.Router
    batcher   batcher.Batcher
    pool      *worker.WorkerPool
    collector *metrics.Collector
    agg       *metrics.TimeSeriesAggregator
    publisher *handlers.SnapshotPublisher // R8c; owned by main.go, shared here
    cfg       *config.Config
    httpSrv   *http.Server
}
```

The `SnapshotPublisher` is created and started by `main.go` alongside
the other background goroutines (Task 5.7); the Server just holds a
reference so `/ws` can hand it to `HandleWebSocket`.

**Route table:**

| Method | Path                | Handler             |
|--------|---------------------|----------------------|
| POST   | /v1/completions     | HandleCompletions    |
| POST   | /v1/embeddings      | HandleEmbeddings     |
| GET    | /metrics            | PrometheusHandler    |
| GET    | /metrics/json       | HandleJSONMetrics    |
| GET    | /metrics/history    | HandleMetricsHistory |
| GET    | /admin/config       | HandleGetConfig      |
| POST   | /admin/strategy     | HandleSetStrategy    |
| GET    | /admin/workers      | HandleGetWorkers     |
| GET    | /health             | HandleHealth         |
| GET    | /ws                 | HandleWebSocket      |

### Task 5.1 — HTTP Server Setup

```
Create internal/api/server.go:

Server struct as shown above.
NewServer(cfg *config.Config, batcher batcher.Batcher, pool *worker.WorkerPool,
    collector *metrics.Collector, agg *metrics.TimeSeriesAggregator,
    publisher *handlers.SnapshotPublisher) *Server
  - Creates chi.NewRouter()
  - Mounts middleware (see Task 5.2)
  - Mounts all routes from the route table above
  - Passes pool into HandleCompletions/HandleEmbeddings so they can call
    AllWorkersUnhealthy() at Submit time (R7 fast-fail).
  - Passes publisher into HandleWebSocket (R8c).

Start(ctx context.Context) error
  - Creates http.Server with cfg.Server.Host, Port, timeouts
  - Runs ListenAndServe in a goroutine
  - Blocks on ctx.Done(), then calls Shutdown with a 10s grace period

HandleHealth returns 200 with {"status": "ok"}.
```

### Task 5.2 — Middleware

```
Implement internal/api/middleware/tracking.go:

RequestID(next http.Handler) http.Handler
  - Generates UUID, sets X-Request-ID response header.
  - Adds request ID to request context.

Logging(logger zerolog.Logger) func(http.Handler) http.Handler
  - Logs: method, path, status code, duration, request ID.
  - Uses zerolog.

MetricsMiddleware(collector *metrics.Collector) func(http.Handler) http.Handler
  - Records request latency to collector after response.

Recovery(next http.Handler) http.Handler
  - Catches panics, logs stack trace, returns 500.
```

---

## Inference Handlers

**POST /v1/completions** request/response (OpenAI-compatible):

```json
// Request
{
    "prompt": "Hello, world",
    "max_tokens": 256,
    "priority": "normal"
}

// Response (200)
{
    "id": "cmpl-<uuid>",
    "object": "text_completion",
    "created": 1234567890,
    "choices": [{"text": "...", "index": 0}],
    "usage": {"prompt_tokens": 3, "completion_tokens": 50, "total_tokens": 53}
}

// Error (503 — queue full)
{ "error": { "message": "request queue is full", "type": "capacity" } }

// Error (504 — timeout)
{ "error": { "message": "request timed out", "type": "timeout" } }
```

**POST /v1/embeddings:**

```json
// Request
{
    "input": "Text to embed",
    "priority": "normal"
}

// Response (200)
{
    "object": "list",
    "data": [{"object": "embedding", "embedding": [0.1, 0.2, ...], "index": 0}]
}
```

**Handler flow (both endpoints follow the same pattern):**

1. Decode JSON request body.
2. Validate fields (prompt/input non-empty, `max_tokens > 0` for
   completions).
3. Parse priority string to `Priority` constant (default
   `PriorityNormal`).
4. **Fast-fail if all workers are unhealthy.** Call
   `pool.AllWorkersUnhealthy()`; if true, respond **HTTP 503**
   immediately with a `capacity` error type. Nothing is queued — we'd
   just time out later, wasting a queue slot and a goroutine (R7).
5. Derive a per-request context from `r.Context()` with a timeout of
   `server.write_timeout - 1s` (the safety margin ensures the handler
   can still write a 504 body before the outer HTTP server forcibly
   closes the connection). Call `NewInferenceRequest(ctx, ...)` with
   the appropriate `RequestType` — the context is stored on the
   request so workers can respect it at their pre-send and
   post-response checkpoints (R3).
6. Call `batcher.Submit(ctx, req)`. If it returns `ErrQueueFull`,
   respond **HTTP 503**.
7. Block on a `select` over `req.ResultChan` and `ctx.Done()`. On
   `ctx.Done()`, respond **HTTP 504**; the workers will see the
   cancelled context and drop the request silently.
8. If `result.Error != nil`, respond **HTTP 500** with the error
   message.
9. Format result into an OpenAI-compatible response. Respond
   **HTTP 200**.

### Task 5.3 — Inference Handlers

```
Implement internal/api/handlers/inference.go:

Types:
  CompletionRequest  { Prompt, MaxTokens, Priority string }
  CompletionResponse { ID, Object, Created, Choices, Usage }
  EmbeddingRequest   { Input, Priority string }
  EmbeddingResponse  { Object, Data }
  ErrorResponse      { Error: { Message, Type } }

HandleCompletions(b batcher.Batcher, pool *worker.WorkerPool, writeTimeout time.Duration) http.HandlerFunc
  — follows the handler flow above with RequestTypeCompletion.

HandleEmbeddings(b batcher.Batcher, pool *worker.WorkerPool, writeTimeout time.Duration) http.HandlerFunc
  — follows the handler flow above with RequestTypeEmbedding.

Both handlers:
  - Consult pool.AllWorkersUnhealthy() BEFORE creating an InferenceRequest;
    503 if true.
  - Create a per-request context via context.WithTimeout(r.Context(),
    writeTimeout - time.Second), and defer cancel().
  - Pass that ctx as the first argument to NewInferenceRequest so the
    context is carried on the request (req.Ctx) all the way to the worker.
  - Block on select { case <-req.ResultChan: ...; case <-ctx.Done(): 504 }.
  - On ctx.Done(), do NOT attempt to drain req.ResultChan — the worker's
    non-blocking send pattern handles the abandoned-handler case.

parsePriority(s string) models.Priority helper:
  "critical" → PriorityCritical
  "high" → PriorityHigh
  "low" → PriorityLow
  default → PriorityNormal
```

### Task 5.4 — Metrics Handlers

```
Implement internal/api/handlers/metrics.go:

HandlePrometheusMetrics() http.Handler
  — delegates to metrics.PrometheusHandler().

HandleJSONMetrics(collector *metrics.Collector) http.HandlerFunc
  — calls JSONSnapshot, writes application/json response.

HandleMetricsHistory(agg *metrics.TimeSeriesAggregator) http.HandlerFunc
  — reads query params: metric (required), duration (default "60s").
  — calls agg.GetHistory, writes JSON array of DataPoints.
```

### Task 5.5 — Admin Handlers

```
Implement internal/api/handlers/admin.go:

HandleGetConfig(cfg *config.Config) http.HandlerFunc
  — marshals config to JSON.

HandleSetStrategy(batcher batcher.Batcher, cfg *config.BatchingConfig) http.HandlerFunc
  — reads JSON body: { "strategy": "queue_depth" }
  — creates the appropriate Strategy instance from config values
  — calls batcher.SetStrategy(newStrategy)
  — responds 200 with { "strategy": "<name>" }
  — responds 400 if strategy name is invalid

HandleGetWorkers(pool *worker.WorkerPool) http.HandlerFunc
  — calls pool.GetStatus(), writes JSON array.

HandleHealth() http.HandlerFunc
  — returns { "status": "ok" }.
```

---

## WebSocket Handler (Dashboard)

The WebSocket endpoint pushes a `*MetricsSnapshot` to connected clients
at `dashboard.ws_push_interval`. Rather than each connected client
racing to read live collector state under locks, a single background
goroutine — the `SnapshotPublisher` — builds the snapshot once per
interval and publishes it via `atomic.Pointer[MetricsSnapshot]`. Every
WebSocket handler then loads the pointer lock-free and writes it to its
socket.

Benefits:

- **No lock contention** between N connected clients and the collector.
- **No torn reads**: the published snapshot is immutable by contract.
- **Work happens once**, regardless of client count.

```go
type SnapshotPublisher struct {
    current atomic.Pointer[MetricsSnapshot]
}

func NewSnapshotPublisher() *SnapshotPublisher {
    sp := &SnapshotPublisher{}
    empty := MetricsSnapshot{Timestamp: time.Now()}
    sp.current.Store(&empty)
    return sp
}

func (sp *SnapshotPublisher) Start(ctx context.Context, c *metrics.Collector, interval time.Duration) {
    go sp.publishLoop(ctx, c, interval)
}

func (sp *SnapshotPublisher) publishLoop(ctx context.Context, c *metrics.Collector, interval time.Duration) {
    tick := time.NewTicker(interval)
    defer tick.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-tick.C:
            snap := c.BuildSnapshot() // freshly-allocated, immutable
            sp.current.Store(&snap)
        }
    }
}

func (sp *SnapshotPublisher) Current() *MetricsSnapshot {
    return sp.current.Load()
}
```

**Handler loop:**

```
GET /ws — upgrades to WebSocket.

Per-client server-side loop:
  ticker := time.NewTicker(pushInterval)
  for {
      select {
      case <-ticker.C:
          snap := publisher.Current()
          if err := conn.WriteJSON(snap); err != nil { return }
      case <-ctx.Done():
          return
      }
  }

Multiple concurrent clients are supported. Each has its own goroutine
and shares the same published snapshot pointer. A separate read
goroutine per connection detects client disconnect via conn.ReadMessage.
```

**Immutability contract:** the snapshot struct must be fully immutable
once published. Specifically, `Workers []WorkerSnapshot` must be a
freshly allocated slice with its elements copied from the collector's
internal storage (enforced by `Collector.BuildSnapshot` — see
[04-metrics.md Task 4.2](./04-metrics.md#task-42--prometheus-metrics)).
Sharing a slice header that points into mutable backing memory re-
introduces the exact race R8c is meant to eliminate. See
[I6](./shared/invariants.md#i6-snapshot-immutability-r8c).

### Task 5.6 — WebSocket Handler

```
Implement internal/api/handlers/ws.go:

SnapshotPublisher struct with atomic.Pointer[MetricsSnapshot] exactly as
shown above.

NewSnapshotPublisher() *SnapshotPublisher
  - Seeds current to an empty snapshot so Current() never returns nil.

Start(ctx, collector, interval) — launches publishLoop.
publishLoop(ctx, collector, interval)
  - Ticks at interval; calls collector.BuildSnapshot() and stores the
    pointer atomically. Nothing else.
Current() — atomic load; returns the latest snapshot pointer.

HandleWebSocket(publisher *SnapshotPublisher, pushInterval time.Duration) http.HandlerFunc
  - Upgrades via gorilla/websocket.Upgrader (allow all origins for local
    dev).
  - Per-connection write goroutine loops at pushInterval, loads
    publisher.Current(), calls conn.WriteJSON(snap).
  - Per-connection read goroutine calls conn.ReadMessage to detect client
    disconnect; on error, cancels the write goroutine via a per-
    connection context.
  - Cleans up on disconnect or outer context cancellation.

Main.go wiring (Task 5.7): build the SnapshotPublisher during startup,
launch its Start(ctx, collector, cfg.Dashboard.WSPushInterval) alongside
the other background loops, and pass the publisher to HandleWebSocket
when mounting the route.
```

---

## Main Server Entry Point

### Task 5.7 — Main Server Entry Point

```
Create cmd/server/main.go:

 1. Load config via config.Load(). Fatal on error.
 2. Initialize zerolog logger.
 3. Create metrics.Collector(cfg.Metrics.PercentileWindowSize, cfg.Metrics.P99SmoothingAlpha).
    Call collector.LatencyTracker.Start(ctx, 100ms) to launch the async
    P99 materialisation loop (R8a; see I7).
 4. Create metrics.TimeSeriesAggregator (no retention arg — retention is
    implicit in its ring dimensions, see Task 4.3).
 5. Create WorkerPool via worker.NewWorkerPool(cfg.Workers, cfg.Health,
    cfg.Upstream). The pool allocates its own priority-indexed
    batchChans internally (each capacity = cfg.Workers.Count) and
    exposes them via pool.BatchChans().
 6. Create Strategy based on cfg.Batching.Strategy:
      "fixed"         → NewFixedStrategy
      "queue_depth"   → NewQueueDepthStrategy
      "latency_aware" → NewLatencyAwareStrategy(
                          NewQueueDepthStrategy(...),
                          kP=0.05,
                          cfg.Batching.TargetP99Ms)
 7. Create AdaptiveBatcher via batcher.NewAdaptiveBatcher(batchCfg,
    strategy, pool.BatchChans()). Build BatcherConfig from cfg.Batching
    with BatchChanSize = cfg.Workers.Count (per-priority capacity).
 8. Create the SnapshotPublisher:
      publisher := handlers.NewSnapshotPublisher()
 9. Create API Server via api.NewServer(cfg, batcher, pool, collector,
    aggregator, publisher).
10. Use errgroup.WithContext(signalCtx) to run, each in its own goroutine:
      - batcher.Start(ctx)
      - pool.Start(ctx)                     // launches workers + ping loop
      - server.Start(ctx)
      - aggregator.StartDownsampler(ctx)
      - publisher.Start(ctx, collector, cfg.Dashboard.WSPushInterval)   // R8c
      - metrics-push goroutine (ticks every cfg.Metrics.CollectionInterval):
          collector.SetQueueDepth(batcher.QueueDepth())
          collector.SetUpstreamUnhealthy(pool.UpstreamUnhealthy())
          for each worker in pool.GetStatus():
              collector.SetWorkerStatus(w.ID, w.Status)
              collector.SetCircuitState(w.ID, w.CircuitState)
          aggregator.Add("queue_depth", float64(batcher.QueueDepth()))
          aggregator.Add("latency_p99", collector.LatencyTracker.P99())
          aggregator.Add("latency_p99_smoothed", collector.LatencyTracker.P99Smoothed())
          aggregator.Add("throughput_rps", collector.ThroughputRPS())
          // Feed the latency-aware strategy's StrategyMetrics from the
          // SMOOTHED p99, not the raw p99 — see I7.
          batcher.UpdateStrategyMetrics(strategies.StrategyMetrics{
              P99LatencyMs: collector.LatencyTracker.P99Smoothed(),
              TargetP99Ms:  cfg.Batching.TargetP99Ms,
          })
11. Wire SIGINT/SIGTERM via signal.NotifyContext so signalCtx cancels on
    the first signal. After the errgroup returns, log final shutdown.
12. Order of shutdown matters:
      a. signalCtx cancels → every Start(ctx) loop unwinds.
      b. batcher.Start returns first (its drain sends ErrShuttingDown to
         queued requests' ResultChans).
      c. After batcher has returned, main calls pool.CloseBatchChans()
         so workers observe the closed channels and exit their priority-
         select loops. Do NOT close them from inside batcher.Start, as
         closing channels the pool still owns from the batcher side
         would be a layering violation.
      d. Wait for the errgroup to complete.
13. Log clean shutdown.
```

---

## What Phase 5 delivers

At the end of Phase 5, `go run cmd/server/main.go` stands up the full
gateway against any upstream that speaks the bulk batch protocol. All
HTTP routes are live, the WebSocket pushes snapshots, and metrics are
exported to Prometheus. Integration tests (Phase 8) can run against
this binary + a mock upstream from Phase 7.
