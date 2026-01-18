package packing

import "github.com/yourname/adaptive-batching-engine/internal/models"

const tokenBucketCount = 6

type TokenGrouper struct {
	buckets [tokenBucketCount]chan *models.InferenceRequest
}

func NewTokenGrouper(capacity int) *TokenGrouper {
	if capacity <= 0 {
		capacity = 1
	}
	tg := &TokenGrouper{}
	for i := 0; i < tokenBucketCount; i++ {
		tg.buckets[i] = make(chan *models.InferenceRequest, capacity)
	}
	return tg
}

func (tg *TokenGrouper) Submit(req *models.InferenceRequest) {
	if req == nil {
		return
	}
	idx := req.TokenBucket()
	if idx < 0 {
		idx = 0
	}
	if idx >= tokenBucketCount {
		idx = tokenBucketCount - 1
	}
	tg.buckets[idx] <- req
}

func (tg *TokenGrouper) GetBatch(maxSize int) []*models.InferenceRequest {
	if maxSize <= 0 {
		return nil
	}

	bucketIndex, first := tg.tryReceiveAny()
	if first == nil {
		return nil
	}

	batch := []*models.InferenceRequest{first}
	for len(batch) < maxSize {
		select {
		case req, ok := <-tg.buckets[bucketIndex]:
			if !ok || req == nil {
				return batch
			}
			batch = append(batch, req)
		default:
			return batch
		}
	}
	return batch
}

func (tg *TokenGrouper) tryReceiveAny() (int, *models.InferenceRequest) {
	for i := 0; i < tokenBucketCount; i++ {
		select {
		case req, ok := <-tg.buckets[i]:
			if ok {
				return i, req
			}
		default:
		}
	}
	return -1, nil
}
