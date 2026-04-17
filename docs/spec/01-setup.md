# Phase 1 — Project Setup

Bootstrap the repository: directory structure, Go module, dependencies,
configuration loader, and core data models.

Tasks: 1.1 Initialize · 1.2 Configuration · 1.3 Core Models

Shared references:
- [shared/data-models.md](./shared/data-models.md) — authoritative type definitions this phase creates.
- [shared/configuration.md](./shared/configuration.md) — authoritative `config.yaml` schema this phase loads.
- [shared/invariants.md](./shared/invariants.md) — cross-cutting contracts (especially [I1 context propagation](./shared/invariants.md#i1-context-propagation-r3)).

---

## Task 1.1 — Initialize Project

```
Create a Go project for "adaptive-batching-engine" with the directory
structure defined in ../README.md. Initialize go.mod with module path
github.com/yourname/adaptive-batching-engine and Go 1.22.

Add dependencies:
  chi v5, viper, prometheus/client_golang, zerolog, uuid,
  golang.org/x/sync, gorilla/websocket.

Create all directories from the project structure.

Create a Makefile with targets:
  build   — go build ./...
  test    — go test ./... -v -race
  run     — go run cmd/server/main.go
  lint    — go vet ./...
  sim     — go run cmd/simulator/main.go

Verify: go build ./... (should produce no output).
```

---

## Task 1.2 — Configuration

The full schema and validation rules are in
[shared/configuration.md](./shared/configuration.md). This task implements
the loader.

```
Create internal/config/config.go.

Use Viper to load from config.yaml with env-var overrides (prefix ABE_,
nested with _, e.g. ABE_BATCHING_STRATEGY).

Define a Config struct with nested structs: ServerConfig, BatchingConfig,
WorkerConfig, UpstreamConfig, HealthConfig, MetricsConfig, DashboardConfig.
Fields and types must match the YAML schema in shared/configuration.md
exactly.

Implement Load() (*Config, error) that:
  - Sets config file name and paths
  - Binds env vars with ABE_ prefix
  - Unmarshals into Config
  - Calls validate() which returns an error if any of the conditions in
    the "Validation rules" section of shared/configuration.md hold.

Verify: go test ./internal/config/... -v -race
```

---

## Task 1.3 — Core Models

The full type definitions live in
[shared/data-models.md](./shared/data-models.md). This task copies those
definitions into source files verbatim.

```
Implement internal/models/request.go, internal/models/batch.go, and
internal/models/errors.go exactly as specified in shared/data-models.md.
Every type, constant, method, and sentinel error must match.

Critical invariant (see shared/invariants.md#i1):
  InferenceRequest carries a context.Context (the HTTP handler's request
  context). NewInferenceRequest takes ctx as its first argument. The Ctx
  field is tagged json:"-" so it is never serialised to the wire.

Verify: go build ./internal/models/... && go test ./internal/models/... -v -race
```

---

## What Phase 1 delivers

At the end of Phase 1, running `go build ./...` compiles cleanly, and
`go test ./internal/config/... ./internal/models/... -v -race` passes.
No runtime behaviour yet — nothing exposes HTTP or dispatches work. Those
come in Phases 2, 3, and 5.

Tests for Phase 1 types are specified in
[08-testing.md Task 8.1](./08-testing.md#task-81--unit-tests-models).
