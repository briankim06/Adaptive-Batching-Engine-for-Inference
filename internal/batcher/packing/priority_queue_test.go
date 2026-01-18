package packing

import (
	"testing"

	"github.com/yourname/adaptive-batching-engine/internal/models"
)

func TestPriorityQueueOrderAndDepth(t *testing.T) {
	pq := NewPriorityQueue(10)

	pq.Submit(&models.InferenceRequest{Priority: models.PriorityNormal})
	pq.Submit(&models.InferenceRequest{Priority: models.PriorityLow})
	pq.Submit(&models.InferenceRequest{Priority: models.PriorityCritical})
	pq.Submit(&models.InferenceRequest{Priority: models.PriorityHigh})

	if pq.Depth() != 4 {
		t.Errorf("expected depth 4, got %d", pq.Depth())
	}

	req, ok := pq.TryReceive()
	if !ok || req.Priority != models.PriorityCritical {
		t.Errorf("expected critical first, got %v (ok=%v)", req, ok)
	}

	req, ok = pq.TryReceive()
	if !ok || req.Priority != models.PriorityHigh {
		t.Errorf("expected high second, got %v (ok=%v)", req, ok)
	}

	req, ok = pq.TryReceive()
	if !ok || req.Priority != models.PriorityNormal {
		t.Errorf("expected normal third, got %v (ok=%v)", req, ok)
	}

	req, ok = pq.TryReceive()
	if !ok || req.Priority != models.PriorityLow {
		t.Errorf("expected low fourth, got %v (ok=%v)", req, ok)
	}

	if pq.Depth() != 0 {
		t.Errorf("expected depth 0, got %d", pq.Depth())
	}

	_, ok = pq.TryReceive()
	if ok {
		t.Error("expected no request after draining queue")
	}
}

func TestPriorityQueueClose(t *testing.T) {
	pq := NewPriorityQueue(1)
	pq.Close()

	req, ok := pq.TryReceive()
	if ok || req != nil {
		t.Errorf("expected empty receive after close, got %v (ok=%v)", req, ok)
	}
}
