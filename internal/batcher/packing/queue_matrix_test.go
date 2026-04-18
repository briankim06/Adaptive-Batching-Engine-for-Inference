package packing

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

// TestQueueMatrixSubmitRoutesToCorrectSubQueue exercises every
// (priority, type, bucket) triple: 4 priorities × 2 types × 6 buckets = 48
// cases. For each triple, a request with matching fields is submitted and
// the matrix's private queues array is inspected to verify only queues[p][rt][b]
// holds the request and every other sub-queue is empty. Guards I8 + I9.
func TestQueueMatrixSubmitRoutesToCorrectSubQueue(t *testing.T) {
	// Token counts chosen to land in each of the 6 buckets per
	// InferenceRequest.TokenBucket(): ≤128, ≤512, ≤1024, ≤2048, ≤4096, >4096.
	bucketTokens := []int{50, 200, 800, 1500, 3000, 5000}
	priorities := []models.Priority{
		models.PriorityLow, models.PriorityNormal, models.PriorityHigh, models.PriorityCritical,
	}
	reqTypes := []models.RequestType{
		models.RequestTypeCompletion, models.RequestTypeEmbedding,
	}

	for _, prio := range priorities {
		for _, rt := range reqTypes {
			for bIdx, tokens := range bucketTokens {
				prio, rt, bIdx, tokens := prio, rt, bIdx, tokens
				t.Run("", func(t *testing.T) {
					qm := NewQueueMatrix(480) // per-queue cap = 30
					req := makeReq(prio, rt, tokens)
					if got := req.TokenBucket(); got != bIdx {
						t.Fatalf("sanity: expected bucket %d, got %d for tokens=%d", bIdx, got, tokens)
					}
					if err := qm.Submit(req); err != nil {
						t.Fatalf("Submit: %v", err)
					}

					pIdx := priorityIndex(prio)
					rtIdx := typeIndex(rt)
					for p := 0; p < numPriorities; p++ {
						for r := 0; r < numRequestTypes; r++ {
							for b := 0; b < numTokenBuckets; b++ {
								got := len(qm.queues[p][r][b])
								want := 0
								if p == pIdx && r == rtIdx && b == bIdx {
									want = 1
								}
								if got != want {
									t.Fatalf("queues[%d][%d][%d] len=%d, want %d (target=[%d][%d][%d])",
										p, r, b, got, want, pIdx, rtIdx, bIdx)
								}
							}
						}
					}
				})
			}
		}
	}
}

// TestQueueMatrixDrainPriorityOrder verifies strict priority draining under
// concurrent submissions: no "select roulette" — every batch returned by
// Drain consists entirely of the highest-priority enqueued requests until
// they're exhausted.
func TestQueueMatrixDrainPriorityOrder(t *testing.T) {
	pairs := []struct {
		name string
		hi   models.Priority
		lo   models.Priority
	}{
		{"critical-vs-low", models.PriorityCritical, models.PriorityLow},
		{"critical-vs-normal", models.PriorityCritical, models.PriorityNormal},
		{"critical-vs-high", models.PriorityCritical, models.PriorityHigh},
		{"high-vs-normal", models.PriorityHigh, models.PriorityNormal},
		{"high-vs-low", models.PriorityHigh, models.PriorityLow},
		{"normal-vs-low", models.PriorityNormal, models.PriorityLow},
	}

	for _, tc := range pairs {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			qm := NewQueueMatrix(480)
			const perPriority = 10

			var wg sync.WaitGroup
			for _, p := range []models.Priority{tc.hi, tc.lo} {
				p := p
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := 0; i < perPriority; i++ {
						if err := qm.Submit(makeReq(p, models.RequestTypeCompletion, 100)); err != nil {
							t.Errorf("Submit(%v): %v", p, err)
							return
						}
					}
				}()
			}
			wg.Wait()

			drainedHi := 0
			for drainedHi < perPriority {
				reqs, prio, _, _ := qm.Drain(100)
				if len(reqs) == 0 {
					t.Fatalf("drain returned empty before exhausting %v", tc.hi)
				}
				if prio != tc.hi {
					t.Fatalf("expected priority %v while drained < %d, got %v (drainedHi=%d)",
						tc.hi, perPriority, prio, drainedHi)
				}
				drainedHi += len(reqs)
			}

			// Next drain must be the lower-priority batch.
			reqs, prio, _, _ := qm.Drain(100)
			if prio != tc.lo {
				t.Fatalf("expected lower priority %v after hi exhausted, got %v", tc.lo, prio)
			}
			if len(reqs) == 0 {
				t.Fatalf("expected lower-priority requests still present, got 0")
			}
		})
	}
}

// TestQueueMatrixDrainHomogeneity: within a single priority, submissions
// to mixed (type, bucket) keys must still yield batches homogeneous in
// both fields.
func TestQueueMatrixDrainHomogeneity(t *testing.T) {
	qm := NewQueueMatrix(480)

	_ = qm.Submit(makeReq(models.PriorityNormal, models.RequestTypeCompletion, 50))   // bucket 0
	_ = qm.Submit(makeReq(models.PriorityNormal, models.RequestTypeEmbedding, 50))    // bucket 0 (embedding)
	_ = qm.Submit(makeReq(models.PriorityNormal, models.RequestTypeCompletion, 3000)) // bucket 4
	_ = qm.Submit(makeReq(models.PriorityNormal, models.RequestTypeCompletion, 50))   // bucket 0

	// Drain 3 successive non-empty batches. Each must be homogeneous.
	for i := 0; i < 3; i++ {
		reqs, prio, rt, bucket := qm.Drain(100)
		if len(reqs) == 0 {
			t.Fatalf("drain %d: unexpected empty result", i)
		}
		if prio != models.PriorityNormal {
			t.Fatalf("drain %d: unexpected priority %v", i, prio)
		}
		for _, r := range reqs {
			if r.RequestType != rt {
				t.Fatalf("drain %d: request type %v != batch type %v", i, r.RequestType, rt)
			}
			if r.TokenBucket() != bucket {
				t.Fatalf("drain %d: request bucket %d != batch bucket %d", i, r.TokenBucket(), bucket)
			}
		}
	}
}

// TestQueueMatrixSubmitFullSubQueue: per-sub-queue isolation. With
// totalCapacity=16 (perQueueCapacity=1), a second submission with the
// same triple returns ErrQueueFull; a submission with a different bucket
// succeeds. Depth reflects only successful submissions.
func TestQueueMatrixSubmitFullSubQueue(t *testing.T) {
	qm := NewQueueMatrix(16) // per-queue cap = 1

	r1 := makeReq(models.PriorityNormal, models.RequestTypeCompletion, 50) // bucket 0
	if err := qm.Submit(r1); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	r2 := makeReq(models.PriorityNormal, models.RequestTypeCompletion, 50) // bucket 0 — same key
	if err := qm.Submit(r2); !errors.Is(err, models.ErrQueueFull) {
		t.Fatalf("expected ErrQueueFull on same-key second submit, got %v", err)
	}

	// Different bucket → different sub-queue → should succeed.
	r3 := makeReq(models.PriorityNormal, models.RequestTypeCompletion, 800) // bucket 2
	if err := qm.Submit(r3); err != nil {
		t.Fatalf("different-bucket submit: expected success, got %v", err)
	}

	// Depth == 2 (first + third), not 3 — ErrQueueFull must not increment.
	if d := qm.Depth(); d != 2 {
		t.Fatalf("expected depth 2, got %d", d)
	}
}

// TestQueueMatrixEventChanNeverLeaks: 1000 concurrent Submit goroutines
// that exit immediately — no goroutines should be blocked in Submit.
func TestQueueMatrixEventChanNeverLeaks(t *testing.T) {
	qm := NewQueueMatrix(4096)

	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	for i := 0; i < 1000; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = qm.Submit(makeReq(models.PriorityNormal, models.RequestTypeCompletion, 100))
		}()
	}
	wg.Wait()

	// Allow the runtime a moment to settle exited goroutines.
	time.Sleep(50 * time.Millisecond)
	current := runtime.NumGoroutine()
	if current > baseline+50 {
		t.Fatalf("goroutine leak suspected: baseline=%d current=%d", baseline, current)
	}
}

// TestQueueMatrixConcurrentSubmitDrain: N producers + 1 consumer. Total
// drained must equal total successfully submitted. No data races under
// -race.
func TestQueueMatrixConcurrentSubmitDrain(t *testing.T) {
	qm := NewQueueMatrix(1600) // per-queue cap = 100

	const producers = 100
	const perProducer = 20

	var submitted int64
	var wg sync.WaitGroup
	stopDrain := make(chan struct{})

	// One consumer goroutine draining continuously.
	var drained int64
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		for {
			select {
			case <-stopDrain:
				// Final drain to pick up stragglers.
				for {
					reqs, _, _, _ := qm.Drain(64)
					if len(reqs) == 0 {
						return
					}
					atomic.AddInt64(&drained, int64(len(reqs)))
				}
			case <-qm.EventChan():
				for {
					reqs, _, _, _ := qm.Drain(64)
					if len(reqs) == 0 {
						break
					}
					atomic.AddInt64(&drained, int64(len(reqs)))
				}
			case <-time.After(5 * time.Millisecond):
				// Timeout poll in case the eventChan was coalesced away.
				reqs, _, _, _ := qm.Drain(64)
				if len(reqs) > 0 {
					atomic.AddInt64(&drained, int64(len(reqs)))
				}
			}
		}
	}()

	for i := 0; i < producers; i++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for j := 0; j < perProducer; j++ {
				p := models.Priority((seed + j) % 4)
				err := qm.Submit(makeReq(p, models.RequestTypeCompletion, 100))
				if err == nil {
					atomic.AddInt64(&submitted, 1)
				}
			}
		}(i)
	}
	wg.Wait()

	// Give the consumer a chance to drain everything before signalling stop.
	time.Sleep(100 * time.Millisecond)
	close(stopDrain)
	<-drainDone

	if atomic.LoadInt64(&drained) != atomic.LoadInt64(&submitted) {
		t.Fatalf("drained=%d submitted=%d — expected equality",
			atomic.LoadInt64(&drained), atomic.LoadInt64(&submitted))
	}
}

// TestQueueMatrixDepthAtomicity: Depth() observations during concurrent
// Submit remain in [0, N] and the final value equals the count of
// successful submissions.
func TestQueueMatrixDepthAtomicity(t *testing.T) {
	const total = 500
	qm := NewQueueMatrix(total * 16) // ample capacity

	var okCount int64
	var wg sync.WaitGroup

	observed := make(chan int64, 1024)
	stopObs := make(chan struct{})

	go func() {
		for {
			select {
			case <-stopObs:
				return
			default:
				observed <- qm.Depth()
			}
		}
	}()

	for i := 0; i < total; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := models.Priority(i % 4)
			if err := qm.Submit(makeReq(p, models.RequestTypeCompletion, 100+i)); err == nil {
				atomic.AddInt64(&okCount, 1)
			}
		}(i)
	}
	wg.Wait()
	close(stopObs)

	// Drain the observation channel and assert every read was in-range.
	for {
		select {
		case v := <-observed:
			if v < 0 || v > total {
				t.Fatalf("observed depth=%d outside [0,%d]", v, total)
			}
		default:
			goto done
		}
	}
done:

	if got := qm.Depth(); got != atomic.LoadInt64(&okCount) {
		t.Fatalf("final depth=%d, expected %d successful submits", got, okCount)
	}
}

// TestQueueMatrixClose: after Close, a non-blocking receive from every
// sub-queue yields ok=false (channel closed).
func TestQueueMatrixClose(t *testing.T) {
	qm := NewQueueMatrix(160)
	qm.Close()

	for p := 0; p < numPriorities; p++ {
		for rt := 0; rt < numRequestTypes; rt++ {
			for b := 0; b < numTokenBuckets; b++ {
				select {
				case _, ok := <-qm.queues[p][rt][b]:
					if ok {
						t.Fatalf("queues[%d][%d][%d] unexpectedly open after Close", p, rt, b)
					}
				default:
					t.Fatalf("queues[%d][%d][%d] non-blocking receive hit default — channel not closed", p, rt, b)
				}
			}
		}
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
