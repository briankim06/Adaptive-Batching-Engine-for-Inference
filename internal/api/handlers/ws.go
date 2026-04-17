package handlers

import (
	"context"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/briankim06/adaptive-batching-engine/internal/metrics"
)

// SnapshotPublisher serialises snapshot production: one background loop
// builds a fresh MetricsSnapshot every interval and publishes it via an
// atomic.Pointer. WebSocket handlers load the pointer lock-free. See
// invariant I6 and docs/spec/05-api.md Task 5.6.
type SnapshotPublisher struct {
	current atomic.Pointer[metrics.MetricsSnapshot]
}

// NewSnapshotPublisher seeds an empty snapshot so Current() never returns
// nil to a reader that starts before the first tick.
func NewSnapshotPublisher() *SnapshotPublisher {
	sp := &SnapshotPublisher{}
	empty := metrics.MetricsSnapshot{Timestamp: time.Now()}
	sp.current.Store(&empty)
	return sp
}

// Start launches the single publish loop. Safe to call exactly once.
func (sp *SnapshotPublisher) Start(ctx context.Context, c *metrics.Collector, interval time.Duration) {
	go sp.publishLoop(ctx, c, interval)
}

func (sp *SnapshotPublisher) publishLoop(ctx context.Context, c *metrics.Collector, interval time.Duration) {
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if c == nil {
				continue
			}
			snap := c.BuildSnapshot()
			sp.current.Store(&snap)
		}
	}
}

// Current returns the most recently published snapshot. Lock-free.
func (sp *SnapshotPublisher) Current() *metrics.MetricsSnapshot {
	return sp.current.Load()
}

// wsUpgrader accepts any origin — this gateway is a local-dev/backend
// component and the browser dashboard is not authenticated here.
var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 4096,
	CheckOrigin: func(_ *http.Request) bool {
		return true
	},
}

// HandleWebSocket upgrades the connection and pushes the latest snapshot
// every pushInterval. A peer read goroutine detects disconnect and
// cancels the write goroutine via a per-connection context.
func HandleWebSocket(publisher *SnapshotPublisher, pushInterval time.Duration) http.HandlerFunc {
	if pushInterval <= 0 {
		pushInterval = 500 * time.Millisecond
	}
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			// Upgrade already wrote the error response.
			return
		}
		defer conn.Close()

		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		// Reader goroutine — we don't expect client messages but we read
		// to observe disconnects, pongs, and close frames.
		go func() {
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					cancel()
					return
				}
			}
		}()

		ticker := time.NewTicker(pushInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if publisher == nil {
					return
				}
				snap := publisher.Current()
				if snap == nil {
					continue
				}
				_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
				if err := conn.WriteJSON(snap); err != nil {
					return
				}
			}
		}
	}
}
