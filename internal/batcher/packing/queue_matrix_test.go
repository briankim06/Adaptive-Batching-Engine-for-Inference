package packing

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/briankim06/adaptive-batching-engine/internal/models"
)

func makeReq(priority models.Priority, rt models.RequestType, tokens int) *models.InferenceRequest {
	req := models.NewInferenceRequest(context.Background(), "p", 0, priority, rt)
	req.EstimatedTokens = tokens
	return req
}

func TestQueueMatrixPerQueueCapacity(t *testing.T) {
	cases := []struct {
		name          string
		totalCapacity int
		expected      int
	}{
		{"zero clamps to one", 0, 1},
		{"less than 16 rounds up to one", 8, 1},
		{"exact multiple", 160, 10},
		{"rounds up", 17, 2},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			qm := NewQueueMatrix(tc.totalCapacity)
			if got := qm.PerQueueCapacity(); got != tc.expected {
				t.Fatalf("totalCapacity=%d: expected per-queue capacity %d, got %d", tc.totalCapacity, tc.expected, got)
			}
		})
	}
}

func TestQueueMatrixSubmitIncrementsDepthAndWakes(t *testing.T) {
	qm := NewQueueMatrix(160)

	if qm.Depth() != 0 {
		t.Fatalf("expected initial depth 0, got %d", qm.Depth())
	}

	if err := qm.Submit(makeReq(models.PriorityNormal, models.RequestTypeCompletion, 100)); err != nil {
		t.Fatalf("unexpected submit error: %v", err)
	}
	if qm.Depth() != 1 {
		t.Fatalf("expected depth 1, got %d", qm.Depth())
	}
	select {
	case <-qm.EventChan():
	default:
		t.Fatal("expected wake on eventChan after Submit")
	}
}

func TestQueueMatrixEventChanCoalesces(t *testing.T) {
	qm := NewQueueMatrix(160)
	for i := 0; i < 5; i++ {
		if err := qm.Submit(makeReq(models.PriorityNormal, models.RequestTypeCompletion, 100)); err != nil {
			t.Fatalf("unexpected submit error on %d: %v", i, err)
		}
	}
	// First receive returns, second must miss — eventChan is cap 1.
	select {
	case <-qm.EventChan():
	default:
		t.Fatal("expected at least one wake")
	}
	select {
	case <-qm.EventChan():
		t.Fatal("expected coalesced wakes")
	default:
	}
}

func TestQueueMatrixSubmitFullReturnsQueueFull(t *testing.T) {
	qm := NewQueueMatrix(1) // per-queue capacity = 1

	if err := qm.Submit(makeReq(models.PriorityNormal, models.RequestTypeCompletion, 100)); err != nil {
		t.Fatalf("unexpected submit error: %v", err)
	}
	// Same (priority, type, bucket) triplet — the second Submit fills.
	err := qm.Submit(makeReq(models.PriorityNormal, models.RequestTypeCompletion, 100))
	if !errors.Is(err, models.ErrQueueFull) {
		t.Fatalf("expected ErrQueueFull, got %v", err)
	}
	if qm.Depth() != 1 {
		t.Fatalf("depth must not change on full submit; got %d", qm.Depth())
	}
}

func TestQueueMatrixDrainsCriticalFirst(t *testing.T) {
	qm := NewQueueMatrix(160)

	_ = qm.Submit(makeReq(models.PriorityLow, models.RequestTypeCompletion, 100))
	_ = qm.Submit(makeReq(models.PriorityNormal, models.RequestTypeCompletion, 100))
	critical := makeReq(models.PriorityCritical, models.RequestTypeCompletion, 100)
	_ = qm.Submit(critical)

	reqs, prio, rt, _ := qm.Drain(4)
	if len(reqs) != 1 {
		t.Fatalf("expected only critical bucket to drain, got %d", len(reqs))
	}
	if reqs[0] != critical {
		t.Fatal("expected drained request to be the critical submission")
	}
	if prio != models.PriorityCritical {
		t.Fatalf("expected critical priority, got %v", prio)
	}
	if rt != models.RequestTypeCompletion {
		t.Fatalf("expected completion type, got %v", rt)
	}
}

func TestQueueMatrixDrainHomogeneousByBucket(t *testing.T) {
	qm := NewQueueMatrix(160)

	// Two requests in the same bucket, one in a different bucket.
	_ = qm.Submit(makeReq(models.PriorityNormal, models.RequestTypeCompletion, 100)) // bucket 0
	_ = qm.Submit(makeReq(models.PriorityNormal, models.RequestTypeCompletion, 120)) // bucket 0
	_ = qm.Submit(makeReq(models.PriorityNormal, models.RequestTypeCompletion, 800)) // bucket 2

	reqs, _, _, bucket := qm.Drain(4)
	if len(reqs) != 2 {
		t.Fatalf("expected bucket-homogeneous drain of 2, got %d", len(reqs))
	}
	if bucket != 0 {
		t.Fatalf("expected bucket 0, got %d", bucket)
	}
}

func TestQueueMatrixDrainRespectsMaxBatchSize(t *testing.T) {
	qm := NewQueueMatrix(160)
	for i := 0; i < 5; i++ {
		_ = qm.Submit(makeReq(models.PriorityNormal, models.RequestTypeCompletion, 100))
	}

	reqs, _, _, _ := qm.Drain(3)
	if len(reqs) != 3 {
		t.Fatalf("expected drain capped at maxBatchSize=3, got %d", len(reqs))
	}
	if qm.Depth() != 2 {
		t.Fatalf("expected depth 2 after partial drain, got %d", qm.Depth())
	}
}

func TestQueueMatrixDrainEmpty(t *testing.T) {
	qm := NewQueueMatrix(160)
	reqs, _, _, _ := qm.Drain(4)
	if len(reqs) != 0 {
		t.Fatalf("expected empty drain, got %d", len(reqs))
	}
}

func TestQueueMatrixSubmitAfterCloseErrs(t *testing.T) {
	qm := NewQueueMatrix(160)
	qm.Close()

	err := qm.Submit(makeReq(models.PriorityNormal, models.RequestTypeCompletion, 100))
	if !errors.Is(err, models.ErrShuttingDown) {
		t.Fatalf("expected ErrShuttingDown after Close, got %v", err)
	}
}

func TestQueueMatrixDrainAllRecoversStranded(t *testing.T) {
	qm := NewQueueMatrix(160)
	for i := 0; i < 3; i++ {
		_ = qm.Submit(makeReq(models.PriorityNormal, models.RequestTypeCompletion, 100))
	}
	_ = qm.Submit(makeReq(models.PriorityHigh, models.RequestTypeEmbedding, 2000))

	all := qm.DrainAll()
	if len(all) != 4 {
		t.Fatalf("expected DrainAll to return 4 requests, got %d", len(all))
	}
	if qm.Depth() != 0 {
		t.Fatalf("expected depth 0 after DrainAll, got %d", qm.Depth())
	}
}

func TestQueueMatrixConcurrentSubmit(t *testing.T) {
	// totalCapacity=1024 ⇒ per-sub-queue cap=64. Load is spread across
	// four priorities so no single sub-queue overflows; the aggregate
	// depth must equal the number of successful Submits.
	qm := NewQueueMatrix(1024)
	const workersPerPriority = 2
	const perWorker = 16

	priorities := []models.Priority{
		models.PriorityLow,
		models.PriorityNormal,
		models.PriorityHigh,
		models.PriorityCritical,
	}

	var wg sync.WaitGroup
	for _, p := range priorities {
		for w := 0; w < workersPerPriority; w++ {
			wg.Add(1)
			go func(p models.Priority) {
				defer wg.Done()
				for i := 0; i < perWorker; i++ {
					if err := qm.Submit(makeReq(p, models.RequestTypeCompletion, 100)); err != nil {
						t.Errorf("unexpected submit error: %v", err)
						return
					}
				}
			}(p)
		}
	}
	wg.Wait()

	expected := len(priorities) * workersPerPriority * perWorker
	if int(qm.Depth()) != expected {
		t.Fatalf("expected depth %d, got %d", expected, qm.Depth())
	}
}
