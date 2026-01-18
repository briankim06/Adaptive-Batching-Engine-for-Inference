package packing

import (
	"sync"

	"github.com/yourname/adaptive-batching-engine/internal/models"
)

type PriorityQueue struct {
	critical  chan *models.InferenceRequest
	high      chan *models.InferenceRequest
	normal    chan *models.InferenceRequest
	low       chan *models.InferenceRequest
	closeOnce sync.Once
}

func NewPriorityQueue(capacity int) *PriorityQueue {
	if capacity <= 0 {
		capacity = 1
	}
	return &PriorityQueue{
		critical: make(chan *models.InferenceRequest, capacity),
		high:     make(chan *models.InferenceRequest, capacity),
		normal:   make(chan *models.InferenceRequest, capacity),
		low:      make(chan *models.InferenceRequest, capacity),
	}
}

func (pq *PriorityQueue) Submit(req *models.InferenceRequest) {
	if req == nil {
		return
	}
	switch req.Priority {
	case models.PriorityCritical:
		pq.critical <- req
	case models.PriorityHigh:
		pq.high <- req
	case models.PriorityNormal:
		pq.normal <- req
	case models.PriorityLow:
		pq.low <- req
	default:
		pq.normal <- req
	}
}

func (pq *PriorityQueue) TryReceive() (*models.InferenceRequest, bool) {
	select {
	case req, ok := <-pq.critical:
		if ok {
			return req, true
		}
	default:
	}
	select {
	case req, ok := <-pq.high:
		if ok {
			return req, true
		}
	default:
	}
	select {
	case req, ok := <-pq.normal:
		if ok {
			return req, true
		}
	default:
	}
	select {
	case req, ok := <-pq.low:
		if ok {
			return req, true
		}
	default:
	}
	return nil, false
}

func (pq *PriorityQueue) Depth() int {
	return len(pq.critical) + len(pq.high) + len(pq.normal) + len(pq.low)
}

func (pq *PriorityQueue) Close() {
	pq.closeOnce.Do(func() {
		close(pq.critical)
		close(pq.high)
		close(pq.normal)
		close(pq.low)
	})
}

func (pq *PriorityQueue) Critical() <-chan *models.InferenceRequest {
	return pq.critical
}

func (pq *PriorityQueue) High() <-chan *models.InferenceRequest {
	return pq.high
}

func (pq *PriorityQueue) Normal() <-chan *models.InferenceRequest {
	return pq.normal
}

func (pq *PriorityQueue) Low() <-chan *models.InferenceRequest {
	return pq.low
}
