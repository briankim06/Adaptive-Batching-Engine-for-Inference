package models

import (
	"time"

	"github.com/google/uuid"
)

type Batch struct {
	ID           string              `json:"id"`
	Requests     []*InferenceRequest `json:"requests"`
	CreatedAt    time.Time           `json:"created_at"`
	StrategyUsed string              `json:"strategy_used"`
}

func NewBatch(requests []*InferenceRequest, strategy string) *Batch {
	return &Batch{
		ID:           uuid.New().String(),
		Requests:     requests,
		CreatedAt:    time.Now(),
		StrategyUsed: strategy,
	}
}

func (b *Batch) Size() int {
	return len(b.Requests)
}

func (b *Batch) TotalTokens() int {
	total := 0
	for _, r := range b.Requests {
		total += r.EstimatedTokens
	}
	return total
}

func (b *Batch) MaxPriority() Priority {
	if len(b.Requests) == 0 {
		return PriorityNormal
	}
	max := PriorityLow
	for _, r := range b.Requests {
		if r.Priority > max {
			max = r.Priority
		}
	}
	return max
}

type BatchResult struct {
	BatchID          string    `json:"batch_id"`
	Results          []any     `json:"results"`
	ProcessingTimeMs float64   `json:"processing_time_ms"`
	WorkerID         string    `json:"worker_id"`
	CompletedAt      time.Time `json:"completed_at"`
	Error            error     `json:"-"`
}
