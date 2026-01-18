package config

import (
	"os"
	"testing"

	"github.com/spf13/viper"
)

func validConfig() *Config {
	return &Config{
		Server: ServerConfig{
			Port: 8080,
		},
		Batching: BatchingConfig{
			MinBatchSize:  1,
			MaxBatchSize:  8,
			MinWaitMs:     5,
			MaxWaitMs:     50,
			QueueCapacity: 100,
		},
		Workers: WorkerConfig{
			Count: 1,
		},
	}
}

func TestConfigValidate(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{
			name: "server port must be > 0",
			mutate: func(cfg *Config) {
				cfg.Server.Port = 0
			},
		},
		{
			name: "min batch size must be > 0",
			mutate: func(cfg *Config) {
				cfg.Batching.MinBatchSize = 0
			},
		},
		{
			name: "max batch size must be >= min batch size",
			mutate: func(cfg *Config) {
				cfg.Batching.MinBatchSize = 2
				cfg.Batching.MaxBatchSize = 1
			},
		},
		{
			name: "max wait must be >= min wait",
			mutate: func(cfg *Config) {
				cfg.Batching.MinWaitMs = 10
				cfg.Batching.MaxWaitMs = 5
			},
		},
		{
			name: "workers count must be > 0",
			mutate: func(cfg *Config) {
				cfg.Workers.Count = 0
			},
		},
		{
			name: "queue capacity must be > 0",
			mutate: func(cfg *Config) {
				cfg.Batching.QueueCapacity = 0
			},
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

	if err := validConfig().Validate(); err != nil {
		t.Fatalf("expected valid config, got error: %v", err)
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
		"ABE_SERVER_PORT":              "8081",
		"ABE_BATCHING_MIN_BATCH_SIZE":  "1",
		"ABE_BATCHING_MAX_BATCH_SIZE":  "2",
		"ABE_BATCHING_MIN_WAIT_MS":     "5",
		"ABE_BATCHING_MAX_WAIT_MS":     "10",
		"ABE_BATCHING_QUEUE_CAPACITY":  "100",
		"ABE_WORKERS_COUNT":            "1",
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
}
