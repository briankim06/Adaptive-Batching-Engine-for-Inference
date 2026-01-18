package packing

import (
	"testing"

	"github.com/yourname/adaptive-batching-engine/internal/models"
)

func TestTokenGrouperBatchSameBucket(t *testing.T) {
	tg := NewTokenGrouper(10)
	req1 := &models.InferenceRequest{EstimatedTokens: 200}
	req2 := &models.InferenceRequest{EstimatedTokens: 300}

	tg.Submit(req1)
	tg.Submit(req2)

	batch := tg.GetBatch(3)
	if len(batch) != 2 {
		t.Fatalf("expected batch size 2, got %d", len(batch))
	}
	if batch[0] != req1 || batch[1] != req2 {
		t.Error("expected batch to contain submitted requests in order")
	}
}

func TestTokenGrouperFallbackBucket(t *testing.T) {
	tg := NewTokenGrouper(10)
	req := &models.InferenceRequest{EstimatedTokens: 800}

	tg.Submit(req)

	batch := tg.GetBatch(2)
	if len(batch) != 1 {
		t.Fatalf("expected batch size 1, got %d", len(batch))
	}
	if batch[0] != req {
		t.Error("expected batch to contain the submitted request")
	}
}

func TestTokenGrouperPrefersLowestBucket(t *testing.T) {
	tg := NewTokenGrouper(10)
	reqLow := &models.InferenceRequest{EstimatedTokens: 50}
	reqHigh := &models.InferenceRequest{EstimatedTokens: 800}

	tg.Submit(reqHigh)
	tg.Submit(reqLow)

	batch := tg.GetBatch(2)
	if len(batch) != 1 {
		t.Fatalf("expected batch size 1, got %d", len(batch))
	}
	if batch[0] != reqLow {
		t.Error("expected batch to prefer lowest available bucket")
	}
}
