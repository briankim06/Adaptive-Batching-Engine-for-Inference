package worker

import (
	"sync"
	"time"
)

// CircuitState represents the three states of the circuit breaker.
type CircuitState int

const (
	CircuitClosed   CircuitState = iota
	CircuitOpen
	CircuitHalfOpen
)

func (s CircuitState) String() string {
	switch s {
	case CircuitClosed:
		return "closed"
	case CircuitOpen:
		return "open"
	case CircuitHalfOpen:
		return "half_open"
	default:
		return "unknown"
	}
}

// CircuitBreaker tracks transport-level outcomes only — connection errors
// and 5xx responses trip the breaker; per-request error fields inside a
// 200 OK body are request-level and must never reach here. See invariant I3.
type CircuitBreaker struct {
	state            CircuitState
	failureCount     int
	lastFailureTime  time.Time
	failureThreshold int
	recoveryTimeout  time.Duration
	mu               sync.RWMutex
}

// NewCircuitBreaker creates a breaker that opens after failureThreshold
// consecutive transport failures and probes again after recoveryTimeout.
func NewCircuitBreaker(failureThreshold int, recoveryTimeout time.Duration) *CircuitBreaker {
	return &CircuitBreaker{
		state:            CircuitClosed,
		failureThreshold: failureThreshold,
		recoveryTimeout:  recoveryTimeout,
	}
}

// RecordSuccess resets the failure count. If the breaker was HalfOpen,
// transitions to Closed. Called only after a complete, non-5xx upstream
// response.
func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.failureCount = 0
	if cb.state == CircuitHalfOpen {
		cb.state = CircuitClosed
	}
}

// RecordFailure increments the failure count and records the time. If the
// count reaches the threshold, transitions to Open. Called only for
// transport errors and 5xx responses.
func (cb *CircuitBreaker) RecordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.failureCount++
	cb.lastFailureTime = time.Now()
	if cb.failureCount >= cb.failureThreshold {
		cb.state = CircuitOpen
	}
}

// CanExecute returns true if the breaker allows a request through.
// Closed → always true. Open → true only if recoveryTimeout has elapsed
// (transitions to HalfOpen). HalfOpen → true (allows one probe).
// Takes a write lock because it may transition state.
func (cb *CircuitBreaker) CanExecute() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	switch cb.state {
	case CircuitClosed:
		return true
	case CircuitOpen:
		if time.Since(cb.lastFailureTime) >= cb.recoveryTimeout {
			cb.state = CircuitHalfOpen
			return true
		}
		return false
	case CircuitHalfOpen:
		return true
	default:
		return false
	}
}

// State returns the current circuit state (read-locked).
func (cb *CircuitBreaker) State() CircuitState {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.state
}

// Reset forces the breaker to Closed and zeros counters.
func (cb *CircuitBreaker) Reset() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.state = CircuitClosed
	cb.failureCount = 0
	cb.lastFailureTime = time.Time{}
}
