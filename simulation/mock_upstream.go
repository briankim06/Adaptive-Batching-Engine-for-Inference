package simulation

import (
	"encoding/json"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/briankim06/adaptive-batching-engine/internal/models"
)

// MockUpstreamConfig parameterises the in-process mock inference server.
// Latency is drawn from:
//
//	sleep = baseLatencyMs + perTokenLatencyMs*totalTokens + N(0, baseLatencyMs*variance)
//
// clamped to >= 1ms. Each batch POST is failed with probability
// FailureRate (503 response). A FailureRate of 1.0 also flips /health
// to 500 so the pool's ping loop can mark the upstream unhealthy.
type MockUpstreamConfig struct {
	BaseLatencyMs     float64
	PerTokenLatencyMs float64
	LatencyVariance   float64
	FailureRate       float64
}

// DefaultMockUpstreamConfig is the latency profile used by the simulator
// when the caller does not supply values. Picked to keep simulations
// short without being so cheap they hide batching differences.
func DefaultMockUpstreamConfig() MockUpstreamConfig {
	return MockUpstreamConfig{
		BaseLatencyMs:     15,
		PerTokenLatencyMs: 0.2,
		LatencyVariance:   0.1,
		FailureRate:       0,
	}
}

// MockUpstream wraps a httptest.Server that mimics the upstream bulk
// inference API. It handles POST /v1/batch and GET /health, plus the
// /v1/batch/health path that the existing pool ping computes by
// concatenating Upstream.URL and Upstream.HealthPath.
type MockUpstream struct {
	server *httptest.Server
	cfg    MockUpstreamConfig

	mu   sync.Mutex
	rand *rand.Rand

	// observedPriorities captures the priority of every batch POST in the
	// order it was received. Used by integration tests to assert that
	// priority-split dispatch prevents FIFO inversion.
	observedPriorities []models.Priority
}

// NewMockUpstream starts the mock server on an OS-chosen loopback port.
// Callers MUST call Close when done to release the listener.
func NewMockUpstream(cfg MockUpstreamConfig) *MockUpstream {
	m := &MockUpstream{
		cfg:  cfg,
		rand: rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/batch", m.handleBatch)
	mux.HandleFunc("/health", m.handleHealth)
	// The pool's ping concatenates Upstream.URL + Upstream.HealthPath, so
	// for a sim configured with URL=.../v1/batch and HealthPath=/health
	// the ping lands here. Register the composed path too.
	mux.HandleFunc("/v1/batch/health", m.handleHealth)
	m.server = httptest.NewServer(mux)
	return m
}

// URL returns the base URL of the httptest server (e.g. http://127.0.0.1:NNNNN).
// The Runner appends "/v1/batch" to produce the upstream.url for the
// ProxyWorker.
func (m *MockUpstream) URL() string { return m.server.URL }

// HealthPath returns the health endpoint path the Runner places in
// cfg.Upstream.HealthPath.
func (m *MockUpstream) HealthPath() string { return "/health" }

// Close shuts down the httptest.Server and releases its listener.
func (m *MockUpstream) Close() { m.server.Close() }

// SetFailureRate atomically updates the mock's failure rate so tests can
// flip health mid-run. A rate >= 1.0 also flips /health to 500 so the
// pool's ping loop marks the upstream unhealthy.
func (m *MockUpstream) SetFailureRate(rate float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg.FailureRate = rate
}

// ObservedPriorities returns the priorities of every batch POST in the
// order received. Batches are homogeneous by construction (QueueMatrix
// guarantees this), so a single value per batch is sufficient.
func (m *MockUpstream) ObservedPriorities() []models.Priority {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]models.Priority, len(m.observedPriorities))
	copy(out, m.observedPriorities)
	return out
}

func (m *MockUpstream) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	m.mu.Lock()
	rate := m.cfg.FailureRate
	m.mu.Unlock()
	if rate >= 1.0 {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (m *MockUpstream) handleBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var batch models.Batch
	if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	m.recordBatch(&batch)

	if m.rollFailure() {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	sleep := m.computeLatency(batch.TotalTokens())
	select {
	case <-r.Context().Done():
		// Client (ProxyWorker) cancelled; no point writing a response.
		return
	case <-time.After(sleep):
	}

	results := make([]models.RequestResult, len(batch.Requests))
	for i, req := range batch.Requests {
		results[i] = models.RequestResult{
			RequestID: req.ID,
			Result:    m.fakeResult(req),
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(results)
}

func (m *MockUpstream) recordBatch(batch *models.Batch) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(batch.Requests) > 0 {
		m.observedPriorities = append(m.observedPriorities, batch.Requests[0].Priority)
	}
}

func (m *MockUpstream) rollFailure() bool {
	if m.cfg.FailureRate <= 0 {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rand.Float64() < m.cfg.FailureRate
}

func (m *MockUpstream) computeLatency(totalTokens int) time.Duration {
	m.mu.Lock()
	jitter := m.rand.NormFloat64() * m.cfg.BaseLatencyMs * m.cfg.LatencyVariance
	m.mu.Unlock()
	ms := m.cfg.BaseLatencyMs + m.cfg.PerTokenLatencyMs*float64(totalTokens) + jitter
	if ms < 1 {
		ms = 1
	}
	return time.Duration(ms * float64(time.Millisecond))
}

// fakeResult returns a shape appropriate for the request type: a short
// string for completion, a small slice of random floats for embedding.
func (m *MockUpstream) fakeResult(req *models.InferenceRequest) any {
	if req.RequestType == models.RequestTypeEmbedding {
		dim := 8
		out := make([]float64, dim)
		m.mu.Lock()
		for i := range out {
			out[i] = m.rand.Float64()
		}
		m.mu.Unlock()
		return out
	}
	// Completion: echo a short snippet of the prompt so responses are
	// deterministic-ish but not empty.
	prompt := req.Prompt
	if len(prompt) > 16 {
		prompt = prompt[:16]
	}
	return strings.TrimSpace(prompt) + " ..."
}
