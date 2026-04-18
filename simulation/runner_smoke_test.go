package simulation

import (
	"context"
	"testing"
	"time"

	"github.com/briankim06/adaptive-batching-engine/internal/batcher/strategies"
	"github.com/briankim06/adaptive-batching-engine/internal/config"
)

// TestRunnerSmoke exercises the full Runner flow (mock upstream,
// batcher, pool, collector) over a 500ms scenario. It guards against
// regressions in the Phase 7 wiring that would otherwise only surface
// under cmd/simulator's 60s scenarios.
func TestRunnerSmoke(t *testing.T) {
	cfg := smokeConfig()

	scenario := Scenario{
		Name: "smoke",
		Generator: NewConstantRateGenerator(50, GeneratorConfig{
			Duration: 500 * time.Millisecond,
		}),
		Duration: 500 * time.Millisecond,
	}

	strategy := strategies.NewFixedStrategy(cfg.Batching.MaxWaitMs, cfg.Batching.MaxBatchSize)

	runner := NewRunner(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res := runner.Run(ctx, scenario, strategy, MockUpstreamConfig{
		BaseLatencyMs:     2,
		PerTokenLatencyMs: 0.01,
		LatencyVariance:   0,
	})

	if res.TotalRequests == 0 {
		t.Fatalf("expected TotalRequests > 0, got 0")
	}
	if res.ErrorRate > 0.1 {
		t.Fatalf("error rate too high: %.2f%% (errors=%d/%d)",
			res.ErrorRate*100, res.TotalErrors, res.TotalRequests)
	}
	if res.ScenarioName != "smoke" {
		t.Errorf("ScenarioName = %q, want smoke", res.ScenarioName)
	}
	if res.StrategyName != "fixed" {
		t.Errorf("StrategyName = %q, want fixed", res.StrategyName)
	}
}

// TestScenarioByName covers the lookup surface used by cmd/simulator.
func TestScenarioByName(t *testing.T) {
	names := []string{"steady_state", "ramp_up", "spike_test", "mixed_priority", "long_tail"}
	for _, name := range names {
		s, err := ScenarioByName(name)
		if err != nil {
			t.Fatalf("ScenarioByName(%q) error: %v", name, err)
		}
		if s.Name != name {
			t.Errorf("ScenarioByName(%q).Name = %q", name, s.Name)
		}
		if s.Generator == nil {
			t.Errorf("ScenarioByName(%q).Generator is nil", name)
		}
		if s.Duration <= 0 {
			t.Errorf("ScenarioByName(%q).Duration = %v", name, s.Duration)
		}
	}
	if _, err := ScenarioByName("does_not_exist"); err == nil {
		t.Errorf("ScenarioByName(unknown) returned nil error")
	}
}

func smokeConfig() *config.Config {
	return &config.Config{
		Server: config.ServerConfig{
			Host: "127.0.0.1", Port: 0,
			ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second,
		},
		Batching: config.BatchingConfig{
			Strategy:                "fixed",
			MinBatchSize:            1,
			MaxBatchSize:            16,
			MinWaitMs:               1,
			MaxWaitMs:               5,
			QueueDepthLowThreshold:  10,
			QueueDepthHighThreshold: 100,
			QueueCapacity:           1024,
			PriorityEnabled:         true,
			TargetP99Ms:             50,
		},
		Workers: config.WorkerConfig{
			Count:          2,
			MaxBatchTokens: 8192,
		},
		Upstream: config.UpstreamConfig{
			URL:             "http://placeholder",
			RequestTimeout:  2 * time.Second,
			MaxIdleConns:    16,
			MaxConnsPerHost: 8,
			HealthPath:      "/health",
		},
		Health: config.HealthConfig{
			CheckInterval:    250 * time.Millisecond,
			FailureThreshold: 3,
			RecoveryTimeout:  1 * time.Second,
		},
		Metrics: config.MetricsConfig{
			CollectionInterval:   50 * time.Millisecond,
			PercentileWindowSize: 500,
			HistoryRetention:     time.Minute,
			P99SmoothingAlpha:    0.3,
		},
		Dashboard: config.DashboardConfig{Enabled: false, Port: 0, WSPushInterval: time.Second},
	}
}
