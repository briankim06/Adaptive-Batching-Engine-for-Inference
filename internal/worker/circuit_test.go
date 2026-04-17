package worker

import (
	"sync"
	"testing"
	"time"
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
