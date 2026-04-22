# Adaptive Batching Engine

Adaptive request batching gateway for LLM inference workloads, written in Go.
It accepts OpenAI-compatible requests, groups them with configurable batching
strategies, dispatches homogeneous batches to an upstream bulk endpoint, and
exposes live metrics for operational tuning.

## What It Does

- Dynamic batching strategies: `fixed`, `queue_depth`, `latency_aware`
- QueueMatrix-based packing by priority, request type, and token bucket
- Worker pool with per-worker circuit breaker and upstream health checks
- Real-time observability via Prometheus metrics, JSON snapshots, history API, and WebSocket feed
- Built-in simulator to compare strategy behavior across realistic traffic scenarios

## Prerequisites

- Go `1.25.6` (from `go.mod`)
- Reachable upstream inference bulk endpoint (configured in `config.yaml`)

## Quick Start

```bash
# 1) Install dependencies
make deps

# 2) Build
make build

# 3) Run API server (default: 0.0.0.0:8080)
make run

# 4) In another terminal, run dashboard UI (default: :8081)
make dashboard
```

## Running Components

```bash
# API server
go run ./cmd/server

# Dashboard server (serves dashboard/static)
go run ./cmd/dashboard

# Simulator (all scenarios x all strategies)
go run ./cmd/simulator --scenarios=all --strategies=all --output=table
```

## API Surface

Inference:
- `POST /v1/completions`
- `POST /v1/embeddings`

Metrics:
- `GET /metrics` (Prometheus)
- `GET /metrics/json` (current snapshot)
- `GET /metrics/history` (time series)

Admin and health:
- `GET /admin/config`
- `POST /admin/strategy`
- `GET /admin/workers`
- `GET /health`

Realtime:
- `GET /ws` (dashboard stream)

## Simulation

Supported scenarios:
- `steady_state`
- `ramp_up`
- `spike_test`
- `mixed_priority`
- `long_tail`

Supported strategies:
- `fixed`
- `queue_depth`
- `latency_aware`

CLI examples:

```bash
# Compare only selected scenarios and strategies
go run ./cmd/simulator --scenarios=steady_state,spike_test --strategies=fixed,queue_depth,latency_aware --output=table

# Export CSV
go run ./cmd/simulator --scenarios=all --strategies=all --output=csv > sim.csv

# Export JSON
go run ./cmd/simulator --scenarios=all --strategies=all --output=json > sim.json
```

Comprehensive sample output (all scenarios x all strategies) shows adaptive
strategies materially reducing tail latency and error rate versus fixed windows:

```text
Scenario        Strategy       RPS     P50    P90     P99      ErrorRate  AvgBatch
steady_state    fixed          1107.8  70.00  103.00  264.00   5.58%      3.03
steady_state    queue_depth    1112.0  71.00  105.00  266.00   5.17%      3.03
steady_state    latency_aware  1103.6  71.00  103.00  248.00   5.65%      3.04
ramp_up         fixed          223.4   54.00  173.00  1425.00  0.36%      3.10
ramp_up         queue_depth    223.9   52.00  168.00  1448.00  0.28%      3.06
ramp_up         latency_aware  223.5   44.00  129.00  1099.00  0.30%      2.83
spike_test      fixed          1090.3  50.00  215.00  282.00   28.01%     3.87
spike_test      queue_depth    1078.0  49.00  210.00  292.00   28.23%     3.91
spike_test      latency_aware  1107.7  56.00  245.00  371.00   28.86%     3.88
mixed_priority  fixed          99.6    75.00  272.00  3020.00  0.32%      3.08
mixed_priority  queue_depth    99.4    73.00  263.00  2737.00  0.38%      2.90
mixed_priority  latency_aware  99.7    41.00  71.00   226.00   0.10%      1.86
long_tail       fixed          99.4    78.00  339.00  6071.00  0.45%      2.74
long_tail       queue_depth    99.3    69.00  317.00  5747.00  0.50%      2.58
long_tail       latency_aware  99.5    45.00  174.00  440.00   0.22%      1.81
```

## Configuration

Configuration is loaded from:
1. `config.yaml`
2. Environment variable overrides prefixed with `ABE_`

Examples:

```bash
ABE_SERVER_PORT=9090 ABE_BATCHING_STRATEGY=queue_depth go run ./cmd/server
```

See:
- `config.yaml` for practical defaults
- `docs/config.md` for loading behavior
- `internal/config/config.go` for schema and validation

## Development Commands

```bash
# Unit + integration tests
make test

# Coverage report
make test-coverage

# Vet
make lint

# Benchmarks
make bench
```

## Project Layout

```text
cmd/         # entrypoints: server, simulator, dashboard
internal/    # app internals (api, batcher, worker, metrics, config, models)
simulation/  # workload generators, scenarios, mock upstream, runner
dashboard/   # static dashboard assets
docs/        # docs and technical specs
test/        # integration + benchmark suites
```
