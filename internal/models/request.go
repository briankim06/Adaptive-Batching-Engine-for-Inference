package models

import (
	"time"

	"github.com/google/uuid"
)

type RequestType string

const (
	RequestTypeCompletion RequestType = "completion"
	RequestTypeEmbedding  RequestType = "embedding"
)

type Priority int

const (
	PriorityLow Priority = iota
	PriorityNormal
	PriorityHigh
	PriorityCritical
)

func (p Priority) String() string {
	switch p {
	case PriorityLow:
		return "low"
	case PriorityNormal:
		return "normal"
	case PriorityHigh:
		return "high"
	case PriorityCritical:
		return "critical"
	default:
		return "unknown"
	}
}

func (p Priority) MaxWaitMs() int {
	switch p {
	case PriorityCritical:
		return 5
	case PriorityHigh:
		return 20
	case PriorityNormal:
		return 50
	case PriorityLow:
		return 100
	default:
		return 50
	}
}

type InferenceRequest struct {
	ID              string         `json:"id"`
	Prompt          string         `json:"prompt"`
	MaxTokens       int            `json:"max_tokens"`
	Priority        Priority       `json:"priority"`
	RequestType     RequestType    `json:"request_type"`
	CreatedAt       time.Time      `json:"created_at"`
	EstimatedTokens int            `json:"estimated_tokens"`
	ResultChan      chan *RequestResult `json:"-"`
}

func NewInferenceRequest(prompt string, maxTokens int, priority Priority, reqType RequestType) *InferenceRequest {
	return &InferenceRequest{
		ID:              uuid.New().String(),
		Prompt:          prompt,
		MaxTokens:       maxTokens,
		Priority:        priority,
		RequestType:     reqType,
		CreatedAt:       time.Now(),
		EstimatedTokens: len(prompt)/4 + maxTokens,
		ResultChan:      make(chan *RequestResult, 1),
	}
}

func (r *InferenceRequest) AgeMs() float64 {
	return float64(time.Since(r.CreatedAt).Milliseconds())
}

func (r *InferenceRequest) TokenBucket() int {
	buckets := []int{128, 512, 1024, 2048, 4096}
	for i, threshold := range buckets {
		if r.EstimatedTokens <= threshold {
			return i
		}
	}
	return len(buckets)
}

type RequestResult struct {
	RequestID string  `json:"request_id"`
	Result    any     `json:"result"`
	Error     error   `json:"-"`
	LatencyMs float64 `json:"latency_ms"`
}
