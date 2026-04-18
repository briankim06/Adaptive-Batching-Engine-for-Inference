// Package benchmark exercises the hot paths of the batching layer so
// regressions show up in CI. These benchmarks do not call out to the
// network; every measurement is in-process so the numbers are comparable
// across runs.
package benchmark

import (
	"context"
	"math/rand"
	"sync/atomic"
	"testing"
	"time"

	"github.com/briankim06/adaptive-batching-engine/internal/batcher"
	"github.com/briankim06/adaptive-batching-engine/internal/batcher/packing"
	"github.com/briankim06/adaptive-batching-engine/internal/batcher/strategies"
	"github.com/briankim06/adaptive-batching-engine/internal/metrics"
	"github.com/briankim06/adaptive-batching-engine/internal/models"
)

func BenchmarkQueueMatrixSubmit(b *testing.B) {
	qm := packing.NewQueueMatrix(100000)
	stop := make(chan struct{})
	defer close(stop)

	// Background drainer so per-sub-queue capacity never saturates during
	// the benchmark. We must consume eventChan too or Submit's wake-up
	// send can coalesce away valid notifications.
	go func() {
		events := qm.EventChan()
		for {
			select {
			case <-stop:
				return
			case <-events:
			}
			for {
				reqs, _, _, _ := qm.Drain(1024)
				if len(reqs) == 0 {
					break
				}
			}
		}
	}()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		r := rand.New(rand.NewSource(time.Now().UnixNano()))
		for pb.Next() {
			req := &models.InferenceRequest{
				ID:              "bench",
				Priority:        models.Priority(r.Intn(4)),
				RequestType:     models.RequestTypeCompletion,
				EstimatedTokens: 1 + r.Intn(8192),
				ResultChan:      make(chan *models.RequestResult, 1),
			}
			if r.Intn(2) == 0 {
				req.RequestType = models.RequestTypeEmbedding
			}
			_ = qm.Submit(req)
		}
	})
}

func BenchmarkBatcherSubmit(b *testing.B) {
	batchCfg := batcher.BatcherConfig{
		MinBatchSize:  1,
		MaxBatchSize:  128,
		QueueCapacity: 200000,
		BatchChanSize: 64,
	}
	var batchChans [4]chan *models.Batch
	for i := range batchChans {
		batchChans[i] = make(chan *models.Batch, batchCfg.BatchChanSize)
	}
	strategy := strategies.NewFixedStrategy(5, batchCfg.MaxBatchSize)
	ad := batcher.NewAdaptiveBatcher(batchCfg, strategy, batchChans)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Background drainers mirror the production workers so the batcher
	// loop never wedges on a full batchChan.
	for i := range batchChans {
		ch := batchChans[i]
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case _, ok := <-ch:
					if !ok {
						return
					}
				}
			}
		}()
	}
	go func() { _ = ad.Start(ctx) }()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		r := rand.New(rand.NewSource(time.Now().UnixNano()))
		for pb.Next() {
			req := models.NewInferenceRequest(
				context.Background(),
				"p", 8,
				models.Priority(r.Intn(4)),
				models.RequestTypeCompletion,
			)
			_ = ad.Submit(context.Background(), req)
		}
	})
	b.StopTimer()
	cancel()
	_ = ad.Stop(context.Background())
}

func BenchmarkBatchFormation(b *testing.B) {
	const prefill = 1024
	qm := packing.NewQueueMatrix(prefill * 2)
	for i := 0; i < prefill; i++ {
		req := models.NewInferenceRequest(
			context.Background(),
			"p", 8,
			models.Priority(i%4),
			models.RequestTypeCompletion,
		)
		_ = qm.Submit(req)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		reqs, _, _, _ := qm.Drain(64)
		if len(reqs) == 0 {
			b.StopTimer()
			// Refill between drain exhaustion and the next iteration.
			for j := 0; j < prefill; j++ {
				req := models.NewInferenceRequest(
					context.Background(),
					"p", 8,
					models.Priority(j%4),
					models.RequestTypeCompletion,
				)
				_ = qm.Submit(req)
			}
			b.StartTimer()
		}
	}
}

func BenchmarkLatencyTrackerAdd(b *testing.B) {
	lt := metrics.NewLatencyTracker(10000, 0.3)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lt.Start(ctx, 10*time.Millisecond)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		r := rand.New(rand.NewSource(time.Now().UnixNano()))
		for pb.Next() {
			lt.Add(r.Float64() * 100)
		}
	})
}

func BenchmarkLatencyTrackerP99(b *testing.B) {
	lt := metrics.NewLatencyTracker(10000, 0.3)
	for i := 0; i < 10000; i++ {
		lt.Add(float64(i))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lt.Start(ctx, 10*time.Millisecond)
	time.Sleep(120 * time.Millisecond) // let the ticker materialise atomics

	var sink float64
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sink += lt.P99()
	}
	b.StopTimer()
	// Publish a side-effect so the compiler does not DCE the loop.
	atomic.StoreUint64(&benchmarkLatencySink, uint64(sink))
}

var benchmarkLatencySink uint64
