package worker

import "context"

// WorkerStatus represents the current state of a worker.
type WorkerStatus int

const (
	WorkerIdle      WorkerStatus = iota
	WorkerBusy
	WorkerUnhealthy
)

func (s WorkerStatus) String() string {
	switch s {
	case WorkerIdle:
		return "idle"
	case WorkerBusy:
		return "busy"
	case WorkerUnhealthy:
		return "unhealthy"
	default:
		return "unknown"
	}
}

// Worker is the pull-model interface. Workers read batches from
// priority-indexed channels — there is no dispatcher. See
// docs/spec/03-workers.md §3.2.
type Worker interface {
	// ID returns a stable identifier like "worker-0".
	ID() string

	// Status returns the worker's current state (Idle / Busy / Unhealthy).
	Status() WorkerStatus

	// Run is the worker's main loop. Blocks until ctx is cancelled or all
	// batch channels are closed. Must be called in a goroutine.
	Run(ctx context.Context) error
}

// WorkerConfig holds per-worker configuration.
type WorkerConfig struct {
	ID             string
	MaxBatchTokens int // guard: if batch.TotalTokens() > MaxBatchTokens, fail before POST
}
