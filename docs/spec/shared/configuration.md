# Configuration

The authoritative `config.yaml` schema. Loaded via Viper at server startup
and validated before any other component is instantiated.

All tunables are centralized in `config.yaml` and overridable via
environment variables (prefix `ABE_`, nested with `_`, e.g.
`ABE_BATCHING_STRATEGY`).

Implementation lives in `internal/config/config.go`; see
[Task 1.2](../01-setup.md#task-12--configuration).

---

## Schema

```yaml
server:
  host: "0.0.0.0"
  port: 8080
  read_timeout: 30s
  write_timeout: 30s

batching:
  strategy: "latency_aware"          # fixed | queue_depth | latency_aware
  min_batch_size: 1
  max_batch_size: 32
  min_wait_ms: 5
  max_wait_ms: 40                    # dominant-latency knob for fixed; latency_aware scales this
  queue_depth_low_threshold: 8       # sized to typical steady-state depth so queue_depth actually engages
  queue_depth_high_threshold: 80
  queue_capacity: 200000             # total across the QueueMatrix; per-sub-queue cap is derived
  priority_enabled: true
  target_p99_ms: 60.0                # setpoint for latency_aware; steady-state is expected to sit near this

workers:
  count: 8
  max_batch_tokens: 8192             # upper bound before forwarding upstream

upstream:
  url: "http://inference-server:8000/v1/batch"
  request_timeout: 5s
  max_idle_conns: 100
  max_conns_per_host: 24
  health_path: "/health"

health:
  check_interval: 5s
  failure_threshold: 3
  recovery_timeout: 30s

metrics:
  collection_interval: 100ms
  percentile_window_size: 1000
  history_retention: 1h
  p99_smoothing_alpha: 0.3            # EWMA alpha fed to latency_aware strategy

dashboard:
  enabled: true
  port: 8081
  ws_push_interval: 500ms             # how often to push snapshots
```

---

## Validation rules

`Load()` must return an error if any of these hold:

- `workers.count < 1`
- `batching.max_batch_size < batching.min_batch_size`
- `batching.queue_capacity < 1`
- `health.failure_threshold < 1`
- `batching.strategy` is not one of `fixed`, `queue_depth`,
  `latency_aware`
- `upstream.url` is empty or not a well-formed `http(s)://` URL
- `upstream.request_timeout <= 0`
- `metrics.p99_smoothing_alpha` is outside `(0, 1]`

---

## Keys intentionally removed (v2 design history)

These keys existed in the v1 simulator-first design and have been deleted.
Do not reintroduce them:

| Removed key | Reason |
|-------------|--------|
| `workers.balancer`                                       | No dispatcher; pull model (see [03-workers.md](../03-workers.md) and [I8](./invariants.md#i8-priority-split-dispatch)) |
| `workers.base_latency_ms`, `per_token_latency_ms`, `latency_variance`, `failure_rate` | Simulation-only knobs; live upstream replaces simulated GPU sleep |
| `batching.token_bucketing_enabled`                       | Always on — segregation happens at `QueueMatrix.Submit` (see [I9](./invariants.md#i9-token-bucketing-is-always-on)) |
