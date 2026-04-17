package metrics

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// PrometheusHandler returns the standard promhttp handler for the
// /metrics endpoint.
func PrometheusHandler() http.Handler {
	return promhttp.Handler()
}

// JSONSnapshot marshals the collector's BuildSnapshot to JSON.
func JSONSnapshot(collector *Collector) ([]byte, error) {
	snap := collector.BuildSnapshot()
	return json.Marshal(snap)
}

// CSVExport returns CSV bytes with columns timestamp,value for the named
// metric over the requested duration.
func CSVExport(agg *TimeSeriesAggregator, metricName string, duration time.Duration) ([]byte, error) {
	points := agg.GetHistory(metricName, duration)
	var buf bytes.Buffer
	buf.WriteString("timestamp,value\n")
	for _, p := range points {
		fmt.Fprintf(&buf, "%s,%f\n", p.Timestamp.Format(time.RFC3339Nano), p.Value)
	}
	return buf.Bytes(), nil
}
