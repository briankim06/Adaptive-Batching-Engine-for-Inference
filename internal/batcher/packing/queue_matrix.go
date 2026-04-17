// Package packing provides the batching-layer intake buffers.
//
// The QueueMatrix is the spec-aligned replacement for the earlier
// PriorityQueue+TokenGrouper pair. See docs/spec/02-batching.md §2.5 and
// invariants I8/I9 for background.
package packing

import (
	"sync"
	"sync/atomic"

	"github.com/briankim06/adaptive-batching-engine/internal/models"
)

const (
	// numPriorities matches len(Priority) for Low/Normal/High/Critical.
	numPriorities = 4
	// numRequestTypes matches completion + embedding.
	numRequestTypes = 2
	// numTokenBuckets matches InferenceRequest.TokenBucket() range 0..5.
	numTokenBuckets = 6
)

// QueueMatrix is a pre-segregated intake buffer keyed by
// (priority, requestType, tokenBucket). Because every request lands in the
// exact sub-queue for its traits, any slice drained from a single sub-queue
// is homogeneous by construction.
type QueueMatrix struct {
	queues           [numPriorities][numRequestTypes][numTokenBuckets]chan *models.InferenceRequest
	perQueueCapacity int
	depth            atomic.Int64
	eventChan        chan struct{}

	closeOnce sync.Once
	closed    atomic.Bool
}

// NewQueueMatrix allocates all 48 sub-queues with a per-queue capacity
// derived from totalCapacity (spec: ceil(totalCapacity/16), floor 1).
func NewQueueMatrix(totalCapacity int) *QueueMatrix {
	if totalCapacity < 1 {
		totalCapacity = 1
	}
	per := (totalCapacity + 15) / 16
	if per < 1 {
		per = 1
	}
	qm := &QueueMatrix{
		perQueueCapacity: per,
		eventChan:        make(chan struct{}, 1),
	}
	for p := 0; p < numPriorities; p++ {
		for rt := 0; rt < numRequestTypes; rt++ {
			for b := 0; b < numTokenBuckets; b++ {
				qm.queues[p][rt][b] = make(chan *models.InferenceRequest, per)
			}
		}
	}
	return qm
}

// Submit routes req into its exact sub-queue. On success it increments
// depth and wakes the batcher via a non-blocking send on eventChan.
// Returns models.ErrQueueFull when the target sub-queue is at capacity,
// models.ErrShuttingDown when Close has been called, and
// models.ErrInvalidRequest for a nil request.
func (qm *QueueMatrix) Submit(req *models.InferenceRequest) error {
	if req == nil {
		return models.ErrInvalidRequest
	}
	if qm.closed.Load() {
		return models.ErrShuttingDown
	}
	p := priorityIndex(req.Priority)
	rt := typeIndex(req.RequestType)
	b := bucketIndex(req.TokenBucket())
	ch := qm.queues[p][rt][b]
	select {
	case ch <- req:
		qm.depth.Add(1)
		select {
		case qm.eventChan <- struct{}{}:
		default:
		}
		return nil
	default:
		return models.ErrQueueFull
	}
}

// Drain walks the matrix in strict priority order (Critical → High →
// Normal → Low) and within each priority in (requestType, tokenBucket)
// iteration order. The first non-empty sub-queue is drained up to
// maxBatchSize via non-blocking receives and returned with its coordinates.
func (qm *QueueMatrix) Drain(maxBatchSize int) ([]*models.InferenceRequest, models.Priority, models.RequestType, int) {
	if maxBatchSize <= 0 {
		return nil, models.PriorityNormal, models.RequestTypeCompletion, 0
	}
	for p := numPriorities - 1; p >= 0; p-- {
		for rt := 0; rt < numRequestTypes; rt++ {
			for b := 0; b < numTokenBuckets; b++ {
				ch := qm.queues[p][rt][b]
				if len(ch) == 0 {
					continue
				}
				drained := qm.drainSubqueue(ch, maxBatchSize)
				if len(drained) == 0 {
					continue
				}
				return drained, indexToPriority(p), indexToType(rt), b
			}
		}
	}
	return nil, models.PriorityNormal, models.RequestTypeCompletion, 0
}

// DrainAll empties every sub-queue; used during shutdown so the batcher
// can fan ErrShuttingDown out to every stranded ResultChan.
func (qm *QueueMatrix) DrainAll() []*models.InferenceRequest {
	var out []*models.InferenceRequest
	for p := numPriorities - 1; p >= 0; p-- {
		for rt := 0; rt < numRequestTypes; rt++ {
			for b := 0; b < numTokenBuckets; b++ {
				out = append(out, drainChannel(qm.queues[p][rt][b])...)
			}
		}
	}
	qm.depth.Store(0)
	return out
}

// EventChan exposes the submission wake channel to the batcher's formBatch
// loop. It is buffered with capacity 1 so repeated submits coalesce into a
// single wake.
func (qm *QueueMatrix) EventChan() <-chan struct{} { return qm.eventChan }

// Depth returns the live total count across all sub-queues (O(1)).
func (qm *QueueMatrix) Depth() int64 { return qm.depth.Load() }

// PerQueueCapacity returns the derived capacity of any single sub-queue.
func (qm *QueueMatrix) PerQueueCapacity() int { return qm.perQueueCapacity }

// Close marks the matrix as shutting down; subsequent Submit calls return
// models.ErrShuttingDown. Already-queued requests remain drainable via
// Drain / DrainAll. Close is idempotent.
func (qm *QueueMatrix) Close() {
	qm.closeOnce.Do(func() {
		qm.closed.Store(true)
	})
}

func (qm *QueueMatrix) drainSubqueue(ch chan *models.InferenceRequest, maxBatchSize int) []*models.InferenceRequest {
	drained := make([]*models.InferenceRequest, 0, maxBatchSize)
	for len(drained) < maxBatchSize {
		select {
		case req, ok := <-ch:
			if !ok {
				break
			}
			if req != nil {
				drained = append(drained, req)
			}
		default:
			if n := len(drained); n > 0 {
				qm.depth.Add(-int64(n))
			}
			return drained
		}
	}
	if n := len(drained); n > 0 {
		qm.depth.Add(-int64(n))
	}
	return drained
}

func drainChannel(ch chan *models.InferenceRequest) []*models.InferenceRequest {
	var out []*models.InferenceRequest
	for {
		select {
		case req, ok := <-ch:
			if !ok {
				return out
			}
			if req != nil {
				out = append(out, req)
			}
		default:
			return out
		}
	}
}

func priorityIndex(p models.Priority) int {
	switch p {
	case models.PriorityLow:
		return 0
	case models.PriorityNormal:
		return 1
	case models.PriorityHigh:
		return 2
	case models.PriorityCritical:
		return 3
	default:
		return 1
	}
}

func indexToPriority(i int) models.Priority {
	switch i {
	case 0:
		return models.PriorityLow
	case 2:
		return models.PriorityHigh
	case 3:
		return models.PriorityCritical
	default:
		return models.PriorityNormal
	}
}

func typeIndex(rt models.RequestType) int {
	if rt == models.RequestTypeEmbedding {
		return 1
	}
	return 0
}

func indexToType(i int) models.RequestType {
	if i == 1 {
		return models.RequestTypeEmbedding
	}
	return models.RequestTypeCompletion
}

func bucketIndex(b int) int {
	if b < 0 {
		return 0
	}
	if b >= numTokenBuckets {
		return numTokenBuckets - 1
	}
	return b
}
