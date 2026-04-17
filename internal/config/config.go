package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// Config is the root runtime configuration. The schema is authoritative in
// docs/spec/shared/configuration.md; keep field names and defaults in sync.
type Config struct {
	Server    ServerConfig    `mapstructure:"server"`
	Batching  BatchingConfig  `mapstructure:"batching"`
	Workers   WorkerConfig    `mapstructure:"workers"`
	Upstream  UpstreamConfig  `mapstructure:"upstream"`
	Health    HealthConfig    `mapstructure:"health"`
	Metrics   MetricsConfig   `mapstructure:"metrics"`
	Dashboard DashboardConfig `mapstructure:"dashboard"`
}

type ServerConfig struct {
	Host         string        `mapstructure:"host"`
	Port         int           `mapstructure:"port"`
	ReadTimeout  time.Duration `mapstructure:"read_timeout"`
	WriteTimeout time.Duration `mapstructure:"write_timeout"`
}

type BatchingConfig struct {
	Strategy                string  `mapstructure:"strategy"`
	MinBatchSize            int     `mapstructure:"min_batch_size"`
	MaxBatchSize            int     `mapstructure:"max_batch_size"`
	MinWaitMs               int     `mapstructure:"min_wait_ms"`
	MaxWaitMs               int     `mapstructure:"max_wait_ms"`
	QueueDepthLowThreshold  int     `mapstructure:"queue_depth_low_threshold"`
	QueueDepthHighThreshold int     `mapstructure:"queue_depth_high_threshold"`
	QueueCapacity           int     `mapstructure:"queue_capacity"`
	PriorityEnabled         bool    `mapstructure:"priority_enabled"`
	TargetP99Ms             float64 `mapstructure:"target_p99_ms"`
}

type WorkerConfig struct {
	Count          int `mapstructure:"count"`
	MaxBatchTokens int `mapstructure:"max_batch_tokens"`
}

type UpstreamConfig struct {
	URL             string        `mapstructure:"url"`
	RequestTimeout  time.Duration `mapstructure:"request_timeout"`
	MaxIdleConns    int           `mapstructure:"max_idle_conns"`
	MaxConnsPerHost int           `mapstructure:"max_conns_per_host"`
	HealthPath      string        `mapstructure:"health_path"`
}

type HealthConfig struct {
	CheckInterval    time.Duration `mapstructure:"check_interval"`
	FailureThreshold int           `mapstructure:"failure_threshold"`
	RecoveryTimeout  time.Duration `mapstructure:"recovery_timeout"`
}

type MetricsConfig struct {
	CollectionInterval   time.Duration `mapstructure:"collection_interval"`
	PercentileWindowSize int           `mapstructure:"percentile_window_size"`
	HistoryRetention     time.Duration `mapstructure:"history_retention"`
	P99SmoothingAlpha    float64       `mapstructure:"p99_smoothing_alpha"`
}

type DashboardConfig struct {
	Enabled        bool          `mapstructure:"enabled"`
	Port           int           `mapstructure:"port"`
	WSPushInterval time.Duration `mapstructure:"ws_push_interval"`
}

var validStrategies = map[string]struct{}{
	"fixed":         {},
	"queue_depth":   {},
	"latency_aware": {},
}

// Validate enforces the rules in docs/spec/shared/configuration.md and a
// small set of defensive sanity checks that are not contradicted by the
// spec.
func (c *Config) Validate() error {
	if c.Server.Port <= 0 {
		return fmt.Errorf("server.port must be > 0")
	}
	if c.Workers.Count < 1 {
		return fmt.Errorf("workers.count must be >= 1")
	}
	if c.Batching.MaxBatchSize < c.Batching.MinBatchSize {
		return fmt.Errorf("batching.max_batch_size must be >= batching.min_batch_size")
	}
	if c.Batching.QueueCapacity < 1 {
		return fmt.Errorf("batching.queue_capacity must be >= 1")
	}
	if _, ok := validStrategies[c.Batching.Strategy]; !ok {
		return fmt.Errorf("batching.strategy must be one of fixed, queue_depth, latency_aware")
	}
	if c.Health.FailureThreshold < 1 {
		return fmt.Errorf("health.failure_threshold must be >= 1")
	}
	if err := validateUpstream(c.Upstream); err != nil {
		return err
	}
	if c.Metrics.P99SmoothingAlpha <= 0 || c.Metrics.P99SmoothingAlpha > 1 {
		return fmt.Errorf("metrics.p99_smoothing_alpha must be in (0, 1]")
	}
	return nil
}

func validateUpstream(u UpstreamConfig) error {
	if u.URL == "" {
		return fmt.Errorf("upstream.url must be set")
	}
	parsed, err := url.Parse(u.URL)
	if err != nil {
		return fmt.Errorf("upstream.url is not a valid URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("upstream.url must use http:// or https:// scheme")
	}
	if parsed.Host == "" {
		return fmt.Errorf("upstream.url must include a host")
	}
	if u.RequestTimeout <= 0 {
		return fmt.Errorf("upstream.request_timeout must be > 0")
	}
	return nil
}

func bindEnvKeys(keys ...string) error {
	for _, key := range keys {
		if err := viper.BindEnv(key); err != nil {
			return err
		}
	}
	return nil
}

// Load reads configuration from config.yaml (optional) and ABE_* env overrides.
// It returns an error if the resulting config fails validation.
func Load() (*Config, error) {
	viper.SetConfigName("config")
	viper.SetConfigType("yaml")
	viper.AddConfigPath(".")
	viper.AddConfigPath("./config")

	viper.SetEnvPrefix("ABE")
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	viper.AutomaticEnv()
	if err := bindEnvKeys(
		"server.host",
		"server.port",
		"server.read_timeout",
		"server.write_timeout",
		"batching.strategy",
		"batching.min_batch_size",
		"batching.max_batch_size",
		"batching.min_wait_ms",
		"batching.max_wait_ms",
		"batching.queue_depth_low_threshold",
		"batching.queue_depth_high_threshold",
		"batching.queue_capacity",
		"batching.priority_enabled",
		"batching.target_p99_ms",
		"workers.count",
		"workers.max_batch_tokens",
		"upstream.url",
		"upstream.request_timeout",
		"upstream.max_idle_conns",
		"upstream.max_conns_per_host",
		"upstream.health_path",
		"health.check_interval",
		"health.failure_threshold",
		"health.recovery_timeout",
		"metrics.collection_interval",
		"metrics.percentile_window_size",
		"metrics.history_retention",
		"metrics.p99_smoothing_alpha",
		"dashboard.enabled",
		"dashboard.port",
		"dashboard.ws_push_interval",
	); err != nil {
		return nil, err
	}

	if err := viper.ReadInConfig(); err != nil {
		var notFound viper.ConfigFileNotFoundError
		if !errors.As(err, &notFound) {
			return nil, err
		}
	}

	var cfg Config
	if err := viper.Unmarshal(&cfg); err != nil {
		return nil, err
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}
