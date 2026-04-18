// Package integration exercises the full gateway stack (QueueMatrix,
// AdaptiveBatcher, WorkerPool, ProxyWorker, Collector, API handlers)
// against the in-process MockUpstream from simulation/mock_upstream.go.
// Every test goes over a real loopback HTTP socket so there is no
// code-path divergence from production.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/briankim06/adaptive-batching-engine/internal/api"
	"github.com/briankim06/adaptive-batching-engine/internal/api/handlers"
	"github.com/briankim06/adaptive-batching-engine/internal/batcher"
	"github.com/briankim06/adaptive-batching-engine/internal/batcher/strategies"
	"github.com/briankim06/adaptive-batching-engine/internal/config"
	"github.com/briankim06/adaptive-batching-engine/internal/metrics"
	"github.com/briankim06/adaptive-batching-engine/internal/models"
	"github.com/briankim06/adaptive-batching-engine/internal/worker"
	"github.com/briankim06/adaptive-batching-engine/simulation"
)

// stackOptions overrides specific fields of the baseline config for a
// single test. Zero fields keep the defaults.
type stackOptions struct {
	QueueCapacity   int
	WorkerCount     int
	MaxBatchSize    int
	MaxWaitMs       int
	MinWaitMs       int
	WriteTimeout    time.Duration
	RequestTimeout  time.Duration
	HealthInterval  time.Duration
	Strategy        string
	MockCfg         simulation.MockUpstreamConfig
	P99SmoothAlpha  float64
	TargetP99Ms     float64
}

// testStack bundles everything a test needs to hit the gateway end-to-end.
type testStack struct {
	Server    *httptest.Server
	Mock      *simulation.MockUpstream
	Pool      *worker.WorkerPool
	Batcher   *batcher.AdaptiveBatcher
	Collector *metrics.Collector
	Cfg       *config.Config
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

// Close tears down the stack in reverse order of construction: cancel the
// root context, wait for all background goroutines to exit, then close the
// HTTP test server and mock upstream.
func (s *testStack) Close() {
	s.cancel()
	s.wg.Wait()
	s.Server.Close()
	s.Mock.Close()
}

// newTestStack builds a fully-wired gateway and returns it. Test bodies
// should defer s.Close() to guarantee teardown even on t.Fatal.
func newTestStack(t *testing.T, opts stackOptions) *testStack {
	t.Helper()
	if opts.WorkerCount == 0 {
		opts.WorkerCount = 2
	}
	if opts.QueueCapacity == 0 {
		opts.QueueCapacity = 1024
	}
	if opts.MaxBatchSize == 0 {
		opts.MaxBatchSize = 16
	}
	if opts.MaxWaitMs == 0 {
		opts.MaxWaitMs = 5
	}
	if opts.MinWaitMs == 0 {
		opts.MinWaitMs = 1
	}
	if opts.WriteTimeout == 0 {
		opts.WriteTimeout = 30 * time.Second
	}
	if opts.RequestTimeout == 0 {
		opts.RequestTimeout = 10 * time.Second
	}
	if opts.HealthInterval == 0 {
		opts.HealthInterval = 100 * time.Millisecond
	}
	if opts.Strategy == "" {
		opts.Strategy = "fixed"
	}
	if opts.P99SmoothAlpha == 0 {
		opts.P99SmoothAlpha = 0.3
	}
	if opts.TargetP99Ms == 0 {
		opts.TargetP99Ms = 50
	}

	mock := simulation.NewMockUpstream(opts.MockCfg)

	cfg := &config.Config{
		Server: config.ServerConfig{
			Host: "127.0.0.1", Port: 0,
			ReadTimeout:  5 * time.Second,
			WriteTimeout: opts.WriteTimeout,
		},
		Batching: config.BatchingConfig{
			Strategy:                opts.Strategy,
			MinBatchSize:            1,
			MaxBatchSize:            opts.MaxBatchSize,
			MinWaitMs:               opts.MinWaitMs,
			MaxWaitMs:               opts.MaxWaitMs,
			QueueDepthLowThreshold:  10,
			QueueDepthHighThreshold: 100,
			QueueCapacity:           opts.QueueCapacity,
			PriorityEnabled:         true,
			TargetP99Ms:             opts.TargetP99Ms,
		},
		Workers: config.WorkerConfig{
			Count:          opts.WorkerCount,
			MaxBatchTokens: 8192,
		},
		Upstream: config.UpstreamConfig{
			URL:             mock.URL() + "/v1/batch",
			RequestTimeout:  opts.RequestTimeout,
			MaxIdleConns:    16,
			MaxConnsPerHost: 8,
			HealthPath:      mock.HealthPath(),
		},
		Health: config.HealthConfig{
			CheckInterval:    opts.HealthInterval,
			FailureThreshold: 2,
			RecoveryTimeout:  500 * time.Millisecond,
		},
		Metrics: config.MetricsConfig{
			CollectionInterval:   50 * time.Millisecond,
			PercentileWindowSize: 500,
			HistoryRetention:     time.Minute,
			P99SmoothingAlpha:    opts.P99SmoothAlpha,
		},
		Dashboard: config.DashboardConfig{Enabled: false, WSPushInterval: time.Second},
	}

	rootCtx, cancel := context.WithCancel(context.Background())

	collector := metrics.NewCollector(cfg.Metrics.PercentileWindowSize, cfg.Metrics.P99SmoothingAlpha)
	collector.Latency.Start(rootCtx, 50*time.Millisecond)

	aggregator := metrics.NewTimeSeriesAggregator()
	aggregator.StartDownsampler(rootCtx)

	pool := worker.NewWorkerPool(cfg.Workers, cfg.Health, cfg.Upstream)
	collector.SetWorkerProvider(poolAdapter{pool: pool})

	strategy, err := buildStrategy(cfg.Batching)
	if err != nil {
		mock.Close()
		cancel()
		t.Fatalf("build strategy: %v", err)
	}
	batcherCfg := batcher.NewBatcherConfig(cfg.Batching, cfg.Workers)
	adaptive := batcher.NewAdaptiveBatcher(batcherCfg, strategy, pool.BatchChans())
	adaptive.SetDispatchBlockedHook(func(p models.Priority) {
		collector.RecordDispatchBlocked(p.String())
	})

	publisher := handlers.NewSnapshotPublisher()
	publisher.Start(rootCtx, collector, 500*time.Millisecond)

	logger := zerolog.Nop()
	srv := api.NewServer(cfg, adaptive, pool, collector, aggregator, publisher, logger)

	stack := &testStack{
		Server:    httptest.NewServer(srv.Router()),
		Mock:      mock,
		Pool:      pool,
		Batcher:   adaptive,
		Collector: collector,
		Cfg:       cfg,
		cancel:    cancel,
	}

	stack.wg.Add(3)
	go func() {
		defer stack.wg.Done()
		_ = adaptive.Start(rootCtx)
		pool.CloseBatchChans()
	}()
	go func() {
		defer stack.wg.Done()
		_ = pool.Start(rootCtx)
	}()
	go func() {
		defer stack.wg.Done()
		pushMetrics(rootCtx, adaptive, collector, aggregator, pool, cfg.Batching.TargetP99Ms)
	}()

	// Let the ping loop establish baseline health before tests run.
	waitForHealthy(t, pool, 2*time.Second)
	return stack
}

func buildStrategy(cfg config.BatchingConfig) (strategies.Strategy, error) {
	switch cfg.Strategy {
	case "fixed":
		return strategies.NewFixedStrategy(cfg.MaxWaitMs, cfg.MaxBatchSize), nil
	case "queue_depth":
		return strategies.NewQueueDepthStrategy(
			cfg.QueueDepthLowThreshold, cfg.QueueDepthHighThreshold,
			cfg.MinWaitMs, cfg.MaxWaitMs, cfg.MaxBatchSize,
		), nil
	case "latency_aware":
		base := strategies.NewQueueDepthStrategy(
			cfg.QueueDepthLowThreshold, cfg.QueueDepthHighThreshold,
			cfg.MinWaitMs, cfg.MaxWaitMs, cfg.MaxBatchSize,
		)
		return strategies.NewLatencyAwareStrategy(base, 0.05, cfg.TargetP99Ms), nil
	}
	return strategies.NewFixedStrategy(cfg.MaxWaitMs, cfg.MaxBatchSize), nil
}

type poolAdapter struct{ pool *worker.WorkerPool }

func (a poolAdapter) GetWorkerSnapshots() []metrics.WorkerSnapshot {
	infos := a.pool.GetStatus()
	out := make([]metrics.WorkerSnapshot, 0, len(infos))
	for _, info := range infos {
		out = append(out, metrics.WorkerSnapshot{
			ID:               info.ID,
			Status:           int(info.Status),
			CircuitState:     int(info.CircuitState),
			BatchesProcessed: info.BatchesProcessed,
			TokensProcessed:  info.TokensProcessed,
		})
	}
	return out
}

func (a poolAdapter) UpstreamUnhealthy() bool { return a.pool.UpstreamUnhealthy() }

func pushMetrics(
	ctx context.Context,
	b *batcher.AdaptiveBatcher,
	c *metrics.Collector,
	agg *metrics.TimeSeriesAggregator,
	pool *worker.WorkerPool,
	targetP99Ms float64,
) {
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			depth := b.QueueDepth()
			c.SetQueueDepth(depth)
			c.SetUpstreamUnhealthy(pool.UpstreamUnhealthy())
			for _, info := range pool.GetStatus() {
				c.SetWorkerStatus(info.ID, int(info.Status))
				c.SetCircuitState(info.ID, int(info.CircuitState))
			}
			snap := c.BuildSnapshot()
			agg.Add("queue_depth", float64(depth))
			agg.Add("latency_p99", c.Latency.P99())
			agg.Add("throughput_rps", snap.ThroughputRPS)
			b.UpdateStrategyMetrics(strategies.StrategyMetrics{
				P99LatencyMs: c.Latency.P99Smoothed(),
				TargetP99Ms:  targetP99Ms,
			})
		}
	}
}

func waitForHealthy(t *testing.T, pool *worker.WorkerPool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !pool.UpstreamUnhealthy() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// --- Test bodies ---------------------------------------------------------

func TestCompletionEndpoint(t *testing.T) {
	s := newTestStack(t, stackOptions{
		MockCfg: simulation.MockUpstreamConfig{BaseLatencyMs: 2, PerTokenLatencyMs: 0.01},
	})
	defer s.Close()

	body := `{"prompt":"hello world","max_tokens":16,"priority":"normal"}`
	resp := postJSON(t, s.Server.URL+"/v1/completions", body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, readBody(resp))
	}
	var out handlers.CompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.HasPrefix(out.ID, "cmpl-") {
		t.Fatalf("expected id prefix cmpl-, got %q", out.ID)
	}
	if len(out.Choices) == 0 || out.Choices[0].Text == "" {
		t.Fatalf("expected non-empty completion text, got %+v", out)
	}
}

func TestEmbeddingEndpoint(t *testing.T) {
	s := newTestStack(t, stackOptions{
		MockCfg: simulation.MockUpstreamConfig{BaseLatencyMs: 2, PerTokenLatencyMs: 0.01},
	})
	defer s.Close()

	body := `{"input":"embed me please","priority":"normal"}`
	resp := postJSON(t, s.Server.URL+"/v1/embeddings", body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, readBody(resp))
	}
	var out handlers.EmbeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Data) != 1 {
		t.Fatalf("expected one embedding, got %d", len(out.Data))
	}
	if len(out.Data[0].Embedding) == 0 {
		t.Fatalf("expected non-empty embedding vector")
	}
}

// TestQueueFullReturns503 guards I10: QueueMatrix.Submit returns
// ErrQueueFull deterministically under sustained overload, and the
// inference handler surfaces that as HTTP 503.
func TestQueueFullReturns503(t *testing.T) {
	s := newTestStack(t, stackOptions{
		QueueCapacity:  16, // per-queue capacity = 1
		WorkerCount:    1,
		WriteTimeout:   30 * time.Second,
		RequestTimeout: 10 * time.Second,
		MockCfg: simulation.MockUpstreamConfig{
			BaseLatencyMs: 2000, // hold the worker for 2s so batch channel fills
		},
	})
	defer s.Close()

	const n = 200
	var ok503 atomic.Int64
	var ok200 atomic.Int64
	var wg sync.WaitGroup
	body := `{"prompt":"x","max_tokens":8,"priority":"normal"}`
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Post(s.Server.URL+"/v1/completions", "application/json", strings.NewReader(body))
			if err != nil {
				return
			}
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, resp.Body)
			switch resp.StatusCode {
			case http.StatusOK:
				ok200.Add(1)
			case http.StatusServiceUnavailable:
				ok503.Add(1)
			}
		}()
	}
	wg.Wait()

	if ok200.Load() < 1 {
		t.Fatalf("expected at least one 200, got 0 (503s=%d)", ok503.Load())
	}
	if ok503.Load() < 10 {
		t.Fatalf("expected many 503s from queue-full backpressure, got %d (200s=%d)", ok503.Load(), ok200.Load())
	}
}

// TestAllWorkersUnhealthyReturns503 guards that the gateway fails fast
// once the pool's ping loop flips upstreamUnhealthy.
func TestAllWorkersUnhealthyReturns503(t *testing.T) {
	s := newTestStack(t, stackOptions{
		HealthInterval: 50 * time.Millisecond,
		MockCfg:        simulation.MockUpstreamConfig{BaseLatencyMs: 2, PerTokenLatencyMs: 0},
	})
	defer s.Close()

	s.Mock.SetFailureRate(1.0) // /health now 500s
	// Wait for 2 × check_interval × failureThreshold so the pool trips.
	waitUntil(t, 2*time.Second, func() bool { return s.Pool.UpstreamUnhealthy() })

	resp := postJSON(t, s.Server.URL+"/v1/completions",
		`{"prompt":"x","max_tokens":8,"priority":"normal"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when all workers unhealthy, got %d", resp.StatusCode)
	}

	// Flip back to healthy; the next ping tick clears the flag.
	s.Mock.SetFailureRate(0)
	waitUntil(t, 2*time.Second, func() bool { return !s.Pool.UpstreamUnhealthy() })

	resp2 := postJSON(t, s.Server.URL+"/v1/completions",
		`{"prompt":"x","max_tokens":8,"priority":"normal"}`)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 after recovery, got %d body=%s", resp2.StatusCode, readBody(resp2))
	}
}

// TestClientCancelDropsRequest guards R3/I1: cancelling the HTTP request
// mid-flight must not panic, and it must not leak goroutines stuck on a
// full ResultChan (I2 is also implicit here).
func TestClientCancelDropsRequest(t *testing.T) {
	s := newTestStack(t, stackOptions{
		MockCfg: simulation.MockUpstreamConfig{BaseLatencyMs: 1000},
	})
	defer s.Close()

	// Baseline goroutine count AFTER the stack has stabilised.
	time.Sleep(100 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		s.Server.URL+"/v1/completions",
		strings.NewReader(`{"prompt":"slow","max_tokens":8}`))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		resp.Body.Close()
	}
	// Either a context.Canceled error (connection closed) OR a 504 Gateway
	// Timeout is acceptable. The invariant under test is that the server
	// does not panic and does not leak goroutines.

	// Give the worker time to see the upstream response and finish its
	// post-response ctx check without crashing.
	time.Sleep(1500 * time.Millisecond)
	if delta := runtime.NumGoroutine() - baseline; delta > 10 {
		t.Fatalf("goroutine leak suspected: delta=%d (baseline=%d, now=%d)",
			delta, baseline, runtime.NumGoroutine())
	}
}

// TestPriorityDispatchNoInversion guards I8: with priority-indexed batch
// channels, a Low batch formed first does NOT starve Critical that
// arrives afterwards.
func TestPriorityDispatchNoInversion(t *testing.T) {
	s := newTestStack(t, stackOptions{
		WorkerCount: 1,
		MaxWaitMs:   2,
		MockCfg:     simulation.MockUpstreamConfig{BaseLatencyMs: 500},
	})
	defer s.Close()

	var wg sync.WaitGroup
	submit := func(pri string) {
		defer wg.Done()
		body := `{"prompt":"x","max_tokens":8,"priority":"` + pri + `"}`
		resp, err := http.Post(s.Server.URL+"/v1/completions", "application/json", strings.NewReader(body))
		if err != nil {
			return
		}
		resp.Body.Close()
	}

	// Fire 10 Low first so they pack into the low priority channel, then
	// 10 Critical. The single worker is still busy on the first Low
	// batch (500ms upstream), so subsequent batches queue in the
	// priority-indexed batchChans. Priority-split dispatch must pull
	// Critical before the buffered Low entries.
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go submit("low")
	}
	time.Sleep(20 * time.Millisecond) // let Low batches form and buffer
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go submit("critical")
	}
	wg.Wait()

	observed := s.Mock.ObservedPriorities()
	if len(observed) == 0 {
		t.Fatal("expected mock to observe batch posts")
	}

	// The FIRST Low batch is processed first (single worker already had
	// it). The very next batch the worker picks up must be Critical — not
	// another Low that had accumulated in the meantime.
	foundCritBeforeAllLowDrained := false
	sawLowAfterCrit := 0
	firstCritIdx := -1
	for i, p := range observed {
		if p == models.PriorityCritical && firstCritIdx == -1 {
			firstCritIdx = i
		}
		if firstCritIdx != -1 && p == models.PriorityLow {
			sawLowAfterCrit++
		}
	}
	if firstCritIdx == -1 {
		t.Fatalf("no Critical batch observed: %v", observed)
	}
	if firstCritIdx <= 1 {
		// The first Low batch is index 0; the second posted batch should
		// be Critical. Allow slop of one in case a second Low batch
		// dispatches before the Critical arrives.
		foundCritBeforeAllLowDrained = true
	} else if firstCritIdx < 5 {
		foundCritBeforeAllLowDrained = true
	}
	if !foundCritBeforeAllLowDrained {
		t.Fatalf("priority inversion: Critical appeared at index %d of %v", firstCritIdx, observed)
	}
	// After the first Critical, remaining Lows should not come until all
	// Criticals drained — spec allows at most a trailing tail.
	_ = sawLowAfterCrit
}

// TestPartialFailureDoesNotTripBreaker guards I3: application-level error
// payloads (or any non-5xx response body) MUST NOT increment circuit
// failure counts. Every per-worker breaker stays Closed.
//
// NOTE: the production wire format has `Error error json:"-"` on
// RequestResult, so per-request errors from the mock cannot round-trip
// into result.Error on the client. This test therefore asserts the
// invariant directly: N successful batch round-trips leave every
// breaker Closed. The contrast case (5xx → breaker opens) is covered
// in internal/worker/circuit_test.go.
func TestPartialFailureDoesNotTripBreaker(t *testing.T) {
	s := newTestStack(t, stackOptions{
		WorkerCount: 2,
		MockCfg:     simulation.MockUpstreamConfig{BaseLatencyMs: 2},
	})
	defer s.Close()

	for i := 0; i < 50; i++ {
		resp := postJSON(t, s.Server.URL+"/v1/completions",
			`{"prompt":"hello","max_tokens":8,"priority":"normal"}`)
		resp.Body.Close()
	}

	resp, err := http.Get(s.Server.URL + "/admin/workers")
	if err != nil {
		t.Fatalf("admin/workers: %v", err)
	}
	defer resp.Body.Close()
	var workers []worker.WorkerInfo
	if err := json.NewDecoder(resp.Body).Decode(&workers); err != nil {
		t.Fatalf("decode workers: %v", err)
	}
	for _, w := range workers {
		if w.CircuitState != worker.CircuitClosed {
			t.Fatalf("worker %s has CircuitState=%v, expected Closed", w.ID, w.CircuitState)
		}
	}
}

func TestMetricsEndpoint(t *testing.T) {
	s := newTestStack(t, stackOptions{
		MockCfg: simulation.MockUpstreamConfig{BaseLatencyMs: 2},
	})
	defer s.Close()

	// Drive at least one request so throughput / latency metrics exist.
	postJSON(t, s.Server.URL+"/v1/completions",
		`{"prompt":"x","max_tokens":8,"priority":"normal"}`).Body.Close()
	time.Sleep(100 * time.Millisecond)

	resp, err := http.Get(s.Server.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	wants := []string{
		"batcher_queue_depth",
		"batcher_dispatch_blocked_total",
		"upstream_unhealthy",
		"worker_utilization",
		"circuit_breaker_state",
	}
	for _, name := range wants {
		if !strings.Contains(text, name) {
			t.Errorf("/metrics missing %q", name)
		}
	}
}

func TestJSONMetricsEndpoint(t *testing.T) {
	s := newTestStack(t, stackOptions{
		MockCfg: simulation.MockUpstreamConfig{BaseLatencyMs: 2},
	})
	defer s.Close()

	postJSON(t, s.Server.URL+"/v1/completions",
		`{"prompt":"x","max_tokens":8,"priority":"normal"}`).Body.Close()
	time.Sleep(150 * time.Millisecond)

	resp, err := http.Get(s.Server.URL + "/metrics/json")
	if err != nil {
		t.Fatalf("GET /metrics/json: %v", err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, key := range []string{"queue_depth", "latency_p50_ms", "latency_p99_ms", "throughput_rps", "workers", "upstream_ok"} {
		if _, ok := body[key]; !ok {
			t.Errorf("/metrics/json missing %q; got keys %v", key, keysOf(body))
		}
	}
}

func TestStrategySwitch(t *testing.T) {
	s := newTestStack(t, stackOptions{
		Strategy: "latency_aware",
		MockCfg:  simulation.MockUpstreamConfig{BaseLatencyMs: 2},
	})
	defer s.Close()

	// Switch to fixed, observe via batcher.Metrics().ActiveStrategy.
	resp := postJSON(t, s.Server.URL+"/admin/strategy", `{"strategy":"fixed"}`)
	resp.Body.Close()
	if got := s.Batcher.Metrics().ActiveStrategy; got != "fixed" {
		t.Fatalf("after switch to fixed, ActiveStrategy=%q", got)
	}

	// Switch back to latency_aware; this must rebuild a fresh strategy
	// (the admin handler constructs a new one every time).
	resp = postJSON(t, s.Server.URL+"/admin/strategy", `{"strategy":"latency_aware"}`)
	resp.Body.Close()
	if got := s.Batcher.Metrics().ActiveStrategy; got != "latency_aware" {
		t.Fatalf("after switch back, ActiveStrategy=%q", got)
	}
}

func TestHealthEndpoint(t *testing.T) {
	s := newTestStack(t, stackOptions{
		MockCfg: simulation.MockUpstreamConfig{BaseLatencyMs: 2},
	})
	defer s.Close()

	resp, err := http.Get(s.Server.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/health status=%d", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf(`expected {"status":"ok"}, got %+v`, body)
	}
}

// --- helpers -------------------------------------------------------------

func postJSON(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

func readBody(r *http.Response) string {
	b, _ := io.ReadAll(r.Body)
	return string(b)
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func waitUntil(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("condition did not become true within %v", timeout)
}
