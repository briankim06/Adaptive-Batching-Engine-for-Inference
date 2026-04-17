# Data Models

Authoritative type definitions for `internal/models/`. Every other phase
imports from here. Implemented as part of [Task 1.3](../01-setup.md#task-13--core-models).

Related invariants: [I1 context propagation](./invariants.md#i1-context-propagation-r3),
[I2 non-blocking ResultChan](./invariants.md#i2-non-blocking-resultchan-sends).

---

## `internal/models/request.go`

```go
type RequestType string

const (
    RequestTypeCompletion RequestType = "completion"
    RequestTypeEmbedding  RequestType = "embedding"
)

type Priority int

const (
    PriorityLow Priority = iota
    PriorityNormal
    PriorityHigh
    PriorityCritical
)

// MaxWaitMs returns the maximum acceptable queuing time for this priority.
func (p Priority) MaxWaitMs() int {
    switch p {
    case PriorityCritical: return 5
    case PriorityHigh:     return 20
    case PriorityNormal:   return 50
    case PriorityLow:      return 100
    default:               return 50
    }
}

type InferenceRequest struct {
    ID              string         `json:"id"`
    Prompt          string         `json:"prompt"`
    MaxTokens       int            `json:"max_tokens"`
    Priority        Priority       `json:"priority"`
    RequestType     RequestType    `json:"request_type"`
    CreatedAt       time.Time      `json:"created_at"`
    EstimatedTokens int            `json:"estimated_tokens"`

    // Ctx is the HTTP handler's request context. Workers inspect Ctx.Err()
    // before and after upstream calls to abort work for clients that have
    // already disconnected or timed out.
    Ctx context.Context `json:"-"`

    // Internal — channel to deliver the result back to the HTTP handler.
    // Always buffered with cap 1 so the worker never blocks on send.
    ResultChan chan *RequestResult `json:"-"`
}

func NewInferenceRequest(ctx context.Context, prompt string, maxTokens int, priority Priority, reqType RequestType) *InferenceRequest {
    return &InferenceRequest{
        ID:              uuid.New().String(),
        Prompt:          prompt,
        MaxTokens:       maxTokens,
        Priority:        priority,
        RequestType:     reqType,
        CreatedAt:       time.Now(),
        EstimatedTokens: len(prompt)/4 + maxTokens,
        Ctx:             ctx,
        ResultChan:      make(chan *RequestResult, 1),
    }
}

func (r *InferenceRequest) AgeMs() float64 {
    return float64(time.Since(r.CreatedAt).Milliseconds())
}

// TokenBucket returns a bucket index (0–5) for grouping similarly-sized
// requests to minimize padding waste.
func (r *InferenceRequest) TokenBucket() int {
    buckets := []int{128, 512, 1024, 2048, 4096}
    for i, threshold := range buckets {
        if r.EstimatedTokens <= threshold {
            return i
        }
    }
    return len(buckets) // bucket 5 = 4096+
}
```

### Token Buckets

| Bucket | Token Range | Index |
|--------|-------------|-------|
| 0      | 0–128       | 0     |
| 1      | 129–512     | 1     |
| 2      | 513–1024    | 2     |
| 3      | 1025–2048   | 3     |
| 4      | 2049–4096   | 4     |
| 5      | 4096+       | 5     |

---

## `internal/models/batch.go`

```go
type Batch struct {
    ID           string              `json:"id"`
    Requests     []*InferenceRequest `json:"requests"`
    CreatedAt    time.Time           `json:"created_at"`
    StrategyUsed string              `json:"strategy_used"`
}

func NewBatch(requests []*InferenceRequest, strategy string) *Batch {
    return &Batch{
        ID:           uuid.New().String(),
        Requests:     requests,
        CreatedAt:    time.Now(),
        StrategyUsed: strategy,
    }
}

func (b *Batch) Size() int { return len(b.Requests) }

func (b *Batch) TotalTokens() int {
    total := 0
    for _, r := range b.Requests {
        total += r.EstimatedTokens
    }
    return total
}

func (b *Batch) MaxPriority() Priority {
    max := PriorityLow
    for _, r := range b.Requests {
        if r.Priority > max {
            max = r.Priority
        }
    }
    return max
}

type BatchResult struct {
    BatchID          string    `json:"batch_id"`
    Results          []any     `json:"results"`
    ProcessingTimeMs float64   `json:"processing_time_ms"`
    WorkerID         string    `json:"worker_id"`
    CompletedAt      time.Time `json:"completed_at"`
    Error            error     `json:"-"`
}

type RequestResult struct {
    RequestID string  `json:"request_id"`
    Result    any     `json:"result"`
    Error     error   `json:"-"`
    LatencyMs float64 `json:"latency_ms"`
}
```

---

## `internal/models/errors.go`

```go
var (
    ErrQueueFull         = errors.New("request queue is full")
    ErrWorkerUnavailable = errors.New("no workers available")
    ErrCircuitOpen       = errors.New("circuit breaker is open")
    ErrBatchTimeout      = errors.New("batch processing timed out")
    ErrInvalidRequest    = errors.New("invalid inference request")
    ErrShuttingDown      = errors.New("server is shutting down")
)
```
