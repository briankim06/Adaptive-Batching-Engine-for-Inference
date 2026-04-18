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

func TestCircuitBreakerStartsClosed(t *testing.T) {
	cb := NewCircuitBreaker(3, 30*time.Second)
	if cb.State() != CircuitClosed {
		t.Fatalf("expected initial state Closed, got %v", cb.State())
	}
	if !cb.CanExecute() {
		t.Fatal("expected CanExecute true when Closed")
	}
}

func TestCircuitBreakerOpensAfterThreshold(t *testing.T) {
	cb := NewCircuitBreaker(3, 30*time.Second)

	cb.RecordFailure()
	cb.RecordFailure()
	if cb.State() != CircuitClosed {
		t.Fatalf("expected Closed after 2 failures, got %v", cb.State())
	}

	cb.RecordFailure()
	if cb.State() != CircuitOpen {
		t.Fatalf("expected Open after 3 failures, got %v", cb.State())
	}
	if cb.CanExecute() {
		t.Fatal("expected CanExecute false when Open and recovery not elapsed")
	}
}

func TestCircuitBreakerSuccessResetsFailures(t *testing.T) {
	cb := NewCircuitBreaker(3, 30*time.Second)
	cb.RecordFailure()
	cb.RecordFailure()
	cb.RecordSuccess()

	// Should need 3 more failures to open, not 1.
	cb.RecordFailure()
	if cb.State() != CircuitClosed {
		t.Fatalf("expected Closed after success reset + 1 failure, got %v", cb.State())
	}
}

func TestCircuitBreakerTransitionsToHalfOpen(t *testing.T) {
	cb := NewCircuitBreaker(1, 10*time.Millisecond)
	cb.RecordFailure()
	if cb.State() != CircuitOpen {
		t.Fatalf("expected Open, got %v", cb.State())
	}

	time.Sleep(15 * time.Millisecond)

	if !cb.CanExecute() {
		t.Fatal("expected CanExecute true after recovery timeout")
	}
	if cb.State() != CircuitHalfOpen {
		t.Fatalf("expected HalfOpen after recovery timeout, got %v", cb.State())
	}
}

func TestCircuitBreakerHalfOpenToClosedOnSuccess(t *testing.T) {
	cb := NewCircuitBreaker(1, 10*time.Millisecond)
	cb.RecordFailure()
	time.Sleep(15 * time.Millisecond)
	cb.CanExecute() // transitions to HalfOpen

	cb.RecordSuccess()
	if cb.State() != CircuitClosed {
		t.Fatalf("expected Closed after HalfOpen success, got %v", cb.State())
	}
}

func TestCircuitBreakerHalfOpenToOpenOnFailure(t *testing.T) {
	cb := NewCircuitBreaker(1, 10*time.Millisecond)
	cb.RecordFailure()
	time.Sleep(15 * time.Millisecond)
	cb.CanExecute() // transitions to HalfOpen

	cb.RecordFailure()
	if cb.State() != CircuitOpen {
		t.Fatalf("expected Open after HalfOpen failure, got %v", cb.State())
	}
}

func TestCircuitBreakerReset(t *testing.T) {
	cb := NewCircuitBreaker(1, 30*time.Second)
	cb.RecordFailure()
	if cb.State() != CircuitOpen {
		t.Fatalf("expected Open, got %v", cb.State())
	}

	cb.Reset()
	if cb.State() != CircuitClosed {
		t.Fatalf("expected Closed after Reset, got %v", cb.State())
	}
	if !cb.CanExecute() {
		t.Fatal("expected CanExecute true after Reset")
	}
}

func TestCircuitBreakerConcurrentSafety(t *testing.T) {
	cb := NewCircuitBreaker(100, time.Millisecond)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				cb.RecordFailure()
				cb.CanExecute()
				cb.RecordSuccess()
				cb.State()
			}
		}()
	}
	wg.Wait()
}

// TestCircuitBreakerPartialFailureScope guards I3: per-request error fields
// in a 200-OK upstream body are application-level, not transport-level —
// they MUST NOT call RecordFailure. Contrast: 5xx responses do.
func TestCircuitBreakerPartialFailureScope(t *testing.T) {
	const rounds = 5

	t.Run("200 with per-request errors keeps breaker closed", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var batch models.Batch
			if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			results := make([]models.RequestResult, len(batch.Requests))
			for i, req := range batch.Requests {
				results[i] = models.RequestResult{
					RequestID: req.ID,
					Error:     errors.New("application-level failure"),
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(results)
		}))
		defer srv.Close()

		cb := NewCircuitBreaker(rounds, time.Hour)
		w := newProcessTestWorker(t, srv.URL, cb)

		for i := 0; i < rounds; i++ {
			req := models.NewInferenceRequest(context.Background(), "p", 4, models.PriorityNormal, models.RequestTypeCompletion)
			batch := models.NewBatch([]*models.InferenceRequest{req}, "test")
			w.process(context.Background(), batch)
			<-req.ResultChan
		}

		if cb.State() != CircuitClosed {
			t.Fatalf("expected breaker Closed after %d partial-failure rounds, got %v", rounds, cb.State())
		}
	})

	t.Run("5xx responses trip breaker", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		cb := NewCircuitBreaker(rounds, time.Hour)
		w := newProcessTestWorker(t, srv.URL, cb)

		for i := 0; i < rounds; i++ {
			req := models.NewInferenceRequest(context.Background(), "p", 4, models.PriorityNormal, models.RequestTypeCompletion)
			batch := models.NewBatch([]*models.InferenceRequest{req}, "test")
			w.process(context.Background(), batch)
			<-req.ResultChan
		}

		if cb.State() != CircuitOpen {
			t.Fatalf("expected breaker Open after %d 5xx rounds, got %v", rounds, cb.State())
		}
	})
}

func newProcessTestWorker(t *testing.T, upstreamURL string, cb *CircuitBreaker) *ProxyWorker {
	t.Helper()
	upCfg := config.UpstreamConfig{
		URL:             upstreamURL,
		RequestTimeout:  5 * time.Second,
		MaxIdleConns:    4,
		MaxConnsPerHost: 4,
		HealthPath:      "/health",
	}
	unhealthy := &atomic.Bool{}
	bufPool := &sync.Pool{New: func() any { return new(bytes.Buffer) }}
	chans := [4]chan *models.Batch{}
	for i := range chans {
		chans[i] = make(chan *models.Batch, 1)
	}
	var recv [4]<-chan *models.Batch
	for i, ch := range chans {
		recv[i] = ch
	}
	return NewProxyWorker(
		WorkerConfig{ID: "worker-i3-scope", MaxBatchTokens: 1024},
		upCfg,
		&http.Client{Timeout: 5 * time.Second},
		cb,
		unhealthy,
		bufPool,
		recv,
	)
}

func TestCircuitStateString(t *testing.T) {
	tests := []struct {
		state    CircuitState
		expected string
	}{
		{CircuitClosed, "closed"},
		{CircuitOpen, "open"},
		{CircuitHalfOpen, "half_open"},
		{CircuitState(99), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.state.String(); got != tt.expected {
			t.Errorf("expected %s, got %s", tt.expected, got)
		}
	}
}
