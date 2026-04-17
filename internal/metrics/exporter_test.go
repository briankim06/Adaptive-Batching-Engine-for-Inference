package metrics

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestJSONSnapshot(t *testing.T) {
	c := NewCollector(100, 0.3)
	c.SetQueueDepth(7)

	data, err := JSONSnapshot(c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var snap MetricsSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if snap.QueueDepth != 7 {
		t.Fatalf("expected queue depth 7, got %d", snap.QueueDepth)
	}
}

func TestCSVExport(t *testing.T) {
	agg := NewTimeSeriesAggregator()
	agg.Add("csv_test", 10.0)
	agg.Add("csv_test", 20.0)

	data, err := CSVExport(agg, "csv_test", 5*time.Minute)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	csv := string(data)
	if !strings.HasPrefix(csv, "timestamp,value\n") {
		t.Fatalf("expected CSV header, got: %s", csv[:min(50, len(csv))])
	}
	lines := strings.Split(strings.TrimSpace(csv), "\n")
	if len(lines) != 3 { // header + 2 data rows
		t.Fatalf("expected 3 lines, got %d", len(lines))
	}
}

func TestCSVExportEmpty(t *testing.T) {
	agg := NewTimeSeriesAggregator()
	data, err := CSVExport(agg, "nonexistent", time.Minute)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(data) != "timestamp,value\n" {
		t.Fatalf("expected header-only CSV for empty metric, got: %s", string(data))
	}
}

func TestPrometheusHandler(t *testing.T) {
	handler := PrometheusHandler()
	if handler == nil {
		t.Fatal("expected non-nil handler")
	}
}
