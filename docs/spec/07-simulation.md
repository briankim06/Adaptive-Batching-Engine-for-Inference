# Phase 7 — Simulation

Load-testing harness: workload generators, predefined scenarios, an
in-process mock upstream that lets the real `ProxyWorker` be exercised
over loopback HTTP, and a CLI that runs every strategy × scenario
combination and prints a comparison table.

Tasks: 7.1 Workload Generator · 7.2 Test Scenarios · 7.3 Simulation Runner · 7.4 Comparison CLI

Shared references:
- [shared/data-models.md](./shared/data-models.md) — `InferenceRequest`, `Priority`, `RequestType`.
- [shared/configuration.md](./shared/configuration.md) — the `upstream:` section; the Runner overwrites `Upstream.URL` at runtime.
- [shared/invariants.md](./shared/invariants.md) — especially [I1 context propagation](./shared/invariants.md#i1-context-propagation-r3). Every generated request carries the Runner's context so the simulator exercises the same code path as production HTTP.

Depends on: Phases 1–5.

---

## Workload Generators

```go
type Generator interface {
    // Generate returns a channel that emits InferenceRequests at the
    // configured rate. Closes the channel when ctx is cancelled or the
    // generator's duration expires.
    Generate(ctx context.Context) <-chan *models.InferenceRequest
}
```

Implementations:

- **ConstantRateGenerator**: fixed RPS using `time.Ticker`. Inter-
  arrival time = `1s / rps`.
- **PoissonGenerator**: exponential inter-arrival times with mean =
  `1s / rps`. Uses `-math.Log(rand.Float64()) / rps` for each interval.
- **RampGenerator**: linearly increasing RPS from `startRPS` to
  `endRPS` over `duration`. Recalculates interval each tick.
- **BurstyGenerator**: base rate with periodic bursts. Emits at
  `baseRPS` for `burstInterval`, then emits
  `burstMultiplier * baseRPS` for `burstDuration`.

All generators use a `GeneratorConfig` controlling:

- `RequestType` distribution (default: 90% completion, 10% embedding)
- `Priority` distribution (default: 80% normal, 15% high, 5% critical)
- Token length distribution: `meanTokens`, `stddevTokens` (normal
  distribution, clamped to `[1, 8192]`)

### Task 7.1 — Workload Generator

```
Implement simulation/workload.go:

Generator interface as shown above.
GeneratorConfig struct with RPS params, RequestType distribution,
Priority distribution, token length params.

ConstantRateGenerator, PoissonGenerator, RampGenerator, BurstyGenerator —
each implementing Generator.

Helper: randomRequest(ctx context.Context, cfg GeneratorConfig) *models.InferenceRequest
  - Picks RequestType and Priority from configured distributions.
  - Generates a prompt of random words sized to the configured token
    distribution.
  - Calls NewInferenceRequest(ctx, prompt, maxTokens, priority, reqType).
    The Runner passes its own per-request context (typically derived
    from the scenario's overall context with an individual timeout) so
    that simulated requests behave identically to real HTTP requests
    under the gateway's context-propagation rules (R3).
```

---

## Predefined Scenarios

| Scenario       | Generator     | Config                                              | Duration |
|----------------|---------------|-----------------------------------------------------|----------|
| steady_state   | ConstantRate  | 1200 RPS                                            | 60s      |
| ramp_up        | Ramp          | 10 → 500 RPS                                        | 60s      |
| spike_test     | Bursty        | 1200 RPS base, 10× burst at 30s for 5s              | 60s      |
| mixed_priority | ConstantRate  | 100 RPS, 80%/15%/5% priority split                  | 60s      |
| long_tail      | ConstantRate  | 100 RPS, bimodal tokens: 70% ~100tok, 30% ~3000tok  | 60s      |

### Task 7.2 — Test Scenarios

```
Implement simulation/scenarios.go:

Scenario struct with Name string, Generator Generator, Duration time.Duration.

Functions returning Scenario:
  SteadyState()     — constant 1200 RPS, 60s (sized to ~80–90% of mock upstream batched capacity)
  RampUp()          — ramp 10→500 RPS over 60s
  SpikeTest()       — bursty: 1200 RPS base, 10x burst at t=30s for 5s
  MixedPriority()   — constant 100 RPS, priority split 80/15/5
  LongTail()        — constant 100 RPS, bimodal token distribution

AllScenarios() []Scenario — returns all five.
ScenarioByName(name string) (Scenario, error) — lookup by name.
```

---

## Simulation Runner

The Runner wires up a batcher, worker pool, and collector just like
`cmd/server/main.go`, but replaces the HTTP handlers with a `Generator`
and replaces the real upstream inference server with an **in-process
mock upstream**. This keeps the production code path — `ProxyWorker`
POSTing bulk JSON over HTTP — byte-for-byte identical to deployment;
only the target `upstream.url` is swapped for a `httptest.Server` URL.
No dev-only `SimulatedWorker` exists; there is one worker
implementation, and the simulator exercises it through real HTTP
sockets on the loopback interface.

```go
type MockUpstream struct {
    server           *httptest.Server
    baseLatencyMs    float64
    perTokenLatencyMs float64
    latencyVariance  float64
    failureRate      float64 // 0.0–1.0: chance of 503 per batch
}
```

`MockUpstream`:

- Exposes `POST /v1/batch` and `GET /health`.
- `/v1/batch` decodes the incoming batch body, sleeps for
  `baseLatencyMs + perTokenLatencyMs * totalTokens + N(0, baseLatencyMs*variance)`
  (clamped to ≥ 1 ms), then responds 200 with a `[]RequestResult`
  containing mock completion strings or random-float embeddings. With
  probability `failureRate`, it responds 503 instead — exercising the
  circuit-breaker path end-to-end.
- `/health` responds 200 unconditionally (or 500 if
  `failureRate == 1.0`, so that scenarios that deliberately break the
  upstream also exercise the pool's health-ping flip).
- `URL()` returns the `httptest.Server`'s URL for the Runner to place
  into `cfg.Upstream.URL`.

```go
type SimulationResult struct {
    ScenarioName  string        `json:"scenario"`
    StrategyName  string        `json:"strategy"`
    Duration      time.Duration `json:"duration"`
    TotalRequests int64         `json:"total_requests"`
    TotalErrors   int64         `json:"total_errors"`
    ErrorRate     float64       `json:"error_rate"`
    ThroughputRPS float64       `json:"throughput_rps"`
    LatencyP50Ms  float64       `json:"latency_p50_ms"`
    LatencyP90Ms  float64       `json:"latency_p90_ms"`
    LatencyP99Ms  float64       `json:"latency_p99_ms"`
    AvgBatchSize  float64       `json:"avg_batch_size"`
}
```

### Task 7.3 — Simulation Runner

```
Implement simulation/runner.go and simulation/mock_upstream.go.

MockUpstream struct as shown above. Constructor:
  NewMockUpstream(cfg MockUpstreamConfig) *MockUpstream
    - Starts an httptest.NewServer wrapping a mux that handles
      POST /v1/batch and GET /health with the behaviour described above.
  URL() string             — returns server.URL
  HealthPath() string      — returns "/health"
  Close()                  — calls server.Close()

SimulationResult struct as shown above.

Runner struct with config *config.Config.
NewRunner(cfg *config.Config) *Runner.

Run(ctx context.Context, scenario Scenario, strategy strategies.Strategy,
    mockCfg MockUpstreamConfig) SimulationResult:

  1. Start a MockUpstream; defer mock.Close().
  2. Clone cfg and overwrite cfg.Upstream.URL = mock.URL() + "/v1/batch",
     cfg.Upstream.HealthPath = "/health". Use short timeouts suitable
     for the scenario (e.g. 5s request_timeout).
  3. Create collector (and start its LatencyTracker materialiser).
  4. Create worker.WorkerPool via NewWorkerPool(cfg.Workers, cfg.Health,
     cfg.Upstream).
  5. Create AdaptiveBatcher via NewAdaptiveBatcher(batchCfg, strategy,
     pool.BatchChans()).
  6. Launch batcher.Start and pool.Start in an errgroup under a context
     that is cancelled when the scenario duration elapses.
  7. For each request from scenario.Generator.Generate(runCtx):
       - batcher.Submit(req.Ctx, req).
       - Spawn a result-collector goroutine per request that reads
         req.ResultChan, records latency/error into the collector, and
         exits.
  8. When scenario.Duration elapses, cancel runCtx, close the pool's
     priority channels (via pool.CloseBatchChans()), wait for the errgroup.
  9. Build a SimulationResult from the collector's counters and
     percentile atomics.

Every request generated by the workload carries a context derived from
runCtx (see Task 7.1); ProxyWorker's pre-send scan and post-response
check exercise the same code path as under production HTTP load.
```

---

## Comparison CLI

### Task 7.4 — Comparison CLI

```
Create cmd/simulator/main.go:

Flags:
  --scenarios  comma-separated list (default: "all")
  --strategies comma-separated list (default: "all")
  --config     path to config.yaml (default: "config.yaml")
  --output     "table" | "csv" | "json" (default: "table")

Behavior:
  1. Load config.
  2. Resolve scenario and strategy lists ("all" expands to all known).
  3. Run every strategy × scenario combination via Runner.
  4. Collect []SimulationResult.
  5. Output:
     - "table": aligned ASCII table to stdout with columns:
       Scenario | Strategy | RPS | P50 | P90 | P99 | ErrorRate | AvgBatch
     - "csv": CSV with header row
     - "json": JSON array of SimulationResult

Example invocation:
  go run cmd/simulator/main.go --scenarios=steady_state,spike_test \
      --strategies=fixed,queue_depth --output=table
```

---

## What Phase 7 delivers

A runnable simulator binary that drives load through the real gateway
against an in-process mock upstream, producing a strategy-comparison
matrix. No production code depends on this phase — it is a sibling to
`cmd/server`.
