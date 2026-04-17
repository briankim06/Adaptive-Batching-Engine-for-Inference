package worker

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/briankim06/adaptive-batching-engine/internal/config"
	"github.com/briankim06/adaptive-batching-engine/internal/models"
	"golang.org/x/sync/errgroup"
)

// WorkerInfo is a snapshot of a single worker's state, used by the admin
// endpoint and dashboard.
type WorkerInfo struct {
	ID               string       `json:"id"`
	Status           WorkerStatus `json:"status"`
	CircuitState     CircuitState `json:"circuit_state"`
	BatchesProcessed int64        `json:"batches_processed"`
	TokensProcessed  int64        `json:"tokens_processed"`
}

// WorkerPool owns N symmetric ProxyWorkers sharing four priority-indexed
// batchChans, their per-worker CircuitBreakers, a shared upstream-
// unhealthy flag, a shared sync.Pool of *bytes.Buffer encoders, and the
// single upstream health-ping goroutine. See docs/spec/03-workers.md §3.4.
type WorkerPool struct {
	workers           []Worker
	circuits          map[string]*CircuitBreaker
	batchChans        [4]chan *models.Batch
	upstream          config.UpstreamConfig
	httpClient        *http.Client
	bufPool           *sync.Pool
	upstreamUnhealthy atomic.Bool
	healthInterval    time.Duration
	failureThreshold  int
}

// NewWorkerPool constructs the pool, its workers, and their shared
// infrastructure. The batchChans are owned by this pool and handed to
// the batcher via BatchChans().
func NewWorkerPool(cfg config.WorkerConfig, healthCfg config.HealthConfig, upstream config.UpstreamConfig) *WorkerPool {
	count := cfg.Count
	if count < 1 {
		count = 1
	}

	var batchChans [4]chan *models.Batch
	for i := range batchChans {
		batchChans[i] = make(chan *models.Batch, count)
	}

	httpClient := &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        upstream.MaxIdleConns,
			MaxConnsPerHost:     upstream.MaxConnsPerHost,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
		},
	}

	bufPool := &sync.Pool{
		New: func() any { return new(bytes.Buffer) },
	}

	p := &WorkerPool{
		workers:          make([]Worker, 0, count),
		circuits:         make(map[string]*CircuitBreaker, count),
		batchChans:       batchChans,
		upstream:         upstream,
		httpClient:       httpClient,
		bufPool:          bufPool,
		healthInterval:   healthCfg.CheckInterval,
		failureThreshold: healthCfg.FailureThreshold,
	}
	p.upstreamUnhealthy.Store(false)

	var recvChans [4]<-chan *models.Batch
	for i, ch := range batchChans {
		recvChans[i] = ch
	}

	for i := 0; i < count; i++ {
		id := fmt.Sprintf("worker-%d", i)
		cb := NewCircuitBreaker(healthCfg.FailureThreshold, healthCfg.RecoveryTimeout)
		p.circuits[id] = cb
		w := NewProxyWorker(
			WorkerConfig{ID: id, MaxBatchTokens: cfg.MaxBatchTokens},
			upstream,
			httpClient,
			cb,
			&p.upstreamUnhealthy,
			bufPool,
			recvChans,
		)
		p.workers = append(p.workers, w)
	}

	return p
}

// Start launches every worker and the upstream health-ping goroutine via
// an errgroup. Blocks until ctx is cancelled and all goroutines exit.
func (p *WorkerPool) Start(ctx context.Context) error {
	g, gctx := errgroup.WithContext(ctx)
	for _, w := range p.workers {
		w := w
		g.Go(func() error {
			return w.Run(gctx)
		})
	}
	g.Go(func() error {
		p.pingLoop(gctx)
		return nil
	})
	return g.Wait()
}

// Stop is a convenience for callers that want to signal shutdown and wait.
// Typically main.go cancels the context passed to Start; this method
// exists for symmetry with the Batcher interface.
func (p *WorkerPool) Stop(_ context.Context) error {
	return nil
}

// AllWorkersUnhealthy returns true if the upstream is unreachable OR every
// per-worker circuit breaker is Open. The inference handler consults this
// at Submit time and responds 503 immediately when true.
func (p *WorkerPool) AllWorkersUnhealthy() bool {
	if p.upstreamUnhealthy.Load() {
		return true
	}
	for _, cb := range p.circuits {
		if cb.State() != CircuitOpen {
			return false
		}
	}
	return true
}

// GetStatus returns a snapshot of each worker's ID, status, circuit state,
// and processing stats.
func (p *WorkerPool) GetStatus() []WorkerInfo {
	infos := make([]WorkerInfo, 0, len(p.workers))
	for _, w := range p.workers {
		id := w.ID()
		info := WorkerInfo{
			ID:           id,
			Status:       w.Status(),
			CircuitState: p.circuits[id].State(),
		}
		if pw, ok := w.(*ProxyWorker); ok {
			info.BatchesProcessed = pw.BatchesProcessed()
			info.TokensProcessed = pw.TokensProcessed()
		}
		infos = append(infos, info)
	}
	return infos
}

// BatchChans returns the four priority-indexed channels. The batcher
// holds the send side; workers hold the receive side.
func (p *WorkerPool) BatchChans() [4]chan *models.Batch {
	return p.batchChans
}

// CloseBatchChans closes all four channels. Called by main.go during
// shutdown after the batcher's drain has completed so workers observe
// the closed channels and exit their select loops.
func (p *WorkerPool) CloseBatchChans() {
	for _, ch := range p.batchChans {
		close(ch)
	}
}

// UpstreamUnhealthy returns the current upstream reachability flag.
func (p *WorkerPool) UpstreamUnhealthy() bool {
	return p.upstreamUnhealthy.Load()
}

// pingLoop is the single upstream health-ping goroutine. It flips
// upstreamUnhealthy true after failureThreshold consecutive failures
// and clears on any success. Per-worker circuit breakers are not
// touched. See invariant I3.
func (p *WorkerPool) pingLoop(ctx context.Context) {
	tick := time.NewTicker(p.healthInterval)
	defer tick.Stop()
	consecutiveFailures := 0
	url := p.upstream.URL + p.upstream.HealthPath
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			ok := ping(ctx, p.httpClient, url)
			if ok {
				p.upstreamUnhealthy.Store(false)
				consecutiveFailures = 0
				continue
			}
			consecutiveFailures++
			if consecutiveFailures >= p.failureThreshold {
				p.upstreamUnhealthy.Store(true)
			}
		}
	}
}

// ping sends a GET to url and returns true if the response is a non-5xx
// status within the context deadline.
func ping(ctx context.Context, client *http.Client, url string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode < 500
}
