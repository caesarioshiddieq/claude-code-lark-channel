# Design Spec: Supervisor Metrics (M1b)

**Date**: 2026-04-23
**Status**: Approved (pending implementation plan)
**Owner**: Caesario Shiddieq
**Reviewed by**: Codex (gpt-5.3-codex), advisor() — 2026-04-23

## Goal

Expose SQL-derived application metrics from the `claude-code-vm` supervisor, forwarded to GCP Cloud Monitoring via the already-installed Ops Agent. The primary operational goal is to have sufficient observability to safely bump `MAX_CONCURRENT_SPAWNS_GLOBAL` from 1 → 2.

## Non-goals

- Alerting / pager rules — separate follow-up
- Multi-task config (M2) — separate feature
- Historical time-series outside Cloud Monitoring's retention
- A standalone Prometheus server (we use Cloud Monitoring, not Prometheus proper)

## Architecture

```
┌─ worker.Spawn (spawn.go) ──────────┐
│  start := time.Now()                │
│  result := claude -p                │
│  dur := time.Since(start).ms        │──┐
└─────────────────────────────────────┘  │
                                         ▼
                              ┌─ InsertTurnUsage (queue.go) ─┐
                              │  writes row to sqlite with   │
                              │  new spawn_duration_ms col   │
                              └──────────────────────────────┘
                                         │
                                         │ (60s scrape interval)
                                         ▼
┌─ Ops Agent ─── scrapes localhost:9090/metrics ──┐
│ prometheus receiver                              │
│ → Cloud Monitoring custom metrics                │
└─────────────────────────────────────────────────┘
                                         │
                                         ▼
                              ┌─ Cloud Monitoring dashboard ─┐
                              │ (JSON-declared, git-tracked)  │
                              └───────────────────────────────┘
                                         │
                                         ▼
                                   Operator eyes
```

## Components

### 1. New Go package `internal/metrics/`

**Files:**
- `metrics.go` — `Collector` struct implementing `prometheus.Collector`, plus a `Handler(db *sqlite.DB) http.Handler` constructor that returns a pre-wired `promhttp.Handler`.
- `metrics_test.go` — seed rows in test DB, hit handler via `httptest`, assert output.

**Public API:**
```go
type Collector struct {
    db            *sqlite.DB
    descs         collectorDescs // pre-built *prometheus.Desc values (name+labels metadata only)
    scrapeErrors  *prometheus.CounterVec // labeled by query name (spawns/tokens/dlq/...); long-lived in-memory
}

// NewCollector creates the Collector. It pre-builds metric Descs but holds no
// per-scrape values — every Collect() call runs fresh SQL and emits fresh metrics.
func NewCollector(db *sqlite.DB) *Collector

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc)

// Collect implements prometheus.Collector. On each scrape it runs the SQL
// aggregates and emits metrics via prometheus.MustNewConstMetric and
// prometheus.MustNewConstHistogram. No accumulated histogram state.
func (c *Collector) Collect(ch chan<- prometheus.Metric)

// Handler returns an http.Handler that exposes the collector's metrics via
// promhttp.HandlerFor on a private Registry (registered at construction).
func Handler(c *Collector) http.Handler
```

**Lifecycle invariant**: the `Collector` is created once at supervisor startup and reused. It holds no mutable Go-level histogram state — bucket counts are computed from SQL each `Collect()` call and emitted via `prometheus.MustNewConstHistogram(desc, count, sum, buckets)`. `scrapeErrors` is the only persistent in-memory value and is a monotonic counter by design.

**Metric inventory:**

| Metric | Type | Labels | SQL Query |
|---|---|---|---|
| `supervisor_spawns_last_30d` | Gauge | `phase` | `SELECT phase, COUNT(*) FROM turn_usage WHERE created_at > (strftime('%s','now','-30 days') * 1000) GROUP BY phase` |
| `supervisor_tokens_last_30d` | Gauge | `kind` (input/output/cache_read/cache_creation) | `SELECT SUM(input_tokens), SUM(output_tokens), SUM(cache_read_tokens), SUM(cache_creation_tokens) FROM turn_usage WHERE created_at > (strftime('%s','now','-30 days') * 1000)` |
| `supervisor_dlq_size` | Gauge | — | `SELECT COUNT(*) FROM dlq` |
| `supervisor_inbox_backlog` | Gauge | — | `SELECT COUNT(*) FROM inbox WHERE processed_at IS NULL` |
| `supervisor_spawn_duration_seconds` | Histogram | `phase` | Bucket-aggregate SQL (see Histogram note below); `WHERE spawn_duration_ms IS NOT NULL AND created_at > now - 1h` |
| `supervisor_metrics_scrape_errors_total` | Counter | `query` (spawns/tokens/dlq/inbox_backlog/spawn_duration/panic) | In-memory, incremented per query on failure |

**Histogram note:** `spawn_duration_seconds` buckets = `[5, 10, 30, 60, 120, 300]` seconds. Computed each `Collect()` call from `turn_usage.spawn_duration_ms` rows inserted in the last 1 hour. Implementation uses `prometheus.MustNewConstHistogram(desc, sampleCount, sampleSum, bucketCountsMap)` — bucket counts are derived in a single SQL query (`SELECT CASE WHEN spawn_duration_ms < 5000 THEN '5' ... END bucket, COUNT(*), SUM(spawn_duration_ms) FROM turn_usage WHERE spawn_duration_ms IS NOT NULL AND created_at > now - 1h GROUP BY bucket`). No Go-level histogram state accumulates; each scrape reflects the same 1h window.

**Naming rationale (`_last_30d` over `_total`):** The `usage_gc` cron deletes `turn_usage` rows older than 30 days at 03:00 WIB. If we named these `_total` and backed them with `COUNT(*)`, the post-GC drop would look like a counter reset to Prometheus/Cloud Monitoring, creating false spikes in `rate()`/`increase()` calculations. Gauge naming is honest.

### 2. Supervisor wiring in `cmd/supervisor/main.go`

**Changes:**
- Read env `METRICS_ADDR` (default `127.0.0.1:9090`, empty string disables metrics)
- In `main()` after DB init, if enabled, spawn goroutine:
  ```go
  collector := metrics.NewCollector(db)
  srv := &http.Server{
      Addr:              metricsAddr,
      Handler:           metrics.Handler(collector),
      ReadHeaderTimeout: 5 * time.Second,
      WriteTimeout:      10 * time.Second,
  }
  go func() {
      <-ctx.Done()
      shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
      defer cancel()
      _ = srv.Shutdown(shutdownCtx)
  }()
  go func() {
      if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
          log.Printf("[metrics] server exited: %v", err)
      }
  }()
  ```
- Log effective address at startup: `[metrics] listening on 127.0.0.1:9090`

### 3. `turn_usage.spawn_duration_ms` column (migration0004)

**Migration SQL:**
```sql
ALTER TABLE turn_usage ADD COLUMN spawn_duration_ms INTEGER;
```

**Backfill:** Not needed — pre-migration rows remain NULL and are excluded from histogram aggregation by `WHERE spawn_duration_ms IS NOT NULL`.

**Struct change:**
```go
type TurnUsage struct {
    CommentID           string
    TaskID              string
    SessionUUID         string
    Phase               string
    InputTokens         int
    OutputTokens        int
    CacheReadTokens     int
    CacheCreationTokens int
    IsRateLimitError    bool
    SpawnDurationMs     int64 // NEW
}
```

### 4. Worker wiring (`internal/worker/spawn.go`)

`SpawnClaudeWithUsage` returns duration alongside the existing result:
```go
type SpawnResult struct {
    Reply                string
    InputTokens          int
    // ... existing fields
    IsRateLimit          bool
    SpawnDurationMs      int64 // NEW
}
```

Implementation: `start := time.Now()` before `exec.CommandContext(...).Run()`, `result.SpawnDurationMs = time.Since(start).Milliseconds()` before return. Caller in `dispatchNormal`/`dispatchCompact`/`dispatchAnswer` passes this through to `InsertTurnUsage`.

### 5. Ops Agent config fragment

New file `infra/ops-agent-prometheus-receiver.yaml` (git-tracked, to be appended to `/etc/google-cloud-ops-agent/config.yaml` on the VM):

```yaml
metrics:
  receivers:
    claude_vm_supervisor:
      type: prometheus
      config:
        scrape_configs:
          - job_name: claude-vm-supervisor
            scrape_interval: 60s
            static_configs:
              - targets: ['localhost:9090']
  service:
    pipelines:
      claude_vm_metrics:
        receivers: [claude_vm_supervisor]
```

**Apply procedure** (documented in `infra/DEPLOY.md`):
```bash
# On VM: append the fragment to the existing config.yaml under `metrics:` / `service:` sections
sudo cp /etc/google-cloud-ops-agent/config.yaml /etc/google-cloud-ops-agent/config.yaml.prev
sudo vim /etc/google-cloud-ops-agent/config.yaml  # merge fragment
sudo systemctl restart google-cloud-ops-agent
sudo journalctl -u google-cloud-ops-agent -n 50
```

### 6. Cloud Monitoring dashboard

New file `infra/cloud-monitoring-dashboard.json` — 6 widgets:

| Row | Type | Metric / Expression |
|---|---|---|
| 1 | Line chart | `prometheus.googleapis.com/supervisor_spawns_last_30d/gauge` grouped by `phase` |
| 1 | Line chart | `prometheus.googleapis.com/supervisor_tokens_last_30d/gauge` grouped by `kind` |
| 2 | Scorecard | `supervisor_inbox_backlog` (threshold: warn > 10, danger > 50) |
| 2 | Scorecard | `supervisor_dlq_size` (threshold: warn > 0) |
| 3 | Heatmap / line | `supervisor_spawn_duration_seconds` histogram — show p50/p95 overlay |
| 4 | Line chart (2 series) | `compute.googleapis.com/instance/memory/utilization` + `/cpu/utilization` |

Apply: `gcloud monitoring dashboards create --config-from-file=infra/cloud-monitoring-dashboard.json --project=devsecops-480902`

### 7. Dependency additions (`go.mod`)

- `github.com/prometheus/client_golang` (Apache-2.0, adds ~1MB to binary, transitively pulls `prometheus/common` + `golang/protobuf`)

## Error handling

**Partial-failure rule** (authoritative): each SQL aggregate runs independently. On individual failure (timeout or error), `scrape_errors_total` is incremented (labeled with the query name), that metric family is **omitted** from the scrape output, and Collect proceeds to the next query. HTTP status depends on whether ANY metric family succeeded:

- **At least one family succeeded** → `200 OK` with the succeeded families only (promhttp default behavior)
- **All families failed** → Collector emits only `scrape_errors_total` + sets a response header `X-Scrape-Status: all-failed`; the handler wrapper inspects the Registry's output and returns **503** when no `supervisor_*` business metric was emitted
- **Handler panic** → recovered by `promhttp` middleware; `scrape_errors_total++`; returns 500

| Failure | Response |
|---|---|
| DB query timeout (2s via `context.WithTimeout`) | `scrape_errors_total{query="spawns"}++`, omit that family, continue (per partial-failure rule) |
| Individual SQL query error | Wrap with `fmt.Errorf("metrics: %s: %w", queryName, err)`, log once per 60s per query, `scrape_errors_total{query=...}++`, omit that family |
| ALL queries fail in one scrape | 503 response, all families absent, only `scrape_errors_total` visible |
| HTTP bind failure at startup | Log `[metrics] bind %s: %v` and **exit** — misconfig should surface immediately |
| Collector `Collect()` panic | Recovered by promhttp; `scrape_errors_total{query="panic"}++`; returns 500 |
| `METRICS_ADDR` unset | Default `127.0.0.1:9090`; empty string explicitly disables metrics goroutine |

## Testing strategy

### Unit tests (`internal/metrics/metrics_test.go`)

Table-driven coverage for `Collector.Collect`:

1. **Empty DB** — all gauges should be 0, no errors
2. **Seeded turn_usage** — spawns_last_30d grouped correctly by phase; tokens_last_30d sums correct
3. **Non-zero DLQ** — dlq_size reports count
4. **Inbox backlog** — only unprocessed rows counted (mixed processed_at=NULL / NOT NULL seed)
5. **Histogram buckets** — seed 5 rows with durations [3000, 8000, 25000, 90000, 350000]ms, assert bucket counts match
6. **Query timeout path** — inject a blocking query via test-only hook, assert 503 + `scrape_errors_total++`

### Fault-injection tests (per Codex Refactor #8)

In `internal/metrics/metrics_fault_test.go` (new file, `_test.go` naming keeps them excluded from prod):

1. **GC during scrape** — `go DeleteOldTurnUsage()`, immediately `go Collect()`; assert scrape doesn't error, result is deterministic (either pre or post-GC state, not partial)
2. **Concurrent writes + scrape** — 4 goroutines hammer `InsertHumanInbox`/`InsertTurnUsage` while scrape runs; assert scrape completes within 2s budget
3. **Scrape timeout → 503** — already in unit tests #6

Target: 80%+ coverage on `internal/metrics/`. All new tests pass with `-race`.

### Integration test (manual, post-deploy)

```bash
# On VM
curl -s http://localhost:9090/metrics | head -30
# Expect: HELP/TYPE headers + all 6 metric families present

# In Cloud Monitoring console
# Metrics Explorer → filter: metric.type = starts_with("prometheus.googleapis.com/supervisor_")
# Expect: 6 supervisor_* metric families with recent data points
```

## Rollout plan

1. Merge feature branch `feat/supervisor-metrics` via PR (subject to standard code review + advisor second-opinion)
2. Build + deploy binary to `claude-code-vm` per existing deploy runbook
3. SSH to VM, apply Ops Agent config fragment, restart agent
4. Verify `curl localhost:9090/metrics` returns expected format
5. Apply Cloud Monitoring dashboard JSON
6. Observe for 24h — verify metrics ingest, dashboard renders, no scrape_errors spikes
7. Only then: consider bumping `MAX_CONCURRENT_SPAWNS_GLOBAL=2` in `/home/caesario/.config/claude-vm/env`

## Rollback plan

- **Supervisor**: revert to `.prev` binary on VM (`/home/caesario/bin/claude-vm-supervisor.prev`)
- **Ops Agent config**: revert `/etc/google-cloud-ops-agent/config.yaml.prev` → restart
- **Dashboard**: delete via `gcloud monitoring dashboards delete`
- **Migration**: no rollback needed — `spawn_duration_ms` NULL rows are harmless to the old binary

## Cost projection

- `prometheus/client_golang` library: $0 (open source)
- No Prometheus server: we forward directly to Cloud Monitoring
- Cloud Monitoring ingestion: ~28 time series × 60s scrape = ~1.2M data points/month ≈ 120–140 MB/month
- Free tier: 150 MB/project/month custom metrics
- **Expected monthly cost: $0**. Worst case (if ingestion byte-count exceeds expectations): ~$3–8/month.

## Risks

| Risk | Mitigation |
|---|---|
| DB scrape contention under N=4 concurrent spawns | 2s query timeout + scrape_errors counter + 60s scrape interval. Tiny DB means queries are sub-ms even with contention. |
| Cardinality explosion from a future label change | Code review rule: never add `task_id` or `comment_id` as labels. Enforced by spec + grep-guard in CI (future). |
| Ops Agent misconfig on VM | Fragment file is git-tracked; config.yaml.prev backup during rollout; journalctl smoke test after restart. |
| Histogram re-observation on every scrape inflates counts | `prometheus.MustNewConstHistogram` pattern: bucket counts derived from SQL per `Collect()`, no accumulated Go-level state. Tested in metrics_test.go #5. |

## Implementation scope summary

| File | Change |
|---|---|
| `internal/metrics/metrics.go` | **New** (~150 LOC) |
| `internal/metrics/metrics_test.go` | **New** (~200 LOC) |
| `internal/metrics/metrics_fault_test.go` | **New** (~80 LOC) |
| `internal/sqlite/queue.go` | migration0004 + `TurnUsage.SpawnDurationMs` + `InsertTurnUsage` sig |
| `internal/sqlite/queue_test.go` | Migration test + updated InsertTurnUsage tests |
| `internal/worker/spawn.go` | `time.Since(start)` capture in `SpawnResult` |
| `cmd/supervisor/main.go` | `METRICS_ADDR` env read, http.Server goroutine, pass duration to InsertTurnUsage in all 3 dispatch paths |
| `infra/ops-agent-prometheus-receiver.yaml` | **New** — git-tracked config fragment |
| `infra/cloud-monitoring-dashboard.json` | **New** — git-tracked dashboard definition |
| `infra/DEPLOY.md` | + section for Ops Agent config apply + dashboard create |
| `.claude/CLAUDE.md` | + `METRICS_ADDR` in Key env vars list |
| `go.mod` / `go.sum` | + `github.com/prometheus/client_golang` |

Estimated: ~400 LOC (half tests), ~1–1.5 day implementation.

## Related

- Handoff that proposed the feature: `.claude/handoffs/2026-04-23-200121-worker-pool-shipped.md`
- Brainstorming session: this spec was produced from a 2026-04-23 brainstorm including Codex (gpt-5.3-codex) design review
- Prior design doc referenced: `.claude/plans/claude-vm-token-budget-design.md` (labels M1/M2 first introduced there)
- Ops Agent installed on VM 2026-04-23 at 13:16 UTC
