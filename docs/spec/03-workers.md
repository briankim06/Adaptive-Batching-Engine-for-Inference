# Phase 3 — Worker Pool

The dispatch side of the pipeline: circuit breaker, the symmetric-puller
`Worker` interface, the `ProxyWorker` that forwards batches to the
upstream inference server, and the pool that owns them all.

Tasks: 3.1 Circuit Breaker · 3.2 Worker Interface · 3.3 Proxy Worker · 3.4 Worker Pool

Shared references:
- [shared/data-models.md](./shared/data-models.md) — `Batch`, `RequestResult`, `Priority`.
- [shared/configuration.md](./shared/configuration.md) — `upstream:` and `health:` sections.
- [shared/invariants.md](./shared/invariants.md) — especially [I1 context propagation](./shared/invariants.md#i1-context-propagation-r3), [I2 non-blocking sends](./shared/invariants.md#i2-non-blocking-resultchan-sends), [I3 breaker scope](./shared/invariants.md#i3-circuit-breaker-scope-r7), [I4 pooled encoding](./shared/invariants.md#i4-pooled-batch-encoding-r1-hardening), [I5 upstream ctx composition](./shared/invariants.md#i5-upstream-context-composition-r1r3-hardening), [I8 priority-split dispatch](./shared/invariants.md#i8-priority-split-dispatch).

Depends on: Phases 1, 2 (consumes `batchChans` produced by the batcher).
Consumed by: Phase 5 (main.go wires pool ↔ batcher; handlers call `AllWorkersUnhealthy`).

---

## Circuit Breaker

The breaker tracks **transport-level outcomes only** — successful
connection to upstream, or a transport error / 5xx response. Application-
level partial failures (an HTTP 200 whose body contains per-request
error fields) are request-level errors and must not trip the breaker.
Mixing the two would let a single worker processing hundreds of
successful requests per second perpetually reset the failure count and
mask a chronically unreachable upstream. Upstream reachability is
tracked separately by the pool's health ping (see Task 3.4).

See [I3](./shared/invariants.md#i3-circuit-breaker-scope-r7).

States: `Closed` → `Open` → `HalfOpen` → `Closed`

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
    failureThreshold int           // from config: health.failure_threshold
    recoveryTimeout  time.Duration // from config: health.recovery_timeout
    mu               sync.RWMutex
}
```

Behavior:

- `RecordSuccess()`: resets `failureCount` to 0. If state was `HalfOpen`,
  transitions to `Closed`. Called only after a complete, non-5xx
  upstream response.
- `RecordFailure()`: increments `failureCount`, records
  `lastFailureTime`. If `failureCount >= failureThreshold`, transitions
  to `Open`. Called only for transport errors and 5xx responses.
- `CanExecute()`: returns true if `Closed`. If `Open` and
  `recoveryTimeout` has elapsed since `lastFailureTime`, transitions to
  `HalfOpen` and returns true. If `HalfOpen`, returns true (allows one
  probe request). Otherwise returns false.
- `State()`: returns current `CircuitState` (read-locked).
- `Reset()`: forces `Closed` state, zeros counters.

All methods are **thread-safe** via `sync.RWMutex`. `CanExecute` takes
a write lock because it may transition state.

### Task 3.1 — Circuit Breaker

```
Implement internal/worker/circuit.go exactly as specified above.

NewCircuitBreaker(failureThreshold int, recoveryTimeout time.Duration)
  constructor.
All five methods: RecordSuccess, RecordFailure, CanExecute, State, Reset.
Thread-safe with sync.RWMutex.

Scope note: callers MUST invoke RecordSuccess/RecordFailure only for
transport-level outcomes (connection errors, 5xx responses, successful
non-5xx responses). Per-request error fields inside a 200 OK body are
request-level errors and must not reach this breaker.
```

---

## Worker Interface (Pull Model)

Workers are **symmetric pullers** over four priority-indexed
`batchChans`. There is no dispatcher goroutine, no load balancer, and
no per-worker inbox: whichever worker finishes its current upstream
round-trip first wins the next batch via a nested-select-with-default
that checks Critical → High → Normal → Low. Backpressure is natural —
when every worker is busy, the priority channels fill, the batcher
blocks on send for the corresponding priority, and pressure eventually
surfaces as 503 at `Submit` time.

```go
// internal/worker/worker.go

type WorkerStatus int

const (
    WorkerIdle WorkerStatus = iota
    WorkerBusy
    WorkerUnhealthy
)

type Worker interface {
    // ID returns a stable identifier like "worker-0".
    ID() string

    // Status returns the worker's current state (Idle / Busy / Unhealthy).
    Status() WorkerStatus

    // Run is the worker's main loop. Reads the next batch via a nested-
    // select-with-default across four priority-indexed channels
    // (Critical > High > Normal > Low), POSTs it to the upstream
    // inference server, and fans out results to each request's
    // ResultChan. Blocks until ctx is cancelled or all four channels
    // are closed. Must be called in a goroutine.
    Run(ctx context.Context) error
}

type WorkerConfig struct {
    ID             string
    MaxBatchTokens int // guard: if batch.TotalTokens() > MaxBatchTokens, fail before POST
}
```

Notes:

- There is no `Submit(batch)` method. The pool passes the four
  priority-indexed `batchChans` into the worker constructor; the worker
  reads from them in priority order.
- There is no per-worker `HealthCheck`. Upstream reachability is one
  concern (tracked by the pool's health-ping goroutine, see Task 3.4)
  and per-worker transport outcomes are another (tracked by the
  worker's circuit breaker) — R7 keeps these signals separate.

### Task 3.2 — Worker Interface

```
Create internal/worker/worker.go with WorkerStatus constants, Worker
interface, and WorkerConfig struct exactly as shown above.
```

---

## Proxy Worker

The `ProxyWorker` forwards a formed batch to the upstream inference
server over HTTP and fans per-request results back to each request's
`ResultChan`. It does not execute any model itself — the Go binary is a
gateway; inference runs on a separate autoscaling GPU cluster behind a
bulk HTTP endpoint.

```go
type ProxyWorker struct {
    config     WorkerConfig
    batchChans [4]<-chan *models.Batch // priority-indexed; see Task 2.7
    httpClient *http.Client
    upstream   config.UpstreamConfig
    circuit    *CircuitBreaker
    unhealthy  *atomic.Bool           // shared with the pool; pool owns the ping
    bufPool    *sync.Pool             // shared with the pool; see Task 3.4
    status     atomic.Int32           // WorkerStatus
    stats      struct {
        batchesProcessed atomic.Int64
        tokensProcessed  atomic.Int64
    }
}
```

**Wire format (v2.1):** synchronous HTTP JSON. The bulk request body is
the serialised batch; the response body is an array of per-request
results. SSE/streaming responses are explicitly out of scope for v2.1
because they restructure the worker's response handling from
"receive → fan out" to "stream → multiplex," which is a larger
undertaking best done once the synchronous path is stable.

**Run loop (priority-ordered receive):**

```go
func (w *ProxyWorker) Run(ctx context.Context) error {
    crit := w.batchChans[models.PriorityCritical]
    high := w.batchChans[models.PriorityHigh]
    norm := w.batchChans[models.PriorityNormal]
    low  := w.batchChans[models.PriorityLow]

    for {
        // Nested select with default, strict priority. Unlike R5's
        // request-level select-roulette concern, here we are racing
        // *already-priority-sorted batches* — at worst two same-priority
        // batches are selected in arbitrary order across workers.
        var batch *models.Batch
        var ok bool
        select {
        case batch, ok = <-crit:
        default:
            select {
            case batch, ok = <-crit:
            case batch, ok = <-high:
            default:
                select {
                case batch, ok = <-crit:
                case batch, ok = <-high:
                case batch, ok = <-norm:
                default:
                    select {
                    case batch, ok = <-crit:
                    case batch, ok = <-high:
                    case batch, ok = <-norm:
                    case batch, ok = <-low:
                    case <-ctx.Done():
                        return nil
                    }
                }
            }
        }
        if !ok {
            return nil // channel closed during shutdown
        }
        w.process(ctx, batch)
    }
}

func (w *ProxyWorker) process(ctx context.Context, batch *models.Batch) {
    w.status.Store(int32(WorkerBusy))
    defer w.status.Store(int32(WorkerIdle))

    if w.unhealthy.Load() || !w.circuit.CanExecute() {
        failBatch(batch, models.ErrWorkerUnavailable)
        return
    }

    // Pre-send scan: drop requests whose ctx is already cancelled.
    scrubbed := filterCancelled(batch)
    if len(scrubbed.Requests) == 0 {
        return
    }

    // Encode into a pooled buffer to avoid per-batch multi-MB allocations
    // under sustained load (R1 hardening; see I4).
    buf := w.bufPool.Get().(*bytes.Buffer)
    buf.Reset()
    defer w.bufPool.Put(buf)
    if err := json.NewEncoder(buf).Encode(scrubbed); err != nil {
        fanOutError(scrubbed, err)
        return
    }

    // Derive an upstream context that cancels (a) on the worker ctx,
    // (b) after upstream.RequestTimeout, OR (c) when every per-request
    // Ctx.Done() has fired — no point in waiting on a response whose
    // every consumer has disappeared. (See I5.)
    reqCtx, cancel := context.WithTimeout(ctx, w.upstream.RequestTimeout)
    defer cancel()
    stopWatch := spawnAllCancelledWatcher(reqCtx, scrubbed.Requests, cancel)
    defer stopWatch()

    httpReq, _ := http.NewRequestWithContext(reqCtx, http.MethodPost, w.upstream.URL, bytes.NewReader(buf.Bytes()))
    httpReq.Header.Set("Content-Type", "application/json")
    httpReq.ContentLength = int64(buf.Len())

    resp, err := w.httpClient.Do(httpReq)
    if err != nil || resp.StatusCode >= 500 {
        w.circuit.RecordFailure()
        fanOutError(scrubbed, models.ErrWorkerUnavailable)
        if resp != nil {
            resp.Body.Close()
        }
        return
    }
    defer resp.Body.Close()
    w.circuit.RecordSuccess()

    results := decodeBatchResponse(resp.Body) // []RequestResult indexed by position
    fanOutResults(scrubbed, results)

    w.stats.batchesProcessed.Add(1)
    w.stats.tokensProcessed.Add(int64(scrubbed.TotalTokens()))
}

// spawnAllCancelledWatcher launches a goroutine that calls cancel() once
// every request's Ctx.Done() has fired. Returns a stop function that the
// caller MUST defer — calling it cleans up the watcher if the happy path
// (upstream response or timeout) returns first.
func spawnAllCancelledWatcher(ctx context.Context, reqs []*models.InferenceRequest, cancel context.CancelFunc) func() {
    stopCh := make(chan struct{})
    go func() {
        for _, r := range reqs {
            select {
            case <-r.Ctx.Done():
                // this one is gone; keep waiting for the rest
            case <-stopCh:
                return // happy-path exit
            case <-ctx.Done():
                return // upstream already cancelled
            }
        }
        // every per-request ctx has fired; upstream work is pure waste
        cancel()
    }()
    return func() { close(stopCh) }
}
```

**Pre-send context scan (R3 checkpoint 1):** `filterCancelled` iterates
`batch.Requests`; for each request where `req.Ctx.Err() != nil`, it
writes a cancellation result to `req.ResultChan` via non-blocking send
and removes it from the outgoing batch. If the scrubbed batch ends up
empty, the upstream call is skipped entirely.

**Post-response context check (R3 checkpoint 2):** inside
`fanOutResults`, before writing each per-request result to
`req.ResultChan`, re-check `req.Ctx.Err()`. If the client disappeared
during the upstream round-trip, drop the result silently (non-blocking
send to a cap-1 buffer that the handler has already abandoned).

**Partial-failure handling:** an HTTP 200 containing some per-request
errors is **not** a circuit-breaker failure. Those per-request errors
are delivered into the corresponding `ResultChan` with `Error`
populated and the breaker records a success for the round-trip. See
[I3](./shared/invariants.md#i3-circuit-breaker-scope-r7).

**Result channel send pattern** (used in both checkpoints):

```go
select {
case req.ResultChan <- result:
default:
    // handler abandoned — drop
}
```

Non-blocking send is required even though `ResultChan` has cap 1,
because the HTTP handler may already have returned without reading
(e.g. a client context timeout fired). See
[I2](./shared/invariants.md#i2-non-blocking-resultchan-sends).

### Task 3.3 — Proxy Worker

```
Implement internal/worker/proxy.go:

ProxyWorker struct as shown above:
  - config: WorkerConfig
  - batchChans: [4]<-chan *models.Batch  (priority-indexed; owned by pool)
  - httpClient: *http.Client             (configured with upstream.MaxIdleConns,
                                           MaxConnsPerHost)
  - upstream: config.UpstreamConfig
  - circuit: *CircuitBreaker
  - unhealthy: *atomic.Bool              (shared with pool)
  - bufPool: *sync.Pool                  (shared with pool; holds *bytes.Buffer)
  - status: atomic.Int32
  - stats: batchesProcessed, tokensProcessed (atomic int64)

NewProxyWorker(cfg WorkerConfig, upstream config.UpstreamConfig,
    httpClient *http.Client, circuit *CircuitBreaker,
    unhealthy *atomic.Bool, bufPool *sync.Pool,
    batchChans [4]<-chan *models.Batch) *ProxyWorker

ID() returns config.ID.
Status() returns WorkerStatus from atomic.Int32.
Run(ctx) implements the priority-ordered pull loop shown above. Exits
  when ctx is cancelled or any batch channel returns !ok (closed).

Helpers (private):
  filterCancelled(batch)        — R3 pre-send scan, returns scrubbed batch
  fanOutResults(batch, results) — re-checks each req.Ctx before sending
  fanOutError(batch, err)       — writes a ResultChan entry carrying err
  failBatch(batch, err)         — alias for fanOutError used in the
                                   unhealthy / circuit-open short-circuit
  decodeBatchResponse(body)     — unmarshals []RequestResult matching the
                                   order of scrubbed.Requests
  spawnAllCancelledWatcher(...) — as shown above; see careful use of the
                                   returned stop function (MUST defer it)

All ResultChan sends are non-blocking per the pattern above.

Request body encoding: always go through the shared sync.Pool of
*bytes.Buffer + json.NewEncoder(buf).Encode. NEVER call json.Marshal
directly — a full 32-request batch with long prompts can be several MB,
and per-batch allocation at steady state will thrash the GC. The buffer
MUST be deferred back to the pool AFTER httpClient.Do returns, because
http.Client reads from the reader passed in the request body throughout
the POST. Set Content-Length explicitly from buf.Len() so upstream
servers that preallocate decode buffers do not have to consume the body
before knowing its size.

Circuit-breaker scope: call circuit.RecordFailure() ONLY for transport
errors (net.Error, context deadline, etc.) and HTTP 5xx responses. An
HTTP 200 whose body carries per-request error fields must call
circuit.RecordSuccess() — those errors are request-level and propagate
via the individual ResultChans.

Out of scope for v2.1: SSE / streaming responses. The synchronous
receive-then-fan-out path is stabilised first.
```

---

## Worker Pool

The `WorkerPool` owns N symmetric `ProxyWorker`s sharing four priority-
indexed `batchChans`, their per-worker `CircuitBreaker`s, the shared
`upstreamUnhealthy` flag, a shared `sync.Pool` of `*bytes.Buffer`
encoders, and the single upstream-health-ping goroutine. There is no
dispatcher and no balancer — the priority-select inside each worker is
the entire dispatch mechanism.

**Why a shared unhealthy flag rather than one per worker:** every
worker points at the same upstream URL, so the answer to "is the
upstream reachable?" is identical for all of them. One ping goroutine,
one flag, read by all workers — cheaper and correct. R7 still holds:
*network health* (upstream reachable?) and *application success* (did
this POST succeed?) remain fully separate signals. The shared flag
covers the former; the per-worker circuit breakers cover the latter.

```go
type WorkerPool struct {
    workers           []Worker
    circuits          map[string]*CircuitBreaker
    batchChans        [4]chan *models.Batch // priority-indexed; created by the pool
    upstream          config.UpstreamConfig
    httpClient        *http.Client
    bufPool           *sync.Pool            // *bytes.Buffer; shared by all workers
    upstreamUnhealthy atomic.Bool
    healthInterval    time.Duration
    failureThreshold  int
}
```

**Startup** (via `Start(ctx)`):

1. Launch each worker's `Run(ctx)` in an errgroup goroutine.
2. Launch the **upstream health-ping** goroutine (single, pool-owned).
3. Block until `ctx.Done()`; then wait for the errgroup.

There is no dispatch loop.

**Upstream health ping (R7 network-health half):**

```go
func (p *WorkerPool) pingLoop(ctx context.Context) {
    tick := time.NewTicker(p.healthInterval)
    defer tick.Stop()
    consecutiveFailures := 0
    url := p.upstream.URL + p.upstream.HealthPath
    for {
        select {
        case <-ctx.Done():
            return
        case <-tick.C:
            ok := ping(ctx, p.httpClient, url)
            if ok {
                p.upstreamUnhealthy.Store(false)
                consecutiveFailures = 0
                continue
            }
            consecutiveFailures++
            if consecutiveFailures >= p.failureThreshold {
                p.upstreamUnhealthy.Store(true)
            }
        }
    }
}
```

The flag flips `true` after `health.failure_threshold` **consecutive**
failures and clears on any success. Per-worker circuit breakers are
*not* touched by this goroutine.

**Helpers exposed to the HTTP layer:**

- `AllWorkersUnhealthy() bool` — returns `true` if
  `upstreamUnhealthy.Load()` is set, or if every per-worker circuit
  breaker is `CircuitOpen`. The inference handler consults this at
  `Submit` time and responds 503 immediately when true, avoiding a
  queued request that would fail anyway.
- `GetStatus() []WorkerInfo` — snapshot of each worker's ID, status,
  circuit state, batches processed, and tokens processed; used by the
  admin endpoint and the dashboard.
- `BatchChans() [4]chan *models.Batch` — exposed so `main.go` can hand
  the same channels to the batcher (send side) while workers hold them
  as receive-only (see Task 3.3 constructor).
- `CloseBatchChans()` — closes all four channels; called by main.go
  during shutdown after the batcher's drain has completed.
- `UpstreamUnhealthy() bool` — trivial accessor on `upstreamUnhealthy`,
  used by the metrics-push loop (Task 5.7).

### Task 3.4 — Worker Pool

```
Implement internal/worker/pool.go:

WorkerPool struct as shown above.

NewWorkerPool(cfg config.WorkerConfig, healthCfg config.HealthConfig,
    upstream config.UpstreamConfig) *WorkerPool
  - Allocates batchChans: for p in 0..3, batchChans[p] = make(chan *models.Batch, cfg.Count).
  - Builds a shared *http.Client with Transport tuned to
      upstream.MaxIdleConns and upstream.MaxConnsPerHost.
  - Builds a shared bufPool:
      &sync.Pool{New: func() any { return new(bytes.Buffer) }}.
  - Creates cfg.Count CircuitBreaker instances keyed by
      "worker-0", "worker-1", ...
  - Creates cfg.Count ProxyWorker instances, each sharing:
      this pool's batchChans (converted to a [4]<-chan at the boundary),
      the shared httpClient,
      the shared bufPool,
      its own CircuitBreaker,
      &p.upstreamUnhealthy.
  - Seeds upstreamUnhealthy to false.

Start(ctx context.Context) error — uses errgroup:
  - Spawns worker.Run(ctx) for every worker.
  - Spawns p.pingLoop(ctx).
  - Returns when ctx is cancelled and all goroutines exit.

AllWorkersUnhealthy() bool
  - Returns true iff upstreamUnhealthy.Load() OR every circuit is Open.

GetStatus() []WorkerInfo
  - Snapshot ID, Status, CircuitState, BatchesProcessed, TokensProcessed.

BatchChans() [4]chan *models.Batch
  - Returns p.batchChans. The batcher keeps the send side; workers hold
    the receive side (constructed via a directional cast).

CloseBatchChans()
  - Closes each of the four batchChans. Main.go calls this during
    shutdown after the batcher's drain has completed so workers observe
    the closed channels and exit their range/select loops.

UpstreamUnhealthy() bool
  - Returns p.upstreamUnhealthy.Load().

Stop(ctx context.Context) — cancel context, await pingLoop + workers.
```

---

## What Phase 3 delivers

At the end of Phase 3, workers can be wired to a batcher's output and
actually POST to an upstream. Integration tests in
[08-testing.md Task 8.5](./08-testing.md#task-85--integration-tests)
depend on this phase being complete.
