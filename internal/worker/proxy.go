package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/briankim06/adaptive-batching-engine/internal/config"
	"github.com/briankim06/adaptive-batching-engine/internal/models"
)

// ProxyWorker forwards formed batches to the upstream inference server
// over HTTP and fans per-request results back to each request's
// ResultChan. See docs/spec/03-workers.md §3.3.
type ProxyWorker struct {
	workerConfig WorkerConfig
	batchChans   [4]<-chan *models.Batch
	httpClient   *http.Client
	upstream     config.UpstreamConfig
	circuit      *CircuitBreaker
	unhealthy    *atomic.Bool
	bufPool      *sync.Pool
	status       atomic.Int32
	stats        struct {
		batchesProcessed atomic.Int64
		tokensProcessed  atomic.Int64
	}
}

// NewProxyWorker constructs a worker. The batchChans are receive-only;
// the pool owns the send side.
func NewProxyWorker(
	cfg WorkerConfig,
	upstream config.UpstreamConfig,
	httpClient *http.Client,
	circuit *CircuitBreaker,
	unhealthy *atomic.Bool,
	bufPool *sync.Pool,
	batchChans [4]<-chan *models.Batch,
) *ProxyWorker {
	w := &ProxyWorker{
		workerConfig: cfg,
		batchChans:   batchChans,
		httpClient:   httpClient,
		upstream:     upstream,
		circuit:      circuit,
		unhealthy:    unhealthy,
		bufPool:      bufPool,
	}
	w.status.Store(int32(WorkerIdle))
	return w
}

func (w *ProxyWorker) ID() string { return w.workerConfig.ID }

func (w *ProxyWorker) Status() WorkerStatus {
	return WorkerStatus(w.status.Load())
}

// BatchesProcessed returns the number of batches this worker has sent
// upstream. Exposed for pool-level status snapshots.
func (w *ProxyWorker) BatchesProcessed() int64 {
	return w.stats.batchesProcessed.Load()
}

// TokensProcessed returns the cumulative token count across all batches.
func (w *ProxyWorker) TokensProcessed() int64 {
	return w.stats.tokensProcessed.Load()
}

// Run implements the priority-ordered pull loop. It blocks until ctx is
// cancelled or any batch channel returns !ok (closed during shutdown).
func (w *ProxyWorker) Run(ctx context.Context) error {
	crit := w.batchChans[models.PriorityCritical]
	high := w.batchChans[models.PriorityHigh]
	norm := w.batchChans[models.PriorityNormal]
	low := w.batchChans[models.PriorityLow]

	for {
		var batch *models.Batch
		var ok bool
		select {
		case batch, ok = <-crit:
		default:
			select {
			case batch, ok = <-crit:
			case batch, ok = <-high:
			default:
				select {
				case batch, ok = <-crit:
				case batch, ok = <-high:
				case batch, ok = <-norm:
				default:
					select {
					case batch, ok = <-crit:
					case batch, ok = <-high:
					case batch, ok = <-norm:
					case batch, ok = <-low:
					case <-ctx.Done():
						return nil
					}
				}
			}
		}
		if !ok {
			return nil
		}
		w.process(ctx, batch)
	}
}

func (w *ProxyWorker) process(ctx context.Context, batch *models.Batch) {
	w.status.Store(int32(WorkerBusy))
	defer w.status.Store(int32(WorkerIdle))

	if w.unhealthy.Load() || !w.circuit.CanExecute() {
		failBatch(batch, models.ErrWorkerUnavailable)
		return
	}

	scrubbed := filterCancelled(batch)
	if len(scrubbed.Requests) == 0 {
		return
	}

	buf := w.bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer w.bufPool.Put(buf)
	if err := json.NewEncoder(buf).Encode(scrubbed); err != nil {
		fanOutError(scrubbed, err)
		return
	}

	reqCtx, cancel := context.WithTimeout(ctx, w.upstream.RequestTimeout)
	defer cancel()
	stopWatch := spawnAllCancelledWatcher(reqCtx, scrubbed.Requests, cancel)
	defer stopWatch()

	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, w.upstream.URL, bytes.NewReader(buf.Bytes()))
	if err != nil {
		w.circuit.RecordFailure()
		fanOutError(scrubbed, models.ErrWorkerUnavailable)
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.ContentLength = int64(buf.Len())

	resp, err := w.httpClient.Do(httpReq)
	if err != nil || resp.StatusCode >= 500 {
		w.circuit.RecordFailure()
		fanOutError(scrubbed, models.ErrWorkerUnavailable)
		if resp != nil {
			resp.Body.Close()
		}
		return
	}
	defer resp.Body.Close()
	w.circuit.RecordSuccess()

	results := decodeBatchResponse(resp.Body)
	fanOutResults(scrubbed, results, w.workerConfig.ID)

	w.stats.batchesProcessed.Add(1)
	w.stats.tokensProcessed.Add(int64(scrubbed.TotalTokens()))
}

// filterCancelled implements the R3 pre-send context scan. Requests whose
// Ctx is already cancelled are removed from the outgoing batch and receive
// a cancellation result via non-blocking send.
func filterCancelled(batch *models.Batch) *models.Batch {
	live := make([]*models.InferenceRequest, 0, len(batch.Requests))
	for _, req := range batch.Requests {
		if req.Ctx != nil && req.Ctx.Err() != nil {
			select {
			case req.ResultChan <- &models.RequestResult{
				RequestID: req.ID,
				Error:     req.Ctx.Err(),
				LatencyMs: float64(time.Since(req.CreatedAt).Milliseconds()),
			}:
			default:
			}
			continue
		}
		live = append(live, req)
	}
	return &models.Batch{
		ID:           batch.ID,
		Requests:     live,
		CreatedAt:    batch.CreatedAt,
		StrategyUsed: batch.StrategyUsed,
	}
}

// fanOutResults delivers per-request results, re-checking each request's
// Ctx before sending (R3 post-response checkpoint). All sends are
// non-blocking per invariant I2.
func fanOutResults(batch *models.Batch, results []models.RequestResult, workerID string) {
	for i, req := range batch.Requests {
		if req.Ctx != nil && req.Ctx.Err() != nil {
			continue
		}
		var res *models.RequestResult
		if i < len(results) {
			r := results[i]
			res = &r
		} else {
			res = &models.RequestResult{
				RequestID: req.ID,
				LatencyMs: float64(time.Since(req.CreatedAt).Milliseconds()),
			}
		}
		res.RequestID = req.ID
		res.LatencyMs = float64(time.Since(req.CreatedAt).Milliseconds())
		select {
		case req.ResultChan <- res:
		default:
		}
	}
}

// fanOutError writes an error result to every request's ResultChan.
// Non-blocking per invariant I2.
func fanOutError(batch *models.Batch, err error) {
	for _, req := range batch.Requests {
		select {
		case req.ResultChan <- &models.RequestResult{
			RequestID: req.ID,
			Error:     err,
			LatencyMs: float64(time.Since(req.CreatedAt).Milliseconds()),
		}:
		default:
		}
	}
}

// failBatch is an alias for fanOutError used in the unhealthy/circuit-open
// short-circuit path.
func failBatch(batch *models.Batch, err error) {
	fanOutError(batch, err)
}

// decodeBatchResponse unmarshals the upstream response body into a slice
// of RequestResult matching the order of the scrubbed batch's Requests.
func decodeBatchResponse(body io.Reader) []models.RequestResult {
	var results []models.RequestResult
	if err := json.NewDecoder(body).Decode(&results); err != nil {
		return nil
	}
	return results
}

// spawnAllCancelledWatcher launches a goroutine that calls cancel() once
// every request's Ctx.Done() has fired. Returns a stop function that the
// caller MUST defer. See invariant I5.
func spawnAllCancelledWatcher(ctx context.Context, reqs []*models.InferenceRequest, cancel context.CancelFunc) func() {
	stopCh := make(chan struct{})
	go func() {
		for _, r := range reqs {
			select {
			case <-r.Ctx.Done():
			case <-stopCh:
				return
			case <-ctx.Done():
				return
			}
		}
		cancel()
	}()
	return func() { close(stopCh) }
}
