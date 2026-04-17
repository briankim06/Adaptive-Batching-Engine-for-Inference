package metrics

import (
	"testing"
	"time"
)

// stubWorkerProvider implements WorkerInfoProvider for tests.
type stubWorkerProvider struct {
	snapshots []WorkerSnapshot
	unhealthy bool
}

func (s *stubWorkerProvider) GetWorkerSnapshots() []WorkerSnapshot { return s.snapshots }
func (s *stubWorkerProvider) UpstreamUnhealthy() bool              { return s.unhealthy }

func TestCollectorRecordAndSnapshot(t *testing.T) {
	c := NewCollector(100, 0.3)
	c.SetWorkerProvider(&stubWorkerProvider{
		snapshots: []WorkerSnapshot{
			{ID: "worker-0", Status: 0, CircuitState: 0},
		},
		unhealthy: false,
	})

	c.RecordRequest()
	c.RecordRequest()
	c.RecordRequest()
	c.RecordLatency(10.0)
	c.RecordLatency(20.0)
	c.SetQueueDepth(5)

	time.Sleep(10 * time.Millisecond)

	snap := c.BuildSnapshot()

	if snap.QueueDepth != 5 {
		t.Fatalf("expected queue depth 5, got %d", snap.QueueDepth)
	}
	if len(snap.Workers) != 1 {
		t.Fatalf("expected 1 worker snapshot, got %d", len(snap.Workers))
	}
	if snap.Workers[0].ID != "worker-0" {
		t.Fatalf("expected worker-0, got %s", snap.Workers[0].ID)
	}
	if !snap.UpstreamOK {
		t.Fatal("expected UpstreamOK true")
	}
	if snap.ThroughputRPS <= 0 {
		t.Fatalf("expected positive throughput, got %v", snap.ThroughputRPS)
	}
}

func TestCollectorSnapshotImmutability(t *testing.T) {
	c := NewCollector(100, 0.3)
	c.SetWorkerProvider(&stubWorkerProvider{
		snapshots: []WorkerSnapshot{
			{ID: "worker-0"},
			{ID: "worker-1"},
		},
	})

	snap1 := c.BuildSnapshot()
	workers1 := snap1.Workers

	// Mutate the returned slice; the next snapshot must not be affected.
	workers1[0].ID = "mutated"

	snap2 := c.BuildSnapshot()
	if snap2.Workers[0].ID == "mutated" {
		t.Fatal("snapshot workers are not independently allocated (I6 violation)")
	}
}

func TestCollectorUpstreamUnhealthy(t *testing.T) {
	c := NewCollector(100, 0.3)
	c.SetWorkerProvider(&stubWorkerProvider{unhealthy: true})

	snap := c.BuildSnapshot()
	if snap.UpstreamOK {
		t.Fatal("expected UpstreamOK false when upstream is unhealthy")
	}
}

func TestCollectorNoWorkerProvider(t *testing.T) {
	c := NewCollector(100, 0.3)
	snap := c.BuildSnapshot()
	if snap.Workers == nil {
		t.Fatal("expected non-nil Workers even without provider")
	}
	if len(snap.Workers) != 0 {
		t.Fatalf("expected empty workers, got %d", len(snap.Workers))
	}
	if !snap.UpstreamOK {
		t.Fatal("expected UpstreamOK true with no provider")
	}
}

func TestCollectorRecordDispatchBlocked(t *testing.T) {
	c := NewCollector(100, 0.3)
	// Should not panic.
	c.RecordDispatchBlocked("critical")
	c.RecordDispatchBlocked("low")
}

func TestCollectorSetWorkerStatus(t *testing.T) {
	c := NewCollector(100, 0.3)
	c.SetWorkerStatus("worker-0", 1) // busy
	c.SetCircuitState("worker-0", 0) // closed
	// Verify via readGauge on the underlying metrics.
	val := readGauge(promWorkerUtilization.WithLabelValues("worker-0"))
	if val != 1 {
		t.Fatalf("expected worker utilization 1, got %v", val)
	}
}

func TestCollectorSetUpstreamUnhealthy(t *testing.T) {
	c := NewCollector(100, 0.3)
	c.SetUpstreamUnhealthy(true)
	if v := readGauge(promUpstreamUnhealthy); v != 1 {
		t.Fatalf("expected upstream_unhealthy=1, got %v", v)
	}
	c.SetUpstreamUnhealthy(false)
	if v := readGauge(promUpstreamUnhealthy); v != 0 {
		t.Fatalf("expected upstream_unhealthy=0, got %v", v)
	}
}

func TestCollectorRecordBatch(t *testing.T) {
	c := NewCollector(100, 0.3)
	c.RecordBatch(8)
	c.RecordBatch(16)
	// Should not panic; histogram observation is fire-and-forget.
}

func TestCollectorRecordError(t *testing.T) {
	c := NewCollector(100, 0.3)
	c.RecordError()
	c.RecordError()
	// Should not panic.
}
