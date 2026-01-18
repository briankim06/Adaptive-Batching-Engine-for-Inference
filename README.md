# Adaptive Batching Engine for LLM Inference

A smart request batching system written in Go that dynamically optimizes the latency/throughput trade-off for LLM inference workloads.

## Features

- **Adaptive Batch Windows**: Dynamically adjust batch timeouts based on queue depth
- **Smart Request Packing**: Group requests by token length, support priority queues
- **Distributed Worker Pool**: Load balancing, health checks, circuit breaking
- **Observable Performance**: Prometheus metrics + real-time dashboard

## Quick Start

```bash
# Install dependencies
go mod download

# Build
make build

# Run the server
./bin/server
# or
make run

# Run tests
make test
```

## API Endpoints

- `POST /v1/completions` - Submit completion request
- `POST /v1/embeddings` - Submit embedding request  
- `GET /metrics` - Prometheus metrics
- `GET /metrics/json` - JSON metrics snapshot
- `GET /admin/config` - Current configuration
- `POST /admin/strategy/{name}` - Switch batching strategy

## Project Structure

```
cmd/           - Application entry points
internal/      - Private application code
  config/      - Configuration loading
  models/      - Data models
  batcher/     - Batching strategies and logic
  worker/      - Worker pool and circuit breaker
  metrics/     - Metrics collection
  api/         - HTTP handlers
simulation/    - Workload simulation
test/          - Integration tests and benchmarks
```

## Running Simulations

```bash
go run ./cmd/simulator --scenarios=steady_state,spike_test --strategies=fixed,queue_depth
```

## Configuration

Configuration can be set via:
1. `config.yaml` file
2. Environment variables (prefixed with app name)

See `config.yaml` for all available options.
