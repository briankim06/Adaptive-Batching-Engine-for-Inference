package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/briankim06/adaptive-batching-engine/internal/config"
	"github.com/briankim06/adaptive-batching-engine/internal/models"
)

func TestNewWorkerPoolCreatesWorkersAndChans(t *testing.T) {
	pool := NewWorkerPool(
		config.WorkerConfig{Count: 3, MaxBatchTokens: 8192},
		config.HealthConfig{FailureThreshold: 3, RecoveryTimeout: 30 * time.Second, CheckInterval: 5 * time.Second},
		config.UpstreamConfig{URL: "http://localhost:0", RequestTimeout: time.Second, MaxIdleConns: 10, MaxConnsPerHost: 10},
	)

	if len(pool.workers) != 3 {
		t.Fatalf("expected 3 workers, got %d", len(pool.workers))
	}
	if len(pool.circuits) != 3 {
		t.Fatalf("expected 3 circuits, got %d", len(pool.circuits))
	}
	for i := range pool.batchChans {
		if cap(pool.batchChans[i]) != 3 {
			t.Fatalf("expected batchChans[%d] capacity 3, got %d", i, cap(pool.batchChans[i]))
		}
	}
}

func TestWorkerPoolBatchChansReturnsOwnedChannels(t *testing.T) {
	pool := NewWorkerPool(
		config.WorkerConfig{Count: 2, MaxBatchTokens: 8192},
		config.HealthConfig{FailureThreshold: 3, RecoveryTimeout: 30 * time.Second, CheckInterval: 5 * time.Second},
		config.UpstreamConfig{URL: "http://localhost:0", RequestTimeout: time.Second},
	)

	chans := pool.BatchChans()
	for i := range chans {
		if chans[i] != pool.batchChans[i] {
			t.Fatalf("BatchChans()[%d] does not match internal channel", i)
		}
	}
}

func TestWorkerPoolAllWorkersUnhealthy(t *testing.T) {
	pool := NewWorkerPool(
		config.WorkerConfig{Count: 2, MaxBatchTokens: 8192},
		config.HealthConfig{FailureThreshold: 1, RecoveryTimeout: time.Hour, CheckInterval: 5 * time.Second},
		config.UpstreamConfig{URL: "http://localhost:0", RequestTimeout: time.Second},
	)

	if pool.AllWorkersUnhealthy() {
		t.Fatal("expected AllWorkersUnhealthy false initially")
	}

	// Setting upstreamUnhealthy directly should cause true.
	pool.upstreamUnhealthy.Store(true)
	if !pool.AllWorkersUnhealthy() {
		t.Fatal("expected AllWorkersUnhealthy true when upstream is unhealthy")
	}

	pool.upstreamUnhealthy.Store(false)
	// Open all circuits.
	for _, cb := range pool.circuits {
		cb.RecordFailure() // threshold=1 opens the circuit
	}
	if !pool.AllWorkersUnhealthy() {
		t.Fatal("expected AllWorkersUnhealthy true when all circuits are Open")
	}

	// Reset one circuit — should become false.
	for _, cb := range pool.circuits {
		cb.Reset()
		break
	}
	if pool.AllWorkersUnhealthy() {
		t.Fatal("expected AllWorkersUnhealthy false when at least one circuit is Closed")
	}
}

func TestWorkerPoolGetStatus(t *testing.T) {
	pool := NewWorkerPool(
		config.WorkerConfig{Count: 2, MaxBatchTokens: 8192},
		config.HealthConfig{FailureThreshold: 3, RecoveryTimeout: 30 * time.Second, CheckInterval: 5 * time.Second},
		config.UpstreamConfig{URL: "http://localhost:0", RequestTimeout: time.Second},
	)

	statuses := pool.GetStatus()
	if len(statuses) != 2 {
		t.Fatalf("expected 2 worker infos, got %d", len(statuses))
	}
	for _, info := range statuses {
		if info.Status != WorkerIdle {
			t.Fatalf("expected idle status, got %v", info.Status)
		}
		if info.CircuitState != CircuitClosed {
			t.Fatalf("expected closed circuit, got %v", info.CircuitState)
		}
	}
}

func TestWorkerPoolUpstreamUnhealthy(t *testing.T) {
	pool := NewWorkerPool(
		config.WorkerConfig{Count: 1, MaxBatchTokens: 8192},
		config.HealthConfig{FailureThreshold: 3, RecoveryTimeout: 30 * time.Second, CheckInterval: 5 * time.Second},
		config.UpstreamConfig{URL: "http://localhost:0", RequestTimeout: time.Second},
	)

	if pool.UpstreamUnhealthy() {
		t.Fatal("expected upstream healthy initially")
	}
	pool.upstreamUnhealthy.Store(true)
	if !pool.UpstreamUnhealthy() {
		t.Fatal("expected upstream unhealthy after Store(true)")
	}
}

func TestWorkerPoolCloseBatchChans(t *testing.T) {
	pool := NewWorkerPool(
		config.WorkerConfig{Count: 1, MaxBatchTokens: 8192},
		config.HealthConfig{FailureThreshold: 3, RecoveryTimeout: 30 * time.Second, CheckInterval: 5 * time.Second},
		config.UpstreamConfig{URL: "http://localhost:0", RequestTimeout: time.Second},
	)

	pool.CloseBatchChans()

	for i, ch := range pool.batchChans {
		select {
		case _, ok := <-ch:
			if ok {
				t.Fatalf("expected batchChans[%d] to be closed", i)
			}
		default:
			t.Fatalf("expected batchChans[%d] to be closed (read should not block)", i)
		}
	}
}

func TestWorkerPoolStartAndShutdown(t *testing.T) {
	results := []models.RequestResult{{Result: "ok"}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(results)
	}))
	defer srv.Close()

	pool := NewWorkerPool(
		config.WorkerConfig{Count: 2, MaxBatchTokens: 8192},
		config.HealthConfig{FailureThreshold: 3, RecoveryTimeout: 30 * time.Second, CheckInterval: 100 * time.Millisecond},
		config.UpstreamConfig{
			URL:             srv.URL,
			RequestTimeout:  5 * time.Second,
			MaxIdleConns:    10,
			MaxConnsPerHost: 10,
			HealthPath:      "/health",
		},
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- pool.Start(ctx)
	}()

	// Submit a batch through the pool's channels.
	req := makeReq(models.PriorityNormal)
	batch := models.NewBatch([]*models.InferenceRequest{req}, "test")
	pool.batchChans[models.PriorityNormal] <- batch

	select {
	case res := <-req.ResultChan:
		if res.Error != nil {
			t.Fatalf("expected no error, got %v", res.Error)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for result")
	}

	// Shutdown.
	cancel()
	pool.CloseBatchChans()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("unexpected pool error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for pool shutdown")
	}
}

func TestWorkerPoolPingLoopSetsUnhealthy(t *testing.T) {
	// Upstream that always returns 500 on health checks.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		// Batch handler — respond OK to avoid worker circuit issues.
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]models.RequestResult{})
	}))
	defer srv.Close()

	pool := NewWorkerPool(
		config.WorkerConfig{Count: 1, MaxBatchTokens: 8192},
		config.HealthConfig{FailureThreshold: 2, RecoveryTimeout: 30 * time.Second, CheckInterval: 20 * time.Millisecond},
		config.UpstreamConfig{
			URL:             srv.URL,
			RequestTimeout:  time.Second,
			MaxIdleConns:    10,
			MaxConnsPerHost: 10,
			HealthPath:      "/health",
		},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go pool.Start(ctx)

	// Wait for enough health check failures to flip the flag.
	deadline := time.After(2 * time.Second)
	for {
		if pool.UpstreamUnhealthy() {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for upstream to be marked unhealthy")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestPingSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if !ping(context.Background(), srv.Client(), srv.URL) {
		t.Fatal("expected ping to succeed on 200")
	}
}

func TestPingFailsOn5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	if ping(context.Background(), srv.Client(), srv.URL) {
		t.Fatal("expected ping to fail on 502")
	}
}
