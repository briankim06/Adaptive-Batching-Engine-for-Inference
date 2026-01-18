package models

import (
	"testing"
	"time"
)

func TestRequestType(t *testing.T) {
	tests := []struct {
		name     string
		reqType  RequestType
		expected string
	}{
		{"completion type", RequestTypeCompletion, "completion"},
		{"embedding type", RequestTypeEmbedding, "embedding"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if string(tt.reqType) != tt.expected {
				t.Errorf("expected %s, got %s", tt.expected, string(tt.reqType))
			}
		})
	}
}

func TestPriorityMaxWaitMs(t *testing.T) {
	tests := []struct {
		priority Priority
		expected int
	}{
		{PriorityCritical, 5},
		{PriorityHigh, 20},
		{PriorityNormal, 50},
		{PriorityLow, 100},
		{Priority(999), 50}, // Unknown priority defaults to 50
	}

	for _, tt := range tests {
		t.Run(tt.priority.String(), func(t *testing.T) {
			result := tt.priority.MaxWaitMs()
			if result != tt.expected {
				t.Errorf("expected %d, got %d", tt.expected, result)
			}
		})
	}
}

func TestPriorityString(t *testing.T) {
	tests := []struct {
		priority Priority
		expected string
	}{
		{PriorityLow, "low"},
		{PriorityNormal, "normal"},
		{PriorityHigh, "high"},
		{PriorityCritical, "critical"},
		{Priority(999), "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			result := tt.priority.String()
			if result != tt.expected {
				t.Errorf("expected %s, got %s", tt.expected, result)
			}
		})
	}
}

func TestNewInferenceRequest(t *testing.T) {
	prompt := "Test prompt with some words"
	maxTokens := 100
	priority := PriorityHigh
	reqType := RequestTypeCompletion

	req := NewInferenceRequest(prompt, maxTokens, priority, reqType)

	if req.ID == "" {
		t.Error("expected non-empty ID")
	}
	if req.Prompt != prompt {
		t.Errorf("expected prompt %s, got %s", prompt, req.Prompt)
	}
	if req.MaxTokens != maxTokens {
		t.Errorf("expected maxTokens %d, got %d", maxTokens, req.MaxTokens)
	}
	if req.Priority != priority {
		t.Errorf("expected priority %d, got %d", priority, req.Priority)
	}
	if req.RequestType != reqType {
		t.Errorf("expected request type %s, got %s", reqType, req.RequestType)
	}
	if req.CreatedAt.IsZero() {
		t.Error("expected non-zero CreatedAt")
	}
	if req.ResultChan == nil {
		t.Error("expected non-nil ResultChan")
	}
	if cap(req.ResultChan) != 1 {
		t.Errorf("expected ResultChan capacity 1, got %d", cap(req.ResultChan))
	}

	// Test EstimatedTokens calculation: len(prompt)/4 + maxTokens
	expectedTokens := len(prompt)/4 + maxTokens
	if req.EstimatedTokens != expectedTokens {
		t.Errorf("expected EstimatedTokens %d, got %d", expectedTokens, req.EstimatedTokens)
	}
}

func TestInferenceRequestAgeMs(t *testing.T) {
	req := NewInferenceRequest("test", 10, PriorityNormal, RequestTypeCompletion)
	
	time.Sleep(10 * time.Millisecond)
	
	age := req.AgeMs()
	if age < 10 {
		t.Errorf("expected age >= 10ms, got %f", age)
	}
	if age > 100 {
		t.Errorf("expected age < 100ms (sanity check), got %f", age)
	}
}

func TestInferenceRequestTokenBucket(t *testing.T) {
	tests := []struct {
		name            string
		estimatedTokens int
		expectedBucket  int
	}{
		{"bucket 0 (<=128)", 50, 0},
		{"bucket 0 exact", 128, 0},
		{"bucket 1 (<=512)", 200, 1},
		{"bucket 1 exact", 512, 1},
		{"bucket 2 (<=1024)", 800, 2},
		{"bucket 2 exact", 1024, 2},
		{"bucket 3 (<=2048)", 1500, 3},
		{"bucket 3 exact", 2048, 3},
		{"bucket 4 (<=4096)", 3000, 4},
		{"bucket 4 exact", 4096, 4},
		{"overflow bucket", 5000, 5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &InferenceRequest{EstimatedTokens: tt.estimatedTokens}
			bucket := req.TokenBucket()
			if bucket != tt.expectedBucket {
				t.Errorf("expected bucket %d, got %d", tt.expectedBucket, bucket)
			}
		})
	}
}

func TestNewBatch(t *testing.T) {
	req1 := NewInferenceRequest("test1", 10, PriorityNormal, RequestTypeCompletion)
	req2 := NewInferenceRequest("test2", 20, PriorityHigh, RequestTypeCompletion)
	requests := []*InferenceRequest{req1, req2}
	strategy := "test_strategy"

	batch := NewBatch(requests, strategy)

	if batch.ID == "" {
		t.Error("expected non-empty batch ID")
	}
	if len(batch.Requests) != 2 {
		t.Errorf("expected 2 requests, got %d", len(batch.Requests))
	}
	if batch.StrategyUsed != strategy {
		t.Errorf("expected strategy %s, got %s", strategy, batch.StrategyUsed)
	}
	if batch.CreatedAt.IsZero() {
		t.Error("expected non-zero CreatedAt")
	}
}

func TestBatchSize(t *testing.T) {
	req1 := NewInferenceRequest("test1", 10, PriorityNormal, RequestTypeCompletion)
	req2 := NewInferenceRequest("test2", 20, PriorityHigh, RequestTypeCompletion)
	
	batch := NewBatch([]*InferenceRequest{req1, req2}, "test")
	
	if batch.Size() != 2 {
		t.Errorf("expected size 2, got %d", batch.Size())
	}

	emptyBatch := NewBatch([]*InferenceRequest{}, "test")
	if emptyBatch.Size() != 0 {
		t.Errorf("expected size 0, got %d", emptyBatch.Size())
	}
}

func TestBatchTotalTokens(t *testing.T) {
	req1 := &InferenceRequest{EstimatedTokens: 100}
	req2 := &InferenceRequest{EstimatedTokens: 200}
	req3 := &InferenceRequest{EstimatedTokens: 50}
	
	batch := NewBatch([]*InferenceRequest{req1, req2, req3}, "test")
	
	expected := 350
	if batch.TotalTokens() != expected {
		t.Errorf("expected total tokens %d, got %d", expected, batch.TotalTokens())
	}

	emptyBatch := NewBatch([]*InferenceRequest{}, "test")
	if emptyBatch.TotalTokens() != 0 {
		t.Errorf("expected total tokens 0, got %d", emptyBatch.TotalTokens())
	}
}

func TestBatchMaxPriority(t *testing.T) {
	tests := []struct {
		name       string
		priorities []Priority
		expected   Priority
	}{
		{
			name:       "mixed priorities",
			priorities: []Priority{PriorityLow, PriorityHigh, PriorityNormal},
			expected:   PriorityHigh,
		},
		{
			name:       "all same priority",
			priorities: []Priority{PriorityNormal, PriorityNormal, PriorityNormal},
			expected:   PriorityNormal,
		},
		{
			name:       "critical priority present",
			priorities: []Priority{PriorityLow, PriorityCritical, PriorityNormal},
			expected:   PriorityCritical,
		},
		{
			name:       "single request",
			priorities: []Priority{PriorityHigh},
			expected:   PriorityHigh,
		},
		{
			name:       "empty batch",
			priorities: []Priority{},
			expected:   PriorityNormal, // Default for empty batch
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var requests []*InferenceRequest
			for _, p := range tt.priorities {
				requests = append(requests, &InferenceRequest{Priority: p})
			}
			
			batch := NewBatch(requests, "test")
			result := batch.MaxPriority()
			
			if result != tt.expected {
				t.Errorf("expected max priority %d (%s), got %d (%s)", 
					tt.expected, tt.expected.String(), result, result.String())
			}
		})
	}
}

func TestRequestResult(t *testing.T) {
	result := &RequestResult{
		RequestID: "test-id",
		Result:    map[string]string{"key": "value"},
		Error:     nil,
		LatencyMs: 15.5,
	}

	if result.RequestID != "test-id" {
		t.Errorf("expected RequestID 'test-id', got %s", result.RequestID)
	}
	if result.LatencyMs != 15.5 {
		t.Errorf("expected LatencyMs 15.5, got %f", result.LatencyMs)
	}
}

func TestBatchResult(t *testing.T) {
	now := time.Now()
	result := &BatchResult{
		BatchID:          "batch-123",
		Results:          []any{"result1", "result2"},
		ProcessingTimeMs: 25.5,
		WorkerID:         "worker-0",
		CompletedAt:      now,
		Error:            nil,
	}

	if result.BatchID != "batch-123" {
		t.Errorf("expected BatchID 'batch-123', got %s", result.BatchID)
	}
	if len(result.Results) != 2 {
		t.Errorf("expected 2 results, got %d", len(result.Results))
	}
	if result.ProcessingTimeMs != 25.5 {
		t.Errorf("expected ProcessingTimeMs 25.5, got %f", result.ProcessingTimeMs)
	}
	if result.WorkerID != "worker-0" {
		t.Errorf("expected WorkerID 'worker-0', got %s", result.WorkerID)
	}
	if !result.CompletedAt.Equal(now) {
		t.Errorf("expected CompletedAt %v, got %v", now, result.CompletedAt)
	}
}

func TestSentinelErrors(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected string
	}{
		{"queue full", ErrQueueFull, "request queue is full"},
		{"worker unavailable", ErrWorkerUnavailable, "no workers available"},
		{"circuit open", ErrCircuitOpen, "circuit breaker is open"},
		{"batch timeout", ErrBatchTimeout, "batch processing timed out"},
		{"invalid request", ErrInvalidRequest, "invalid inference request"},
		{"shutting down", ErrShuttingDown, "server is shutting down"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.err == nil {
				t.Error("expected non-nil error")
			}
			if tt.err.Error() != tt.expected {
				t.Errorf("expected error message %s, got %s", tt.expected, tt.err.Error())
			}
		})
	}
}
