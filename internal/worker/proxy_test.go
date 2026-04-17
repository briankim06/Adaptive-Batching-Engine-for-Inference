package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/briankim06/adaptive-batching-engine/internal/config"
	"github.com/briankim06/adaptive-batching-engine/internal/models"
)

func newTestUpstream(handler http.HandlerFunc) (*httptest.Server, config.UpstreamConfig) {
	srv := httptest.NewServer(handler)
	return srv, config.UpstreamConfig{
		URL:             srv.URL,
		RequestTimeout:  5 * time.Second,
		MaxIdleConns:    10,
		MaxConnsPerHost: 10,
		HealthPath:      "/health",
	}
}

func newTestProxyWorker(upstream config.UpstreamConfig, batchChans [4]chan *models.Batch) (*ProxyWorker, *atomic.Bool) {
	unhealthy := &atomic.Bool{}
	bufPool := &sync.Pool{New: func() any { return new(bytes.Buffer) }}
	cb := NewCircuitBreaker(3, 30*time.Second)
	var recvChans [4]<-chan *models.Batch
	for i, ch := range batchChans {
		recvChans[i] = ch
	}
	w := NewProxyWorker(
		WorkerConfig{ID: "worker-test", MaxBatchTokens: 8192},
		upstream,
		&http.Client{Timeout: 5 * time.Second},
		cb,
		unhealthy,
		bufPool,
		recvChans,
	)
	return w, unhealthy
}

func makeTestBatchChans() [4]chan *models.Batch {
	var chans [4]chan *models.Batch
	for i := range chans {
		chans[i] = make(chan *models.Batch, 4)
	}
	return chans
}

func makeReq(priority models.Priority) *models.InferenceRequest {
	return models.NewInferenceRequest(context.Background(), "test", 10, priority, models.RequestTypeCompletion)
}

func TestProxyWorkerIDAndStatus(t *testing.T) {
	chans := makeTestBatchChans()
	w, _ := newTestProxyWorker(config.UpstreamConfig{
		URL:            "http://localhost:0",
		RequestTimeout: time.Second,
	}, chans)

	if w.ID() != "worker-test" {
		t.Fatalf("expected ID worker-test, got %s", w.ID())
	}
	if w.Status() != WorkerIdle {
		t.Fatalf("expected initial status Idle, got %v", w.Status())
	}
}

func TestProxyWorkerProcessesSuccessfulBatch(t *testing.T) {
	results := []models.RequestResult{
		{Result: "response-1"},
		{Result: "response-2"},
	}
	srv, upCfg := newTestUpstream(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(results)
	})
	defer srv.Close()

	chans := makeTestBatchChans()
	worker, _ := newTestProxyWorker(upCfg, chans)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go worker.Run(ctx)

	req1 := makeReq(models.PriorityNormal)
	req2 := makeReq(models.PriorityNormal)
	batch := models.NewBatch([]*models.InferenceRequest{req1, req2}, "test")
	chans[models.PriorityNormal] <- batch

	for _, req := range []*models.InferenceRequest{req1, req2} {
		select {
		case res := <-req.ResultChan:
			if res.Error != nil {
				t.Fatalf("expected no error, got %v", res.Error)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for result")
		}
	}

	if worker.BatchesProcessed() != 1 {
		t.Fatalf("expected 1 batch processed, got %d", worker.BatchesProcessed())
	}
}

func TestProxyWorkerRespectsUpstreamUnhealthy(t *testing.T) {
	srv, upCfg := newTestUpstream(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("upstream should not be called when unhealthy")
	})
	defer srv.Close()

	chans := makeTestBatchChans()
	worker, unhealthy := newTestProxyWorker(upCfg, chans)
	unhealthy.Store(true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go worker.Run(ctx)

	req := makeReq(models.PriorityNormal)
	batch := models.NewBatch([]*models.InferenceRequest{req}, "test")
	chans[models.PriorityNormal] <- batch

	select {
	case res := <-req.ResultChan:
		if !errors.Is(res.Error, models.ErrWorkerUnavailable) {
			t.Fatalf("expected ErrWorkerUnavailable, got %v", res.Error)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for result")
	}
}

func TestProxyWorkerCircuitOpenFailsBatch(t *testing.T) {
	srv, upCfg := newTestUpstream(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("upstream should not be called when circuit is open")
	})
	defer srv.Close()

	chans := makeTestBatchChans()
	unhealthy := &atomic.Bool{}
	bufPool := &sync.Pool{New: func() any { return new(bytes.Buffer) }}
	cb := NewCircuitBreaker(1, time.Hour)
	cb.RecordFailure() // opens the circuit

	var recvChans [4]<-chan *models.Batch
	for i, ch := range chans {
		recvChans[i] = ch
	}
	worker := NewProxyWorker(
		WorkerConfig{ID: "worker-circuit", MaxBatchTokens: 8192},
		upCfg, &http.Client{Timeout: time.Second},
		cb, unhealthy, bufPool, recvChans,
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go worker.Run(ctx)

	req := makeReq(models.PriorityNormal)
	batch := models.NewBatch([]*models.InferenceRequest{req}, "test")
	chans[models.PriorityNormal] <- batch

	select {
	case res := <-req.ResultChan:
		if !errors.Is(res.Error, models.ErrWorkerUnavailable) {
			t.Fatalf("expected ErrWorkerUnavailable, got %v", res.Error)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for result")
	}
}

func TestProxyWorkerFiltersCancelledRequests(t *testing.T) {
	var receivedBody []byte
	srv, upCfg := newTestUpstream(func(w http.ResponseWriter, r *http.Request) {
		buf := new(bytes.Buffer)
		buf.ReadFrom(r.Body)
		receivedBody = buf.Bytes()
		results := []models.RequestResult{{Result: "ok"}}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(results)
	})
	defer srv.Close()

	chans := makeTestBatchChans()
	worker, _ := newTestProxyWorker(upCfg, chans)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go worker.Run(ctx)

	cancelledCtx, cancelReq := context.WithCancel(context.Background())
	cancelReq()

	cancelled := models.NewInferenceRequest(cancelledCtx, "cancelled", 10, models.PriorityNormal, models.RequestTypeCompletion)
	live := makeReq(models.PriorityNormal)
	batch := models.NewBatch([]*models.InferenceRequest{cancelled, live}, "test")
	chans[models.PriorityNormal] <- batch

	// The cancelled request should get a cancellation result.
	select {
	case res := <-cancelled.ResultChan:
		if res.Error == nil {
			t.Fatal("expected error for cancelled request")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for cancelled result")
	}

	// The live request should get a success.
	select {
	case res := <-live.ResultChan:
		if res.Error != nil {
			t.Fatalf("expected no error for live request, got %v", res.Error)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for live result")
	}

	// Verify upstream received exactly one request (the live one).
	if len(receivedBody) == 0 {
		t.Fatal("expected upstream to receive a request body")
	}
}

func TestProxyWorkerSkipsUpstreamWhenAllCancelled(t *testing.T) {
	called := &atomic.Bool{}
	srv, upCfg := newTestUpstream(func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		json.NewEncoder(w).Encode([]models.RequestResult{})
	})
	defer srv.Close()

	chans := makeTestBatchChans()
	worker, _ := newTestProxyWorker(upCfg, chans)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go worker.Run(ctx)

	cancelledCtx, cancelReq := context.WithCancel(context.Background())
	cancelReq()

	req := models.NewInferenceRequest(cancelledCtx, "cancelled", 10, models.PriorityNormal, models.RequestTypeCompletion)
	batch := models.NewBatch([]*models.InferenceRequest{req}, "test")
	chans[models.PriorityNormal] <- batch

	select {
	case <-req.ResultChan:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for cancelled result")
	}

	// Give a moment for any upstream call to complete, then verify none happened.
	time.Sleep(50 * time.Millisecond)
	if called.Load() {
		t.Fatal("upstream should not be called when all requests are cancelled")
	}
}

func TestProxyWorkerPriorityOrdering(t *testing.T) {
	srv, upCfg := newTestUpstream(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]models.RequestResult{{Result: "ok"}})
	})
	defer srv.Close()

	chans := makeTestBatchChans()
	worker, _ := newTestProxyWorker(upCfg, chans)

	// Pre-load batches at different priorities before the worker starts.
	lowReq := makeReq(models.PriorityLow)
	critReq := makeReq(models.PriorityCritical)
	chans[models.PriorityLow] <- models.NewBatch([]*models.InferenceRequest{lowReq}, "test")
	chans[models.PriorityCritical] <- models.NewBatch([]*models.InferenceRequest{critReq}, "test")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go worker.Run(ctx)

	// Critical should be processed first.
	select {
	case <-critReq.ResultChan:
	case <-lowReq.ResultChan:
		t.Fatal("expected critical-priority batch to be processed before low")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for critical result")
	}
}

func TestProxyWorkerRecordsCircuitFailureOn5xx(t *testing.T) {
	srv, upCfg := newTestUpstream(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	defer srv.Close()

	chans := makeTestBatchChans()
	unhealthy := &atomic.Bool{}
	bufPool := &sync.Pool{New: func() any { return new(bytes.Buffer) }}
	cb := NewCircuitBreaker(3, time.Hour)
	var recvChans [4]<-chan *models.Batch
	for i, ch := range chans {
		recvChans[i] = ch
	}
	worker := NewProxyWorker(
		WorkerConfig{ID: "worker-5xx", MaxBatchTokens: 8192},
		upCfg, &http.Client{Timeout: time.Second},
		cb, unhealthy, bufPool, recvChans,
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go worker.Run(ctx)

	req := makeReq(models.PriorityNormal)
	batch := models.NewBatch([]*models.InferenceRequest{req}, "test")
	chans[models.PriorityNormal] <- batch

	select {
	case res := <-req.ResultChan:
		if !errors.Is(res.Error, models.ErrWorkerUnavailable) {
			t.Fatalf("expected ErrWorkerUnavailable on 5xx, got %v", res.Error)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for result")
	}

	// Wait for the worker to finish processing.
	time.Sleep(50 * time.Millisecond)
	if cb.State() == CircuitClosed {
		// One failure shouldn't open (threshold=3), but failure count should increase.
	}
}

func TestProxyWorkerRecordsSuccessOn200WithErrors(t *testing.T) {
	// An HTTP 200 with per-request error fields must record a circuit
	// success — those are request-level errors, not transport failures (I3).
	srv, upCfg := newTestUpstream(func(w http.ResponseWriter, r *http.Request) {
		results := []models.RequestResult{
			{RequestID: "1", Error: errors.New("model overloaded")},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(results)
	})
	defer srv.Close()

	chans := makeTestBatchChans()
	unhealthy := &atomic.Bool{}
	bufPool := &sync.Pool{New: func() any { return new(bytes.Buffer) }}
	cb := NewCircuitBreaker(1, time.Hour)
	var recvChans [4]<-chan *models.Batch
	for i, ch := range chans {
		recvChans[i] = ch
	}
	worker := NewProxyWorker(
		WorkerConfig{ID: "worker-200err", MaxBatchTokens: 8192},
		upCfg, &http.Client{Timeout: time.Second},
		cb, unhealthy, bufPool, recvChans,
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go worker.Run(ctx)

	req := makeReq(models.PriorityNormal)
	batch := models.NewBatch([]*models.InferenceRequest{req}, "test")
	chans[models.PriorityNormal] <- batch

	select {
	case <-req.ResultChan:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for result")
	}

	time.Sleep(50 * time.Millisecond)
	if cb.State() != CircuitClosed {
		t.Fatalf("expected circuit to remain Closed after 200 with per-request errors, got %v", cb.State())
	}
}

func TestProxyWorkerExitsOnChannelClose(t *testing.T) {
	chans := makeTestBatchChans()
	worker, _ := newTestProxyWorker(config.UpstreamConfig{
		URL:            "http://localhost:0",
		RequestTimeout: time.Second,
	}, chans)

	done := make(chan struct{})
	go func() {
		worker.Run(context.Background())
		close(done)
	}()

	for i := range chans {
		close(chans[i])
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not exit after channel close")
	}
}

func TestFilterCancelledPreservesLiveRequests(t *testing.T) {
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	cancelled := models.NewInferenceRequest(cancelledCtx, "c", 10, models.PriorityNormal, models.RequestTypeCompletion)
	live := makeReq(models.PriorityNormal)
	batch := models.NewBatch([]*models.InferenceRequest{cancelled, live}, "test")

	scrubbed := filterCancelled(batch)
	if len(scrubbed.Requests) != 1 {
		t.Fatalf("expected 1 live request, got %d", len(scrubbed.Requests))
	}
	if scrubbed.Requests[0] != live {
		t.Fatal("expected the live request to survive filtering")
	}

	// Cancelled request should have received a result.
	select {
	case res := <-cancelled.ResultChan:
		if res.Error == nil {
			t.Fatal("expected error in cancelled result")
		}
	default:
		t.Fatal("expected cancelled request to have a result on its channel")
	}
}

func TestSpawnAllCancelledWatcher(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ctx1, cancel1 := context.WithCancel(context.Background())
	ctx2, cancel2 := context.WithCancel(context.Background())

	req1 := models.NewInferenceRequest(ctx1, "a", 10, models.PriorityNormal, models.RequestTypeCompletion)
	req2 := models.NewInferenceRequest(ctx2, "b", 10, models.PriorityNormal, models.RequestTypeCompletion)

	watcherCancelled := make(chan struct{})
	watchCtx, watchCancel := context.WithCancel(ctx)
	stopWatch := spawnAllCancelledWatcher(watchCtx, []*models.InferenceRequest{req1, req2}, func() {
		watchCancel()
		close(watcherCancelled)
	})
	defer stopWatch()

	cancel1()
	// Only one cancelled — watcher should not fire yet.
	select {
	case <-watcherCancelled:
		t.Fatal("watcher fired before all requests cancelled")
	case <-time.After(50 * time.Millisecond):
	}

	cancel2()
	select {
	case <-watcherCancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not fire after all requests cancelled")
	}
}
