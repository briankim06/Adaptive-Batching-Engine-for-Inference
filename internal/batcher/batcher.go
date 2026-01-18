package batcher

import (
	"context"

	"github.com/yourname/adaptive-batching-engine/internal/config"
	"github.com/yourname/adaptive-batching-engine/internal/models"
)

type Batcher interface {
	Submit(ctx context.Context, req *models.InferenceRequest) error
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	QueueDepth() int
	Metrics() BatcherMetrics
}

type BatcherMetrics struct {
	QueueDepth     int     `json:"queue_depth"`
	BatchesFormed  int64   `json:"batches_formed"`
	RequestsQueued int64   `json:"requests_queued"`
	AvgBatchSize   float64 `json:"avg_batch_size"`
}

type BatcherConfig struct {
	Strategy                string
	MinBatchSize            int
	MaxBatchSize            int
	MinWaitMs               int
	MaxWaitMs               int
	QueueDepthLowThreshold  int
	QueueDepthHighThreshold int
	QueueCapacity           int
	TokenBucketingEnabled   bool
	PriorityEnabled         bool
}

func NewBatcherConfig(cfg config.BatchingConfig) BatcherConfig {
	return BatcherConfig{
		Strategy:                cfg.Strategy,
		MinBatchSize:            cfg.MinBatchSize,
		MaxBatchSize:            cfg.MaxBatchSize,
		MinWaitMs:               cfg.MinWaitMs,
		MaxWaitMs:               cfg.MaxWaitMs,
		QueueDepthLowThreshold:  cfg.QueueDepthLowThreshold,
		QueueDepthHighThreshold: cfg.QueueDepthHighThreshold,
		QueueCapacity:           cfg.QueueCapacity,
		TokenBucketingEnabled:   cfg.TokenBucketingEnabled,
		PriorityEnabled:         cfg.PriorityEnabled,
	}
}
