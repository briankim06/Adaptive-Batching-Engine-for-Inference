package config

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
)

type Config struct {
	Server   ServerConfig   `mapstructure:"server"`
	Batching BatchingConfig `mapstructure:"batching"`
	Workers  WorkerConfig   `mapstructure:"workers"`
	Health   HealthConfig   `mapstructure:"health"`
	Metrics  MetricsConfig  `mapstructure:"metrics"`
}

type ServerConfig struct {
	Host         string        `mapstructure:"host"`
	Port         int           `mapstructure:"port"`
	ReadTimeout  time.Duration `mapstructure:"read_timeout"`
	WriteTimeout time.Duration `mapstructure:"write_timeout"`
}

type BatchingConfig struct {
	Strategy               string `mapstructure:"strategy"`
	MinBatchSize           int    `mapstructure:"min_batch_size"`
	MaxBatchSize           int    `mapstructure:"max_batch_size"`
	MinWaitMs              int    `mapstructure:"min_wait_ms"`
	MaxWaitMs              int    `mapstructure:"max_wait_ms"`
	QueueDepthLowThreshold int    `mapstructure:"queue_depth_low_threshold"`
	QueueDepthHighThreshold int   `mapstructure:"queue_depth_high_threshold"`
	QueueCapacity          int    `mapstructure:"queue_capacity"`
	TokenBucketingEnabled  bool   `mapstructure:"token_bucketing_enabled"`
	PriorityEnabled        bool   `mapstructure:"priority_enabled"`
}

type WorkerConfig struct {
	Count            int     `mapstructure:"count"`
	BaseLatencyMs    float64 `mapstructure:"base_latency_ms"`
	PerTokenLatencyMs float64 `mapstructure:"per_token_latency_ms"`
	LatencyVariance  float64 `mapstructure:"latency_variance"`
	MaxBatchTokens   int     `mapstructure:"max_batch_tokens"`
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
}

func (c *Config) Validate() error {
	if c.Server.Port <= 0 {
		return fmt.Errorf("server.port must be > 0")
	}
	if c.Batching.MinBatchSize <= 0 {
		return fmt.Errorf("batching.min_batch_size must be > 0")
	}
	if c.Batching.MaxBatchSize < c.Batching.MinBatchSize {
		return fmt.Errorf("batching.max_batch_size must be >= batching.min_batch_size")
	}
	if c.Batching.MaxWaitMs < c.Batching.MinWaitMs {
		return fmt.Errorf("batching.max_wait_ms must be >= batching.min_wait_ms")
	}
	if c.Workers.Count <= 0 {
		return fmt.Errorf("workers.count must be > 0")
	}
	if c.Batching.QueueCapacity <= 0 {
		return fmt.Errorf("batching.queue_capacity must be > 0")
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
		"batching.token_bucketing_enabled",
		"batching.priority_enabled",
		"workers.count",
		"workers.base_latency_ms",
		"workers.per_token_latency_ms",
		"workers.latency_variance",
		"workers.max_batch_tokens",
		"health.check_interval",
		"health.failure_threshold",
		"health.recovery_timeout",
		"metrics.collection_interval",
		"metrics.percentile_window_size",
		"metrics.history_retention",
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
