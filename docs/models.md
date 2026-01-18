# Data Models

Core data structures for the Adaptive Batching Engine, defining requests, batches, and error types.

## Overview

The `internal/models` package provides the foundational types for the batching system:
- **InferenceRequest**: Individual inference requests with priority and token estimation
- **Batch**: Collections of requests grouped for processing
- **BatchResult/RequestResult**: Processing outcomes
- **Sentinel Errors**: Standard error types for common failure modes

## Request Types

### RequestType
```go
type RequestType string

const (
    RequestTypeCompletion RequestType = "completion"
    RequestTypeEmbedding  RequestType = "embedding"
)
```

Distinguishes between text completion and embedding requests, enabling type-specific batching strategies.

## Priority System

### Priority
```go
type Priority int

const (
    PriorityLow Priority = iota      // MaxWait: 100ms
    PriorityNormal                   // MaxWait: 50ms
    PriorityHigh                     // MaxWait: 20ms
    PriorityCritical                 // MaxWait: 5ms
)
```

Four-tier priority system with descending wait times. Critical requests flush batches immediately to minimize latency.

**Methods:**
- `MaxWaitMs() int`: Returns maximum batch wait time for the priority level
- `String() string`: Human-readable priority name

## InferenceRequest

Represents a single inference request with tracking metadata.

**Fields:**
- `ID`: Unique identifier (UUID)
- `Prompt`: Input text
- `MaxTokens`: Maximum output tokens
- `Priority`: Request priority level
- `RequestType`: Completion or embedding
- `CreatedAt`: Timestamp for latency tracking
- `EstimatedTokens`: Calculated as `len(prompt)/4 + maxTokens`
- `ResultChan`: Buffered channel for async result delivery

**Constructor:**
```go
NewInferenceRequest(prompt string, maxTokens int, priority Priority, reqType RequestType) *InferenceRequest
```

Auto-generates ID, timestamp, token estimate, and result channel.

**Methods:**
- `AgeMs() float64`: Milliseconds since creation (queue time tracking)
- `TokenBucket() int`: Returns bucket index (0-5) for token-aware batching
  - Buckets: 128, 512, 1024, 2048, 4096, overflow

## Batch

A collection of requests dispatched together to a worker.

**Fields:**
- `ID`: Unique batch identifier
- `Requests`: Slice of inference requests
- `CreatedAt`: Batch formation timestamp
- `StrategyUsed`: Name of batching strategy that created this batch

**Constructor:**
```go
NewBatch(requests []*InferenceRequest, strategy string) *Batch
```

**Methods:**
- `Size() int`: Number of requests in batch
- `TotalTokens() int`: Sum of EstimatedTokens across all requests
- `MaxPriority() Priority`: Highest priority in batch (defaults to Normal if empty)

## Results

### RequestResult
Individual request outcome, sent via `InferenceRequest.ResultChan`.

**Fields:**
- `RequestID`: Corresponding request ID
- `Result`: Inference output (any type)
- `Error`: Processing error (nil on success)
- `LatencyMs`: End-to-end latency

### BatchResult
Batch-level processing outcome from workers.

**Fields:**
- `BatchID`: Corresponding batch ID
- `Results`: Slice of results for all requests
- `ProcessingTimeMs`: Worker processing duration
- `WorkerID`: Identifier of worker that processed the batch
- `CompletedAt`: Completion timestamp
- `Error`: Batch-level error (nil on success)

## Errors

Sentinel errors for common failure scenarios:

| Error | Description |
|-------|-------------|
| `ErrQueueFull` | Request queue at capacity |
| `ErrWorkerUnavailable` | No workers available to process batch |
| `ErrCircuitOpen` | Circuit breaker triggered, rejecting requests |
| `ErrBatchTimeout` | Batch processing exceeded deadline |
| `ErrInvalidRequest` | Malformed or invalid request |
| `ErrShuttingDown` | Server in graceful shutdown |

Use with `errors.Is()` for error checking:
```go
if errors.Is(err, models.ErrQueueFull) {
    // Handle queue overflow
}
```

## Design Decisions

1. **Token Bucketing**: Groups similar-sized requests to minimize padding waste in batches
2. **Priority Channel Pattern**: `ResultChan` enables async request/response without explicit goroutine coordination
3. **Age Tracking**: `CreatedAt` + `AgeMs()` provide queue time visibility for adaptive strategies
4. **Defensive MaxPriority**: Returns `PriorityNormal` for empty batches to avoid zero-value issues
5. **Estimated Tokens**: Simple `len/4 + maxTokens` heuristic avoids tokenizer dependency

## Testing

Comprehensive tests in `models_test.go` cover:
- Priority wait time mapping
- Token bucket assignment edge cases
- Batch aggregation methods
- Constructor auto-population
- Error definitions

Run tests:
```bash
go test ./internal/models -v
```
