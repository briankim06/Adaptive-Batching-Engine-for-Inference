# Phase 6 — Dashboard

A single-page HTML/JS/CSS application that connects to the API server's
WebSocket endpoint and renders real-time system metrics. A minimal
static-file server binary serves the page.

Tasks: 6.1 Dashboard Frontend · 6.2 Dashboard Server

Shared references:
- [shared/configuration.md](./shared/configuration.md) — `dashboard:` section.

Depends on: Phase 5 (consumes `/ws`, `MetricsSnapshot` JSON schema).

---

## Dashboard Panels

The dashboard displays:

1. **Queue Depth** — line chart, last 60 seconds.
2. **Latency Percentiles** — line chart with P50, P90, P99 lines.
3. **Throughput (RPS)** — line chart.
4. **Batch Size Distribution** — bar chart.
5. **Worker Status** — colored tiles per worker (green = idle, yellow =
   busy, red = unhealthy) with circuit breaker state.
6. **Active Strategy** — text display with current strategy name.
7. **System Stats** — counters: total requests, total errors, uptime.

### Task 6.1 — Dashboard Frontend

```
Create dashboard/static/index.html:

A single-file HTML page with embedded CSS and JS (no build step).

Requirements:
  - Connects to ws://<api-host>/ws on load. The URL comes from a
    query string or template variable injected by cmd/dashboard
    (default ws://localhost:8080/ws).
  - Parses incoming MetricsSnapshot JSON (see 04-metrics.md Task 4.2).
  - Renders all 7 panels described above.
  - Charts: use Canvas 2D API or a lightweight charting lib loaded via CDN
    (e.g. Chart.js). Keep a rolling 60-second window for line charts.
  - Worker tiles update color based on status.
  - Reconnects automatically on WebSocket disconnect (exponential backoff,
    max 10s).
  - Responsive layout that works at 1200px+ width.

Style: clean, utilitarian, dark theme. Monospace font for numbers. No
framework dependencies. The dashboard is a monitoring tool — prioritize
readability and information density over visual flair.
```

### Task 6.2 — Dashboard Server

```
Create cmd/dashboard/main.go:

A minimal static-file server that:
  - Serves dashboard/static/ on / via http.FileServer.
  - Listens on cfg.Dashboard.Port (default 8081).
  - Accepts --api-url flag to configure the API server address injected
    into index.html at serve time via a query string or template variable
    (default "ws://localhost:8080/ws").

The dashboard frontend connects directly to the API server's /ws endpoint
rather than proxying through this binary — the API server already owns
the SnapshotPublisher and CORS on /ws is permissive (allow all origins)
for local dev. If cfg.Dashboard.Enabled is false, cmd/dashboard is not
run.
```

---

## What Phase 6 delivers

A runnable `cmd/dashboard` binary serving a browser-facing monitoring
UI. No behaviour changes to the API server; this is pure observability.
