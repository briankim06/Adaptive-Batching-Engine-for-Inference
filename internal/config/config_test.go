package config

import (
	"os"
	"testing"
	"time"

	"github.com/spf13/viper"
)

func validConfig() *Config {
	return &Config{
		Server: ServerConfig{
			Port: 8080,
		},
		Batching: BatchingConfig{
			Strategy:      "queue_depth",
			MinBatchSize:  1,
			MaxBatchSize:  8,
			MinWaitMs:     5,
			MaxWaitMs:     50,
			QueueCapacity: 100,
		},
		Workers: WorkerConfig{
			Count: 1,
		},
		Upstream: UpstreamConfig{
			URL:            "http://inference:8000/v1/batch",
			RequestTimeout: 30 * time.Second,
		},
		Health: HealthConfig{
			FailureThreshold: 3,
		},
		Metrics: MetricsConfig{
			P99SmoothingAlpha: 0.3,
		},
	}
}

func TestConfigValidate_ValidBaseline(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
		t.Fatalf("expected valid config, got error: %v", err)
	}
}

func TestConfigValidate_RejectsInvalid(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{
			name:   "server port must be > 0",
			mutate: func(cfg *Config) { cfg.Server.Port = 0 },
		},
		{
			name: "max batch size must be >= min batch size",
			mutate: func(cfg *Config) {
				cfg.Batching.MinBatchSize = 2
				cfg.Batching.MaxBatchSize = 1
			},
		},
		{
			name:   "workers count must be >= 1",
			mutate: func(cfg *Config) { cfg.Workers.Count = 0 },
		},
		{
			name:   "queue capacity must be >= 1",
			mutate: func(cfg *Config) { cfg.Batching.QueueCapacity = 0 },
		},
		{
			name:   "strategy must be one of fixed/queue_depth/latency_aware",
			mutate: func(cfg *Config) { cfg.Batching.Strategy = "bogus" },
		},
		{
			name:   "health failure threshold must be >= 1",
			mutate: func(cfg *Config) { cfg.Health.FailureThreshold = 0 },
		},
		{
			name:   "upstream url must be set",
			mutate: func(cfg *Config) { cfg.Upstream.URL = "" },
		},
		{
			name:   "upstream url must be http(s)",
			mutate: func(cfg *Config) { cfg.Upstream.URL = "ftp://example.com" },
		},
		{
			name:   "upstream request timeout must be > 0",
			mutate: func(cfg *Config) { cfg.Upstream.RequestTimeout = 0 },
		},
		{
			name:   "p99 smoothing alpha must be > 0",
			mutate: func(cfg *Config) { cfg.Metrics.P99SmoothingAlpha = 0 },
		},
		{
			name:   "p99 smoothing alpha must be <= 1",
			mutate: func(cfg *Config) { cfg.Metrics.P99SmoothingAlpha = 1.5 },
		},
	}

	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			cfg := validConfig()
			testCase.mutate(cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatalf("expected validation error")
			}
		})
	}
}

func TestLoadEnvOnlyConfig(t *testing.T) {
	viper.Reset()

	tmpDir := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd failed: %v", err)
	}
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("chdir failed: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(oldWD)
	})

	envs := map[string]string{
		"ABE_SERVER_PORT":                 "8081",
		"ABE_BATCHING_STRATEGY":           "fixed",
		"ABE_BATCHING_MIN_BATCH_SIZE":     "1",
		"ABE_BATCHING_MAX_BATCH_SIZE":     "2",
		"ABE_BATCHING_MIN_WAIT_MS":        "5",
		"ABE_BATCHING_MAX_WAIT_MS":        "10",
		"ABE_BATCHING_QUEUE_CAPACITY":     "100",
		"ABE_WORKERS_COUNT":               "1",
		"ABE_UPSTREAM_URL":                "http://inference:8000/v1/batch",
		"ABE_UPSTREAM_REQUEST_TIMEOUT":    "30s",
		"ABE_HEALTH_FAILURE_THRESHOLD":    "3",
		"ABE_METRICS_P99_SMOOTHING_ALPHA": "0.5",
	}
	for key, value := range envs {
		if err := os.Setenv(key, value); err != nil {
			t.Fatalf("setenv %s failed: %v", key, err)
		}
		t.Cleanup(func() {
			_ = os.Unsetenv(key)
		})
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("expected Load() to succeed with env-only config: %v", err)
	}
	if cfg.Server.Port != 8081 {
		t.Fatalf("expected server.port=8081, got %d", cfg.Server.Port)
	}
	if cfg.Upstream.URL != "http://inference:8000/v1/batch" {
		t.Fatalf("expected upstream.url to round-trip, got %q", cfg.Upstream.URL)
	}
	if cfg.Metrics.P99SmoothingAlpha != 0.5 {
		t.Fatalf("expected p99_smoothing_alpha=0.5, got %v", cfg.Metrics.P99SmoothingAlpha)
	}
}
