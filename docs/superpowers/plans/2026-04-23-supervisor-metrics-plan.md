# Supervisor Metrics (M1b) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Expose 6 SQL-derived metrics from the `claude-code-vm` supervisor via a new `/metrics` HTTP endpoint, scraped by the already-installed Ops Agent and forwarded to GCP Cloud Monitoring — sufficient observability to safely bump `MAX_CONCURRENT_SPAWNS_GLOBAL` from 1 → 2.

**Architecture:** New `internal/metrics/` Go package implements `prometheus.Collector`. On each `Collect()` call, a `BEGIN DEFERRED` transaction runs independent SQL aggregates over existing tables (`turn_usage`, `inbox`, `dlq`) plus a phase-labeled bucket-aggregate for the `spawn_duration_seconds` histogram. Partial-failure rule: individual query errors increment `scrape_errors_total{query=...}` and omit that family (HTTP 200); only all-failed responses return 503. New `spawn_duration_ms` column (migration0004) timestamps each claude subprocess invocation. HTTP server runs on `127.0.0.1:9090` (env override via `METRICS_ADDR`).

**Tech Stack:** Go 1.26, `modernc.org/sqlite` (single-conn WAL mode), `github.com/prometheus/client_golang`, GCP Ops Agent (Prometheus receiver), Cloud Monitoring.

**Spec reference:** `docs/superpowers/specs/2026-04-23-supervisor-metrics.md` (commit `e993ffe`).

---

## File Structure

| File | Action | Responsibility |
|---|---|---|
| `internal/sqlite/queue.go` | Modify | Add `migration0004` (adds `spawn_duration_ms`) + `SpawnDurationMs` field on `TurnUsage` + include in `InsertTurnUsage` |
| `internal/sqlite/queue_test.go` | Modify | Add `TestMigration0004_NewColumn` + assert `InsertTurnUsage` persists the new field |
| `internal/worker/spawn.go` | Modify | Add `SpawnDurationMs` to `SpawnResult`; capture `time.Since(start)` in `SpawnClaudeWithUsage` |
| `internal/worker/spawn_test.go` | Modify | Assert `SpawnDurationMs` is set (> 0) by a test that stubs the claude CLI |
| `cmd/supervisor/main.go` | Modify | Pass `result.SpawnDurationMs` into `InsertTurnUsage` in `dispatchNormal`, `dispatchCompact`, `dispatchAnswer`; add `METRICS_ADDR` env read + metrics server goroutine |
| `internal/metrics/metrics.go` | Create | `Collector` struct + `Describe` + `Collect` (with `BEGIN DEFERRED`, per-query error handling, panic recovery) + `Handler` |
| `internal/metrics/metrics_test.go` | Create | 9 unit tests: empty DB, seeded, DLQ, backlog, histogram, single-query-timeout (200), all-fail (503), panic |
| `internal/metrics/metrics_fault_test.go` | Create | 2 fault-injection tests: GC during scrape, concurrent writes+scrape |
| `internal/metrics/export_test.go` | Create | Test-only injection helpers (`SetFailQueryForTest`, `PanicOnCollectForTest`) |
| `infra/ops-agent-prometheus-receiver.yaml` | Create | Fragment to be appended to `/etc/google-cloud-ops-agent/config.yaml` on VM |
| `infra/cloud-monitoring-dashboard.json` | Create | Declarative dashboard definition (6 widgets) |
| `infra/DEPLOY.md` | Modify | Append sections for Ops Agent config apply + dashboard create |
| `.claude/CLAUDE.md` | Modify | Append `METRICS_ADDR` to Key env vars list |
| `go.mod` / `go.sum` | Modify | `go get github.com/prometheus/client_golang@latest` |

---

## Task 0: Pre-flight — create feature branch

**Files:** none (git state only)

- [ ] **Step 1: Verify main is clean and up to date**

Run:
```bash
cd ~/Kerja/claude-code-lark-channel
git checkout main
git pull origin main
git status
```

Expected: `On branch main`, `nothing to commit, working tree clean`, HEAD at `e993ffe` or later.

- [ ] **Step 2: Create feature branch**

Run:
```bash
git checkout -b feat/supervisor-metrics
```

Expected: `Switched to a new branch 'feat/supervisor-metrics'`.

- [ ] **Step 3: Add prometheus dependency**

Run:
```bash
go get github.com/prometheus/client_golang@latest
go mod tidy
```

Expected: `go.mod` and `go.sum` updated. New indirect deps: `prometheus/common`, `prometheus/client_model`.

- [ ] **Step 4: Commit**

```bash
git add go.mod go.sum
git commit -m "chore: add prometheus/client_golang dependency for M1b metrics"
```

---

## Task 1: migration0004 — add `spawn_duration_ms` column

**Files:**
- Modify: `internal/sqlite/queue.go` (append `migration0004` + register in `migrate()`)
- Test: `internal/sqlite/queue_test.go` (add `TestMigration0004_NewColumn`)

- [ ] **Step 1: Write the failing test**

Append to `internal/sqlite/queue_test.go`:

```go
func TestMigration0004_NewColumn(t *testing.T) {
	db := openTestDB(t)
	var name string
	if err := db.RawDB().QueryRow(
		`SELECT name FROM pragma_table_info('turn_usage') WHERE name = 'spawn_duration_ms'`,
	).Scan(&name); err != nil {
		t.Errorf("turn_usage column spawn_duration_ms missing after migration 0004: %v", err)
	}
	if name != "spawn_duration_ms" {
		t.Errorf("got column name %q, want spawn_duration_ms", name)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -race -run TestMigration0004_NewColumn ./internal/sqlite/ -v`

Expected: FAIL — `turn_usage column spawn_duration_ms missing after migration 0004`.

- [ ] **Step 3: Add migration0004 to queue.go**

Append after `migration0003` (around `internal/sqlite/queue.go:51`):

```go
var migration0004 = []string{
	`ALTER TABLE turn_usage ADD COLUMN spawn_duration_ms INTEGER`,
}
```

- [ ] **Step 4: Register migration in `migrate()`**

Find the migrations slice in `internal/sqlite/queue.go` (around line 59):

```go
migrations := []struct {
    version int
    stmts   []string
}{
    {2, migration0002},
    {3, migration0003},
}
```

Replace with:

```go
migrations := []struct {
    version int
    stmts   []string
}{
    {2, migration0002},
    {3, migration0003},
    {4, migration0004},
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test -race -run TestMigration0004_NewColumn ./internal/sqlite/ -v`

Expected: PASS.

- [ ] **Step 6: Run full sqlite suite to ensure no regressions**

Run: `go test -race ./internal/sqlite/ -v`

Expected: All existing tests still pass.

- [ ] **Step 7: Commit**

```bash
git add internal/sqlite/queue.go internal/sqlite/queue_test.go
git commit -m "feat(sqlite): migration0004 adds spawn_duration_ms column to turn_usage"
```

---

## Task 2: Extend `TurnUsage` struct + `InsertTurnUsage`

**Files:**
- Modify: `internal/sqlite/queue.go` (`TurnUsage` struct + `InsertTurnUsage` body)
- Test: `internal/sqlite/queue_test.go` (add `TestInsertTurnUsage_PersistsSpawnDurationMs`)

- [ ] **Step 1: Write the failing test**

Append to `internal/sqlite/queue_test.go`:

```go
func TestInsertTurnUsage_PersistsSpawnDurationMs(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	u := sqlite.TurnUsage{
		CommentID:       "C1",
		TaskID:          "T1",
		SessionUUID:     "S1",
		Phase:           "normal",
		InputTokens:     100,
		OutputTokens:    50,
		SpawnDurationMs: 42000, // 42s
	}
	if err := db.InsertTurnUsage(ctx, u); err != nil {
		t.Fatalf("InsertTurnUsage: %v", err)
	}
	var got int64
	if err := db.RawDB().QueryRow(
		`SELECT spawn_duration_ms FROM turn_usage WHERE comment_id = ?`, u.CommentID,
	).Scan(&got); err != nil {
		t.Fatalf("scan spawn_duration_ms: %v", err)
	}
	if got != 42000 {
		t.Errorf("got spawn_duration_ms=%d, want 42000", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -race -run TestInsertTurnUsage_PersistsSpawnDurationMs ./internal/sqlite/ -v`

Expected: FAIL — struct field or SQL binding missing.

- [ ] **Step 3: Add field to `TurnUsage` struct**

In `internal/sqlite/queue.go`, find `type TurnUsage struct` (around line 428) and add field:

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
	SpawnDurationMs     int64 // NEW: 0 when unset (pre-migration behavior — stored as NULL)
}
```

- [ ] **Step 4: Update `InsertTurnUsage` SQL to include new column**

Replace the body of `InsertTurnUsage` (around line 440):

```go
func (d *DB) InsertTurnUsage(ctx context.Context, u TurnUsage) error {
	rl := 0
	if u.IsRateLimitError {
		rl = 1
	}
	var dur interface{}
	if u.SpawnDurationMs > 0 {
		dur = u.SpawnDurationMs
	} // else: leave NULL
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO turn_usage
		 (comment_id, task_id, session_uuid, phase,
		  input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
		  is_rate_limit_error, spawn_duration_ms, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		u.CommentID, u.TaskID, u.SessionUUID, u.Phase,
		u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheCreationTokens,
		rl, dur, time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("insert turn usage: %w", err)
	}
	return nil
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test -race -run TestInsertTurnUsage_PersistsSpawnDurationMs ./internal/sqlite/ -v`

Expected: PASS.

- [ ] **Step 6: Run full sqlite suite**

Run: `go test -race ./internal/sqlite/`

Expected: All tests pass.

- [ ] **Step 7: Commit**

```bash
git add internal/sqlite/queue.go internal/sqlite/queue_test.go
git commit -m "feat(sqlite): thread SpawnDurationMs through TurnUsage + InsertTurnUsage"
```

---

## Task 3: Capture spawn duration in worker

**Files:**
- Modify: `internal/worker/spawn.go` (`SpawnResult` struct + timer in `SpawnClaudeWithUsage`)
- Test: `internal/worker/spawn_test.go` (new test for duration capture)

- [ ] **Step 1: Write the failing test**

Append (or create) `internal/worker/spawn_test.go`:

```go
package worker_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/caesarioshiddieq/claude-code-lark-channel/internal/worker"
)

func TestSpawnClaudeWithUsage_CapturesDuration(t *testing.T) {
	// Stub the claude CLI with a tiny shell script that sleeps briefly and emits
	// valid JSON. This forces a measurable SpawnDurationMs.
	tmpDir := t.TempDir()
	stubPath := filepath.Join(tmpDir, "claude")
	script := `#!/bin/sh
sleep 0.1
printf '{"type":"result","subtype":"success","result":"hi","usage":{"input_tokens":1,"output_tokens":1,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}'
`
	if err := os.WriteFile(stubPath, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	oldPath := os.Getenv("PATH")
	t.Cleanup(func() { os.Setenv("PATH", oldPath) })
	os.Setenv("PATH", tmpDir+string(os.PathListSeparator)+oldPath)

	ctx := context.Background()
	result, err := worker.SpawnClaudeWithUsage(ctx, "session-uuid", true, "hi")
	if err != nil {
		t.Fatalf("SpawnClaudeWithUsage: %v", err)
	}
	if result.SpawnDurationMs < 100 {
		t.Errorf("SpawnDurationMs=%d, want >= 100 (sleep 0.1 in stub)", result.SpawnDurationMs)
	}
	if result.SpawnDurationMs > 5000 {
		t.Errorf("SpawnDurationMs=%d unexpectedly large", result.SpawnDurationMs)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -race -run TestSpawnClaudeWithUsage_CapturesDuration ./internal/worker/ -v`

Expected: FAIL — `SpawnDurationMs` not a field on `SpawnResult`.

- [ ] **Step 3: Add field to `SpawnResult`**

In `internal/worker/spawn.go` (around line 81):

```go
type SpawnResult struct {
	Reply               string
	InputTokens         int
	OutputTokens        int
	CacheReadTokens     int
	CacheCreationTokens int
	IsRateLimit         bool
	SpawnDurationMs     int64 // NEW: wall-clock duration of the claude subprocess
}
```

- [ ] **Step 4: Capture duration in `SpawnClaudeWithUsage`**

Replace the body (around line 148):

```go
func SpawnClaudeWithUsage(ctx context.Context, sessionUUID string, isNew bool, prompt string) (SpawnResult, error) {
	start := time.Now()
	args := []string{"-p", "--output-format", "json"}
	if isNew {
		args = append(args, "--session-id", sessionUUID)
	} else {
		args = append(args, "--resume", sessionUUID)
	}
	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Stdin = strings.NewReader(prompt)
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	out, err := cmd.Output()
	durMs := time.Since(start).Milliseconds()
	if err != nil {
		return SpawnResult{
			IsRateLimit:     rateLimitRe.Match(stderrBuf.Bytes()),
			SpawnDurationMs: durMs,
		}, fmt.Errorf("claude spawn: %w", err)
	}
	result, parseErr := ParseClaudeOutputWithUsage(out, stderrBuf.Bytes())
	result.SpawnDurationMs = durMs
	return result, parseErr
}
```

Note: `time` is already imported in this file.

- [ ] **Step 5: Run test to verify it passes**

Run: `go test -race -run TestSpawnClaudeWithUsage_CapturesDuration ./internal/worker/ -v`

Expected: PASS.

- [ ] **Step 6: Run full worker suite**

Run: `go test -race ./internal/worker/`

Expected: All tests pass.

- [ ] **Step 7: Commit**

```bash
git add internal/worker/spawn.go internal/worker/spawn_test.go
git commit -m "feat(worker): capture spawn duration in SpawnResult.SpawnDurationMs"
```

---

## Task 4: Thread duration through the supervisor's 3 dispatch paths

**Files:**
- Modify: `cmd/supervisor/main.go` (dispatchNormal, dispatchCompact, dispatchAnswer — pass `result.SpawnDurationMs` into `InsertTurnUsage`)

- [ ] **Step 1: Review current `InsertTurnUsage` call sites**

Run: `grep -n "InsertTurnUsage" cmd/supervisor/main.go`

Expected: 3 call sites (one per dispatch function). Note the line numbers.

- [ ] **Step 2: Update `dispatchNormal`**

Find the `InsertTurnUsage` call in `dispatchNormal` (around `main.go:243`). Replace:

```go
if usageErr := db.InsertTurnUsage(ctx, sqlite.TurnUsage{
    CommentID: row.CommentID, TaskID: row.TaskID, SessionUUID: sessionUUID, Phase: "normal",
    InputTokens: result.InputTokens, OutputTokens: result.OutputTokens,
    CacheReadTokens: result.CacheReadTokens, CacheCreationTokens: result.CacheCreationTokens,
    IsRateLimitError: result.IsRateLimit,
}); usageErr != nil {
```

With:

```go
if usageErr := db.InsertTurnUsage(ctx, sqlite.TurnUsage{
    CommentID: row.CommentID, TaskID: row.TaskID, SessionUUID: sessionUUID, Phase: "normal",
    InputTokens: result.InputTokens, OutputTokens: result.OutputTokens,
    CacheReadTokens: result.CacheReadTokens, CacheCreationTokens: result.CacheCreationTokens,
    IsRateLimitError: result.IsRateLimit,
    SpawnDurationMs:  result.SpawnDurationMs,
}); usageErr != nil {
```

- [ ] **Step 3: Update `dispatchCompact`**

Same pattern — add `SpawnDurationMs: result.SpawnDurationMs,` as the last field in the struct literal for the `InsertTurnUsage` call inside `dispatchCompact`.

- [ ] **Step 4: Update `dispatchAnswer`**

Same pattern — add `SpawnDurationMs: result.SpawnDurationMs,` as the last field in the struct literal for the `InsertTurnUsage` call inside `dispatchAnswer`.

- [ ] **Step 5: Verify build**

Run: `go build ./...`

Expected: clean build, no errors.

- [ ] **Step 6: Run full test suite**

Run: `go test -race ./...`

Expected: all tests pass (including prior tasks).

- [ ] **Step 7: Commit**

```bash
git add cmd/supervisor/main.go
git commit -m "feat(supervisor): persist SpawnDurationMs via all 3 dispatch paths"
```

---

## Task 5: Create `internal/metrics/` package skeleton

**Files:**
- Create: `internal/metrics/metrics.go`
- Create: `internal/metrics/metrics_test.go`

- [ ] **Step 1: Write the failing test for constructor**

Create `internal/metrics/metrics_test.go`:

```go
package metrics_test

import (
	"testing"

	"github.com/caesarioshiddieq/claude-code-lark-channel/internal/metrics"
	"github.com/caesarioshiddieq/claude-code-lark-channel/internal/sqlite"
)

func TestNewCollector_NonNil(t *testing.T) {
	db, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	c := metrics.NewCollector(db)
	if c == nil {
		t.Fatal("NewCollector returned nil")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -race ./internal/metrics/ -v`

Expected: FAIL — package `internal/metrics` does not exist.

- [ ] **Step 3: Create `internal/metrics/metrics.go` skeleton**

```go
// Package metrics exposes Prometheus-format supervisor metrics derived from
// SQLite state. A single Collector instance is created at supervisor startup
// and reused across scrapes; each Collect() call runs fresh SQL inside a
// BEGIN DEFERRED transaction for snapshot semantics.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	sqlite "github.com/caesarioshiddieq/claude-code-lark-channel/internal/sqlite"
)

type collectorDescs struct {
	spawnsLast30d *prometheus.Desc
	tokensLast30d *prometheus.Desc
	dlqSize       *prometheus.Desc
	inboxBacklog  *prometheus.Desc
	spawnDuration *prometheus.Desc
}

// Collector is a long-lived prometheus.Collector that runs SQL per scrape.
type Collector struct {
	db           *sqlite.DB
	descs        collectorDescs
	scrapeErrors *prometheus.CounterVec
}

// NewCollector returns a Collector with pre-built descriptors and a
// long-lived scrape_errors_total CounterVec. Holds no per-scrape state.
func NewCollector(db *sqlite.DB) *Collector {
	return &Collector{
		db: db,
		descs: collectorDescs{
			spawnsLast30d: prometheus.NewDesc(
				"supervisor_spawns_last_30d",
				"Count of claude spawns in the last 30 days, by phase.",
				[]string{"phase"}, nil,
			),
			tokensLast30d: prometheus.NewDesc(
				"supervisor_tokens_last_30d",
				"Sum of tokens consumed in the last 30 days, by kind.",
				[]string{"kind"}, nil,
			),
			dlqSize: prometheus.NewDesc(
				"supervisor_dlq_size",
				"Current count of dead-letter-queue rows. Healthy = 0.",
				nil, nil,
			),
			inboxBacklog: prometheus.NewDesc(
				"supervisor_inbox_backlog",
				"Current count of unprocessed inbox rows.",
				nil, nil,
			),
			spawnDuration: prometheus.NewDesc(
				"supervisor_spawn_duration_seconds",
				"Histogram of claude spawn wall-clock duration in seconds (last 1h), by phase.",
				[]string{"phase"}, nil,
			),
		},
		scrapeErrors: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "supervisor_metrics_scrape_errors_total",
				Help: "Count of per-query failures during metrics scrapes.",
			},
			[]string{"query"},
		),
	}
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.descs.spawnsLast30d
	ch <- c.descs.tokensLast30d
	ch <- c.descs.dlqSize
	ch <- c.descs.inboxBacklog
	ch <- c.descs.spawnDuration
	c.scrapeErrors.Describe(ch)
}

// Collect implements prometheus.Collector. Fleshed out in Tasks 6–11.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	// Placeholder — populated in subsequent tasks.
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -race -run TestNewCollector_NonNil ./internal/metrics/ -v`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/metrics/metrics.go internal/metrics/metrics_test.go
git commit -m "feat(metrics): scaffold internal/metrics package with Collector + Describe"
```

---

## Task 6: Implement `Collect()` — `spawns_last_30d` + `tokens_last_30d` gauges

**Files:**
- Modify: `internal/metrics/metrics.go` (add `Handler` + SQL helpers, fill in `Collect()`)
- Modify: `internal/metrics/metrics_test.go` (add test)

- [ ] **Step 1: Write the failing test**

Append to `internal/metrics/metrics_test.go`:

```go
import (
	"context"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"time"
	// plus the existing imports
)

func seedTurnUsage(t *testing.T, db *sqlite.DB, phase string, input, output int, ageDays int) {
	t.Helper()
	createdAt := time.Now().AddDate(0, 0, -ageDays).UnixMilli()
	ctx := context.Background()
	if _, err := db.RawDB().ExecContext(ctx, `INSERT INTO turn_usage
		(comment_id, task_id, session_uuid, phase, input_tokens, output_tokens,
		 cache_read_tokens, cache_creation_tokens, is_rate_limit_error, created_at)
		VALUES (?, ?, ?, ?, ?, ?, 0, 0, 0, ?)`,
		fmt.Sprintf("C-%d-%s", time.Now().UnixNano(), phase),
		"T1", "S1", phase, input, output, createdAt); err != nil {
		t.Fatal(err)
	}
}

func TestCollect_SpawnsAndTokensLast30d(t *testing.T) {
	db, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// 2 normal spawns within 30d, 1 compact within 30d, 1 normal OLDER than 30d.
	seedTurnUsage(t, db, "normal", 100, 50, 1)
	seedTurnUsage(t, db, "normal", 200, 100, 5)
	seedTurnUsage(t, db, "compact", 0, 10, 2)
	seedTurnUsage(t, db, "normal", 999, 999, 40)

	c := metrics.NewCollector(db)
	h := metrics.Handler(c)
	req := httptest.NewRequest("GET", "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	out := string(body)

	if !strings.Contains(out, `supervisor_spawns_last_30d{phase="normal"} 2`) {
		t.Errorf("expected spawns_last_30d{phase=normal}=2, got:\n%s", out)
	}
	if !strings.Contains(out, `supervisor_spawns_last_30d{phase="compact"} 1`) {
		t.Errorf("expected spawns_last_30d{phase=compact}=1, got:\n%s", out)
	}
	// tokens: kind=input = 100+200 = 300 (old row excluded)
	if !strings.Contains(out, `supervisor_tokens_last_30d{kind="input"} 300`) {
		t.Errorf("expected tokens_last_30d{kind=input}=300, got:\n%s", out)
	}
	// kind=output = 50+100+10 = 160
	if !strings.Contains(out, `supervisor_tokens_last_30d{kind="output"} 160`) {
		t.Errorf("expected tokens_last_30d{kind=output}=160, got:\n%s", out)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -race -run TestCollect_SpawnsAndTokensLast30d ./internal/metrics/ -v`

Expected: FAIL — `metrics.Handler` undefined AND empty `Collect()`.

- [ ] **Step 3: Add imports to metrics.go**

Merge these into the existing `import ()` block in `internal/metrics/metrics.go`:

```go
import (
	"context"
	"database/sql"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	sqlite "github.com/caesarioshiddieq/claude-code-lark-channel/internal/sqlite"
)
```

- [ ] **Step 4: Implement `Handler`**

Append to `internal/metrics/metrics.go`:

```go
// Handler returns an http.Handler that exposes the collector's metrics via
// a private Registry. Wrapped in Task 10 for partial-failure status codes.
func Handler(c *Collector) http.Handler {
	reg := prometheus.NewRegistry()
	reg.MustRegister(c)
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
}
```

- [ ] **Step 5: Implement `Collect()` with the first 2 gauges**

Replace the placeholder `Collect()` body:

```go
// Collect implements prometheus.Collector. Opens a BEGIN DEFERRED transaction
// for snapshot semantics, runs each aggregate independently, and emits metrics.
// Per-query errors increment scrape_errors_total{query=...} and skip that family.
// Task 11 adds a top-level defer recover() for panics.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	tx, err := c.db.RawDB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		for _, q := range []string{"spawns", "tokens", "dlq", "inbox_backlog", "spawn_duration"} {
			c.scrapeErrors.WithLabelValues(q).Inc()
		}
		c.scrapeErrors.Collect(ch)
		return
	}
	defer tx.Rollback() //nolint:errcheck

	c.collectSpawnsLast30d(ctx, tx, ch)
	c.collectTokensLast30d(ctx, tx, ch)
	// dlq, inbox_backlog, spawn_duration added in Task 7 and Task 8.

	if err := tx.Commit(); err != nil {
		c.scrapeErrors.WithLabelValues("commit").Inc()
	}
	c.scrapeErrors.Collect(ch)
}

const thirtyDaysMs = `(strftime('%s','now','-30 days') * 1000)`

func (c *Collector) collectSpawnsLast30d(ctx context.Context, tx *sql.Tx, ch chan<- prometheus.Metric) {
	rows, err := tx.QueryContext(ctx,
		`SELECT phase, COUNT(*) FROM turn_usage
		 WHERE created_at > `+thirtyDaysMs+`
		 GROUP BY phase`)
	if err != nil {
		c.scrapeErrors.WithLabelValues("spawns").Inc()
		return
	}
	defer rows.Close()
	for rows.Next() {
		var phase string
		var count float64
		if err := rows.Scan(&phase, &count); err != nil {
			c.scrapeErrors.WithLabelValues("spawns").Inc()
			return
		}
		ch <- prometheus.MustNewConstMetric(c.descs.spawnsLast30d, prometheus.GaugeValue, count, phase)
	}
	if err := rows.Err(); err != nil {
		c.scrapeErrors.WithLabelValues("spawns").Inc()
	}
}

func (c *Collector) collectTokensLast30d(ctx context.Context, tx *sql.Tx, ch chan<- prometheus.Metric) {
	var in, out, cacheRead, cacheCreate sql.NullInt64
	err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
		        COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(cache_creation_tokens),0)
		 FROM turn_usage WHERE created_at > `+thirtyDaysMs).Scan(&in, &out, &cacheRead, &cacheCreate)
	if err != nil {
		c.scrapeErrors.WithLabelValues("tokens").Inc()
		return
	}
	emit := func(kind string, v int64) {
		ch <- prometheus.MustNewConstMetric(c.descs.tokensLast30d, prometheus.GaugeValue, float64(v), kind)
	}
	emit("input", in.Int64)
	emit("output", out.Int64)
	emit("cache_read", cacheRead.Int64)
	emit("cache_creation", cacheCreate.Int64)
}
```

- [ ] **Step 6: Run test to verify it passes**

Run: `go test -race -run TestCollect_SpawnsAndTokensLast30d ./internal/metrics/ -v`

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/metrics/metrics.go internal/metrics/metrics_test.go
git commit -m "feat(metrics): spawns_last_30d + tokens_last_30d gauges via BEGIN DEFERRED"
```

---

## Task 7: Implement `dlq_size` + `inbox_backlog` gauges

**Files:**
- Modify: `internal/metrics/metrics.go` (add 2 collect helpers, wire into `Collect()`)
- Modify: `internal/metrics/metrics_test.go` (add test)

- [ ] **Step 1: Write the failing test**

Append to `internal/metrics/metrics_test.go`:

```go
func TestCollect_DLQAndBacklog(t *testing.T) {
	db, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	// 2 unprocessed inbox rows, 1 processed.
	_, err = db.RawDB().ExecContext(ctx, `INSERT INTO inbox
		(comment_id, task_id, content, creator_id, created_at, processed_at)
		VALUES ('C1','T1','hi','U1',1,NULL),('C2','T1','hi','U1',2,NULL),
		       ('C3','T1','hi','U1',3,1000)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.RawDB().ExecContext(ctx, `INSERT INTO dlq
		(comment_id, task_id, last_error, moved_at)
		VALUES ('C99','T1','boom',1)`)
	if err != nil {
		t.Fatal(err)
	}

	c := metrics.NewCollector(db)
	h := metrics.Handler(c)
	req := httptest.NewRequest("GET", "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	body, _ := io.ReadAll(rec.Body)
	out := string(body)

	if !strings.Contains(out, "supervisor_dlq_size 1") {
		t.Errorf("expected dlq_size=1, got:\n%s", out)
	}
	if !strings.Contains(out, "supervisor_inbox_backlog 2") {
		t.Errorf("expected inbox_backlog=2, got:\n%s", out)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -race -run TestCollect_DLQAndBacklog ./internal/metrics/ -v`

Expected: FAIL — gauges not emitted.

- [ ] **Step 3: Add collect helpers**

Append to `internal/metrics/metrics.go`:

```go
func (c *Collector) collectDLQSize(ctx context.Context, tx *sql.Tx, ch chan<- prometheus.Metric) {
	var count float64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM dlq`).Scan(&count); err != nil {
		c.scrapeErrors.WithLabelValues("dlq").Inc()
		return
	}
	ch <- prometheus.MustNewConstMetric(c.descs.dlqSize, prometheus.GaugeValue, count)
}

func (c *Collector) collectInboxBacklog(ctx context.Context, tx *sql.Tx, ch chan<- prometheus.Metric) {
	var count float64
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM inbox WHERE processed_at IS NULL`).Scan(&count); err != nil {
		c.scrapeErrors.WithLabelValues("inbox_backlog").Inc()
		return
	}
	ch <- prometheus.MustNewConstMetric(c.descs.inboxBacklog, prometheus.GaugeValue, count)
}
```

- [ ] **Step 4: Wire into `Collect()`**

In `Collect()`, after `c.collectTokensLast30d(ctx, tx, ch)`, add:

```go
	c.collectDLQSize(ctx, tx, ch)
	c.collectInboxBacklog(ctx, tx, ch)
```

- [ ] **Step 5: Run test**

Run: `go test -race -run TestCollect_DLQAndBacklog ./internal/metrics/ -v`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/metrics/metrics.go internal/metrics/metrics_test.go
git commit -m "feat(metrics): add dlq_size + inbox_backlog gauges"
```

---

## Task 8: Implement `spawn_duration_seconds` histogram (phase-labeled)

**Files:**
- Modify: `internal/metrics/metrics.go` (histogram collect helper)
- Modify: `internal/metrics/metrics_test.go` (histogram test)

- [ ] **Step 1: Write the failing test**

Append to `internal/metrics/metrics_test.go`:

```go
func TestCollect_SpawnDurationHistogram(t *testing.T) {
	db, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	now := time.Now().UnixMilli()
	// 5 rows, phase=normal. Durations (ms): 3000, 8000, 25000, 90000, 350000.
	// Buckets [5,10,30,60,120,300] seconds → expected cumulative counts:
	//   le=5:   1, le=10:  2, le=30:  3, le=60:  3,
	//   le=120: 4, le=300: 4, +Inf: 5
	durations := []int64{3000, 8000, 25000, 90000, 350000}
	for i, d := range durations {
		_, err := db.RawDB().ExecContext(ctx, `INSERT INTO turn_usage
			(comment_id, task_id, session_uuid, phase, input_tokens, output_tokens,
			 cache_read_tokens, cache_creation_tokens, is_rate_limit_error,
			 spawn_duration_ms, created_at)
			VALUES (?, 'T1', 'S1', 'normal', 1, 1, 0, 0, 0, ?, ?)`,
			fmt.Sprintf("C%d", i), d, now)
		if err != nil {
			t.Fatal(err)
		}
	}

	c := metrics.NewCollector(db)
	h := metrics.Handler(c)
	req := httptest.NewRequest("GET", "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	body, _ := io.ReadAll(rec.Body)
	out := string(body)

	wantLines := []string{
		`supervisor_spawn_duration_seconds_bucket{phase="normal",le="5"} 1`,
		`supervisor_spawn_duration_seconds_bucket{phase="normal",le="10"} 2`,
		`supervisor_spawn_duration_seconds_bucket{phase="normal",le="30"} 3`,
		`supervisor_spawn_duration_seconds_bucket{phase="normal",le="60"} 3`,
		`supervisor_spawn_duration_seconds_bucket{phase="normal",le="120"} 4`,
		`supervisor_spawn_duration_seconds_bucket{phase="normal",le="300"} 4`,
		`supervisor_spawn_duration_seconds_bucket{phase="normal",le="+Inf"} 5`,
		`supervisor_spawn_duration_seconds_count{phase="normal"} 5`,
	}
	for _, line := range wantLines {
		if !strings.Contains(out, line) {
			t.Errorf("missing %q in output:\n%s", line, out)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -race -run TestCollect_SpawnDurationHistogram ./internal/metrics/ -v`

Expected: FAIL — histogram not emitted.

- [ ] **Step 3: Add histogram collect helper**

Append to `internal/metrics/metrics.go`:

```go
var durationBuckets = []float64{5, 10, 30, 60, 120, 300}

func (c *Collector) collectSpawnDuration(ctx context.Context, tx *sql.Tx, ch chan<- prometheus.Metric) {
	rows, err := tx.QueryContext(ctx, `
		SELECT phase,
		       CASE
		         WHEN spawn_duration_ms < 5000    THEN 5
		         WHEN spawn_duration_ms < 10000   THEN 10
		         WHEN spawn_duration_ms < 30000   THEN 30
		         WHEN spawn_duration_ms < 60000   THEN 60
		         WHEN spawn_duration_ms < 120000  THEN 120
		         WHEN spawn_duration_ms < 300000  THEN 300
		         ELSE 999999
		       END AS bucket,
		       COUNT(*) AS n,
		       SUM(spawn_duration_ms) AS sum_ms
		FROM turn_usage
		WHERE spawn_duration_ms IS NOT NULL
		  AND created_at > (strftime('%s','now','-1 hour') * 1000)
		GROUP BY phase, bucket`)
	if err != nil {
		c.scrapeErrors.WithLabelValues("spawn_duration").Inc()
		return
	}
	defer rows.Close()

	type phaseData struct {
		counts map[float64]uint64 // bucket upper-bound → non-cumulative count at THAT bucket
		total  uint64
		sumMs  int64
	}
	byPhase := make(map[string]*phaseData)

	for rows.Next() {
		var phase string
		var bucket float64
		var n uint64
		var sumMs int64
		if err := rows.Scan(&phase, &bucket, &n, &sumMs); err != nil {
			c.scrapeErrors.WithLabelValues("spawn_duration").Inc()
			return
		}
		pd, ok := byPhase[phase]
		if !ok {
			pd = &phaseData{counts: make(map[float64]uint64)}
			byPhase[phase] = pd
		}
		pd.counts[bucket] = n
		pd.total += n
		pd.sumMs += sumMs
	}
	if err := rows.Err(); err != nil {
		c.scrapeErrors.WithLabelValues("spawn_duration").Inc()
		return
	}

	for phase, pd := range byPhase {
		cumBuckets := make(map[float64]uint64, len(durationBuckets))
		var cum uint64
		for _, b := range durationBuckets {
			cum += pd.counts[b]
			cumBuckets[b] = cum
		}
		sumSec := float64(pd.sumMs) / 1000.0
		ch <- prometheus.MustNewConstHistogram(
			c.descs.spawnDuration,
			pd.total,
			sumSec,
			cumBuckets,
			phase,
		)
	}
}
```

- [ ] **Step 4: Wire into `Collect()`**

After `c.collectInboxBacklog(ctx, tx, ch)`, add:

```go
	c.collectSpawnDuration(ctx, tx, ch)
```

- [ ] **Step 5: Run test**

Run: `go test -race -run TestCollect_SpawnDurationHistogram ./internal/metrics/ -v`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/metrics/metrics.go internal/metrics/metrics_test.go
git commit -m "feat(metrics): phase-labeled spawn_duration_seconds histogram"
```

---

## Task 9: Empty-DB smoke test

**Files:**
- Modify: `internal/metrics/metrics_test.go`

- [ ] **Step 1: Add the empty-DB test**

```go
func TestCollect_EmptyDB(t *testing.T) {
	db, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	c := metrics.NewCollector(db)
	h := metrics.Handler(c)
	req := httptest.NewRequest("GET", "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	out := string(body)

	if !strings.Contains(out, "supervisor_dlq_size 0") {
		t.Errorf("missing dlq_size=0:\n%s", out)
	}
	if !strings.Contains(out, "supervisor_inbox_backlog 0") {
		t.Errorf("missing inbox_backlog=0:\n%s", out)
	}
	for _, kind := range []string{"input", "output", "cache_read", "cache_creation"} {
		want := fmt.Sprintf(`supervisor_tokens_last_30d{kind="%s"} 0`, kind)
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "# TYPE supervisor_metrics_scrape_errors_total counter") {
		t.Errorf("missing scrape_errors TYPE header:\n%s", out)
	}
}
```

- [ ] **Step 2: Run test**

Run: `go test -race -run TestCollect_EmptyDB ./internal/metrics/ -v`

Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/metrics/metrics_test.go
git commit -m "test(metrics): empty-DB smoke — gauges emit zeros, scrape_errors family present"
```

---

## Task 10: Partial-failure (200) + Total-failure (503) paths

**Files:**
- Create: `internal/metrics/export_test.go`
- Modify: `internal/metrics/metrics.go` (add `failQuery` field, per-helper gate, Handler wrapper)
- Modify: `internal/metrics/metrics_test.go` (2 tests)

- [ ] **Step 1: Create `export_test.go` with a test-only injection hook**

Create `internal/metrics/export_test.go`:

```go
package metrics

// SetFailQueryForTest forces the named query (e.g. "spawns") to error on the
// next Collect() call. Test-only: lives in _test.go so it's excluded from prod.
func (c *Collector) SetFailQueryForTest(query string) {
	c.failQuery = query
}
```

- [ ] **Step 2: Add `failQuery` field**

In `internal/metrics/metrics.go`, update the `Collector` struct:

```go
type Collector struct {
	db           *sqlite.DB
	descs        collectorDescs
	scrapeErrors *prometheus.CounterVec
	failQuery    string // test-only; empty in production
}
```

- [ ] **Step 3: Add the fail-gate at the top of each helper**

In each of `collectSpawnsLast30d`, `collectTokensLast30d`, `collectDLQSize`, `collectInboxBacklog`, `collectSpawnDuration`, add the first line (using the matching query name per helper):

```go
// collectSpawnsLast30d — prepend:
if c.failQuery == "spawns" {
	c.scrapeErrors.WithLabelValues("spawns").Inc()
	return
}

// collectTokensLast30d — prepend:
if c.failQuery == "tokens" {
	c.scrapeErrors.WithLabelValues("tokens").Inc()
	return
}

// collectDLQSize — prepend:
if c.failQuery == "dlq" {
	c.scrapeErrors.WithLabelValues("dlq").Inc()
	return
}

// collectInboxBacklog — prepend:
if c.failQuery == "inbox_backlog" {
	c.scrapeErrors.WithLabelValues("inbox_backlog").Inc()
	return
}

// collectSpawnDuration — prepend:
if c.failQuery == "spawn_duration" {
	c.scrapeErrors.WithLabelValues("spawn_duration").Inc()
	return
}
```

- [ ] **Step 4: Replace `Handler` with a wrapper that enforces 503-on-empty**

In `internal/metrics/metrics.go`, replace the existing `Handler`:

```go
// Handler returns an http.Handler exposing the collector's metrics. Implements
// the partial-failure rule:
//   - At least one supervisor_* business family emitted → 200
//   - No business family emitted → 503
func Handler(c *Collector) http.Handler {
	reg := prometheus.NewRegistry()
	reg.MustRegister(c)
	base := promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := &responseBuffer{headers: http.Header{}}
		base.ServeHTTP(buf, r)

		for k, v := range buf.headers {
			w.Header()[k] = v
		}
		body := buf.body.Bytes()
		hasBusiness := bytes.Contains(body, []byte("supervisor_spawns_last_30d")) ||
			bytes.Contains(body, []byte("supervisor_tokens_last_30d")) ||
			bytes.Contains(body, []byte("supervisor_dlq_size")) ||
			bytes.Contains(body, []byte("supervisor_inbox_backlog")) ||
			bytes.Contains(body, []byte("supervisor_spawn_duration_seconds"))
		if !hasBusiness {
			w.WriteHeader(http.StatusServiceUnavailable)
		} else {
			if buf.status == 0 {
				buf.status = http.StatusOK
			}
			w.WriteHeader(buf.status)
		}
		_, _ = w.Write(body)
	})
}

type responseBuffer struct {
	body    bytes.Buffer
	headers http.Header
	status  int
}

func (b *responseBuffer) Header() http.Header { return b.headers }
func (b *responseBuffer) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	return b.body.Write(p)
}
func (b *responseBuffer) WriteHeader(code int) { b.status = code }
```

Add `"bytes"` to the imports.

- [ ] **Step 5: Write partial-failure test**

Append to `metrics_test.go`:

```go
func TestCollect_PartialFailureReturns200(t *testing.T) {
	db, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedTurnUsage(t, db, "normal", 10, 20, 1)

	c := metrics.NewCollector(db)
	c.SetFailQueryForTest("spawns")
	h := metrics.Handler(c)
	req := httptest.NewRequest("GET", "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Errorf("status=%d, want 200", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	out := string(body)
	if strings.Contains(out, "supervisor_spawns_last_30d{") {
		t.Errorf("spawns family should be absent after inject-fail:\n%s", out)
	}
	if !strings.Contains(out, "supervisor_tokens_last_30d") {
		t.Errorf("tokens family missing:\n%s", out)
	}
	if !strings.Contains(out, `supervisor_metrics_scrape_errors_total{query="spawns"} 1`) {
		t.Errorf("expected scrape_errors{query=spawns}=1:\n%s", out)
	}
}
```

- [ ] **Step 6: Write total-failure test**

Append to `metrics_test.go`:

```go
func TestCollect_TotalFailureReturns503(t *testing.T) {
	db, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.Close() // Close so BeginTx fails.

	c := metrics.NewCollector(db)
	h := metrics.Handler(c)
	req := httptest.NewRequest("GET", "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 503 {
		t.Errorf("status=%d, want 503", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	out := string(body)
	if strings.Contains(out, "supervisor_dlq_size") {
		t.Errorf("no business family should be present:\n%s", out)
	}
	for _, q := range []string{"spawns", "tokens", "dlq", "inbox_backlog", "spawn_duration"} {
		want := fmt.Sprintf(`supervisor_metrics_scrape_errors_total{query="%s"}`, q)
		if !strings.Contains(out, want) {
			t.Errorf("missing %s:\n%s", want, out)
		}
	}
}
```

- [ ] **Step 7: Run tests**

Run: `go test -race -run "TestCollect_PartialFailure|TestCollect_TotalFailure" ./internal/metrics/ -v`

Expected: both PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/metrics/metrics.go internal/metrics/metrics_test.go internal/metrics/export_test.go
git commit -m "feat(metrics): partial-failure (200+omit) + total-failure (503)"
```

---

## Task 11: Panic recovery in `Collect()`

**Files:**
- Modify: `internal/metrics/export_test.go` (add panic injection)
- Modify: `internal/metrics/metrics.go` (add `panicNext` field + defer recover in `Collect`)
- Modify: `internal/metrics/metrics_test.go` (panic test)

- [ ] **Step 1: Add panic injection to `export_test.go`**

Append:

```go
// PanicOnCollectForTest causes the next Collect() call to panic. Test-only.
func (c *Collector) PanicOnCollectForTest() {
	c.panicNext = true
}
```

- [ ] **Step 2: Update `Collector` struct and `Collect()`**

In `internal/metrics/metrics.go`, update the struct:

```go
type Collector struct {
	db           *sqlite.DB
	descs        collectorDescs
	scrapeErrors *prometheus.CounterVec
	failQuery    string
	panicNext    bool // test-only
}
```

Prepend to `Collect()`'s body (before the `ctx, cancel :=` line):

```go
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	defer func() {
		if r := recover(); r != nil {
			c.scrapeErrors.WithLabelValues("panic").Inc()
			c.scrapeErrors.Collect(ch)
		}
	}()
	if c.panicNext {
		c.panicNext = false
		panic("injected test panic")
	}
	// ... rest of Collect unchanged
```

- [ ] **Step 3: Write the failing test**

Append to `metrics_test.go`:

```go
func TestCollect_PanicRecovery(t *testing.T) {
	db, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	c := metrics.NewCollector(db)
	c.PanicOnCollectForTest()
	h := metrics.Handler(c)
	req := httptest.NewRequest("GET", "/metrics", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req) // must not crash

	if rec.Code != 503 {
		t.Errorf("status=%d, want 503 (panic before any family emitted)", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	out := string(body)
	if !strings.Contains(out, `supervisor_metrics_scrape_errors_total{query="panic"} 1`) {
		t.Errorf("expected scrape_errors{query=panic}=1:\n%s", out)
	}
}
```

- [ ] **Step 4: Run test**

Run: `go test -race -run TestCollect_PanicRecovery ./internal/metrics/ -v`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/metrics/metrics.go internal/metrics/metrics_test.go internal/metrics/export_test.go
git commit -m "feat(metrics): panic recovery in Collect increments scrape_errors{query=panic}"
```

---

## Task 12: Fault-injection tests (GC during scrape, concurrent writes + scrape)

**Files:**
- Create: `internal/metrics/metrics_fault_test.go`

- [ ] **Step 1: Create the fault-test file**

Create `internal/metrics/metrics_fault_test.go`:

```go
package metrics_test

import (
	"context"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caesarioshiddieq/claude-code-lark-channel/internal/metrics"
	"github.com/caesarioshiddieq/claude-code-lark-channel/internal/sqlite"
)

// TestFault_GCDuringScrape asserts the BEGIN DEFERRED transaction gives a
// consistent snapshot even when DeleteOldTurnUsage runs concurrently.
func TestFault_GCDuringScrape(t *testing.T) {
	db, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	now := time.Now().UnixMilli()
	for i := 0; i < 10; i++ {
		_, err := db.RawDB().ExecContext(ctx, `INSERT INTO turn_usage
			(comment_id, task_id, session_uuid, phase, input_tokens, output_tokens,
			 cache_read_tokens, cache_creation_tokens, is_rate_limit_error, created_at)
			VALUES (?, 'T1', 'S1', 'normal', 1, 1, 0, 0, 0, ?)`,
			fmt.Sprintf("C%d", i), now-2*24*60*60*1000)
		if err != nil {
			t.Fatal(err)
		}
	}

	c := metrics.NewCollector(db)
	h := metrics.Handler(c)

	var wg sync.WaitGroup
	var scrapeBody string
	var scrapeStatus int
	wg.Add(2)
	go func() {
		defer wg.Done()
		req := httptest.NewRequest("GET", "/metrics", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		scrapeStatus = rec.Code
		b, _ := io.ReadAll(rec.Body)
		scrapeBody = string(b)
	}()
	go func() {
		defer wg.Done()
		// Cut off 1 day ago → deletes all 10 seeded rows.
		if _, err := db.DeleteOldTurnUsage(ctx, now-24*60*60*1000); err != nil {
			t.Errorf("DeleteOldTurnUsage: %v", err)
		}
	}()
	wg.Wait()

	if scrapeStatus != 200 {
		t.Errorf("status=%d, want 200", scrapeStatus)
	}
	// Consistent: either 0 (post-GC, emitted or absent) or 10 (pre-GC).
	ok10 := strings.Contains(scrapeBody, `supervisor_spawns_last_30d{phase="normal"} 10`)
	ok0 := strings.Contains(scrapeBody, `supervisor_spawns_last_30d{phase="normal"} 0`)
	absent := !strings.Contains(scrapeBody, `supervisor_spawns_last_30d{phase="normal"}`)
	if !(ok10 || ok0 || absent) {
		t.Errorf("spawns count neither 0/10/absent — mid-GC snapshot leaked:\n%s", scrapeBody)
	}
}

// TestFault_ConcurrentWritesAndScrape asserts scrape completes within 2s even
// with concurrent writers hammering the DB.
func TestFault_ConcurrentWritesAndScrape(t *testing.T) {
	db, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			n := 0
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = db.InsertTurnUsage(ctx, sqlite.TurnUsage{
					CommentID:    fmt.Sprintf("C%d-%d", id, n),
					TaskID:       "T1",
					SessionUUID:  "S1",
					Phase:        "normal",
					InputTokens:  1,
					OutputTokens: 1,
				})
				n++
			}
		}(i)
	}

	c := metrics.NewCollector(db)
	h := metrics.Handler(c)
	req := httptest.NewRequest("GET", "/metrics", nil)
	rec := httptest.NewRecorder()

	start := time.Now()
	h.ServeHTTP(rec, req)
	elapsed := time.Since(start)
	close(stop)
	wg.Wait()

	if elapsed > 2*time.Second {
		t.Errorf("scrape took %s, want < 2s under concurrent write load", elapsed)
	}
	if rec.Code != 200 {
		t.Errorf("status=%d, want 200", rec.Code)
	}
}
```

- [ ] **Step 2: Run fault tests**

Run: `go test -race -run TestFault ./internal/metrics/ -v`

Expected: both PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/metrics/metrics_fault_test.go
git commit -m "test(metrics): fault-injection — GC race + concurrent-write scrape"
```

---

## Task 13: Supervisor wiring — `METRICS_ADDR` env + HTTP server goroutine

**Files:**
- Modify: `cmd/supervisor/main.go`

- [ ] **Step 1: Add imports**

In `cmd/supervisor/main.go`, add to the import block:

```go
import (
	// ... existing imports ...
	"errors"
	"net/http"

	"github.com/caesarioshiddieq/claude-code-lark-channel/internal/metrics"
)
```

- [ ] **Step 2: Add the metrics server goroutine**

In `main()`, after the DB is opened and before the worker pool starts (e.g. right before `worker.NewPool(...)`), insert:

```go
	// Metrics server (optional). Default 127.0.0.1:9090; empty string disables.
	metricsAddr := "127.0.0.1:9090"
	if v, ok := os.LookupEnv("METRICS_ADDR"); ok {
		metricsAddr = v // may be empty → explicit disable
	}
	if metricsAddr != "" {
		collector := metrics.NewCollector(db)
		mux := http.NewServeMux()
		mux.Handle("/metrics", metrics.Handler(collector))
		srv := &http.Server{
			Addr:              metricsAddr,
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
			WriteTimeout:      10 * time.Second,
		}
		go func() {
			<-ctx.Done()
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer shutdownCancel()
			_ = srv.Shutdown(shutdownCtx)
		}()
		go func() {
			log.Printf("[metrics] listening on %s", metricsAddr)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("[metrics] server exited: %v", err)
			}
		}()
	} else {
		log.Printf("[metrics] METRICS_ADDR empty — server disabled")
	}
```

- [ ] **Step 3: Build**

Run: `go build ./...`

Expected: clean build.

- [ ] **Step 4: Run full test suite**

Run: `go test -race ./...`

Expected: all tests pass.

- [ ] **Step 5: Smoke-test locally**

In one terminal:
```bash
rm -f /tmp/metrics-smoke.db
DB_PATH=/tmp/metrics-smoke.db \
LARK_APP_ID=x LARK_APP_SECRET=y LARK_TASKLIST_ID=z \
METRICS_ADDR=127.0.0.1:9091 \
go run ./cmd/supervisor/ 2>&1 | head -20
```

In another terminal:
```bash
curl -s http://127.0.0.1:9091/metrics | head -30
```

Expected: output includes `# TYPE supervisor_*` lines and zeroed gauges. `supervisor_dlq_size 0`, `supervisor_inbox_backlog 0`, four `tokens_last_30d{kind=...} 0`.

Kill the supervisor (Ctrl-C or `pkill -f supervisor`) and clean up: `rm /tmp/metrics-smoke.db`.

- [ ] **Step 6: Commit**

```bash
git add cmd/supervisor/main.go
git commit -m "feat(supervisor): serve /metrics endpoint (METRICS_ADDR, default 127.0.0.1:9090)"
```

---

## Task 14: Document `METRICS_ADDR` in `.claude/CLAUDE.md`

**Files:**
- Modify: `.claude/CLAUDE.md`

- [ ] **Step 1: Append to the env vars list**

Find `## Key env vars (Go supervisor)` section. Append a new line after the last existing env var entry:

```
METRICS_ADDR — Prometheus /metrics listen address (default: 127.0.0.1:9090; empty string disables)
```

- [ ] **Step 2: Commit**

```bash
git add .claude/CLAUDE.md
git commit -m "docs(claude): document METRICS_ADDR env var"
```

---

## Task 15: Create Ops Agent Prometheus receiver fragment

**Files:**
- Create: `infra/ops-agent-prometheus-receiver.yaml`

- [ ] **Step 1: Create the fragment**

```yaml
# Fragment — MERGE into /etc/google-cloud-ops-agent/config.yaml on claude-code-vm.
# Do NOT overwrite the existing config; merge the receivers + pipelines block.
# After merging: sudo systemctl restart google-cloud-ops-agent
# Verify:        sudo journalctl -u google-cloud-ops-agent -n 30

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

- [ ] **Step 2: Commit**

```bash
git add infra/ops-agent-prometheus-receiver.yaml
git commit -m "infra: Ops Agent Prometheus receiver fragment for supervisor metrics"
```

---

## Task 16: Create Cloud Monitoring dashboard JSON

**Files:**
- Create: `infra/cloud-monitoring-dashboard.json`

- [ ] **Step 1: Create the dashboard file**

```json
{
  "displayName": "Claude VM Supervisor",
  "mosaicLayout": {
    "columns": 12,
    "tiles": [
      {
        "width": 6, "height": 4, "xPos": 0, "yPos": 0,
        "widget": {
          "title": "Spawns (last 30d) by phase",
          "xyChart": {
            "dataSets": [{
              "timeSeriesQuery": {
                "timeSeriesFilter": {
                  "filter": "metric.type=\"prometheus.googleapis.com/supervisor_spawns_last_30d/gauge\"",
                  "aggregation": {
                    "alignmentPeriod": "60s",
                    "perSeriesAligner": "ALIGN_MEAN",
                    "groupByFields": ["metric.label.phase"]
                  }
                }
              }
            }]
          }
        }
      },
      {
        "width": 6, "height": 4, "xPos": 6, "yPos": 0,
        "widget": {
          "title": "Tokens (last 30d) by kind",
          "xyChart": {
            "dataSets": [{
              "timeSeriesQuery": {
                "timeSeriesFilter": {
                  "filter": "metric.type=\"prometheus.googleapis.com/supervisor_tokens_last_30d/gauge\"",
                  "aggregation": {
                    "alignmentPeriod": "60s",
                    "perSeriesAligner": "ALIGN_MEAN",
                    "groupByFields": ["metric.label.kind"]
                  }
                }
              }
            }]
          }
        }
      },
      {
        "width": 6, "height": 4, "xPos": 0, "yPos": 4,
        "widget": {
          "title": "Inbox backlog",
          "scorecard": {
            "timeSeriesQuery": {
              "timeSeriesFilter": {
                "filter": "metric.type=\"prometheus.googleapis.com/supervisor_inbox_backlog/gauge\"",
                "aggregation": {"alignmentPeriod": "60s", "perSeriesAligner": "ALIGN_MEAN"}
              }
            },
            "thresholds": [
              {"value": 10, "color": "YELLOW"},
              {"value": 50, "color": "RED"}
            ]
          }
        }
      },
      {
        "width": 6, "height": 4, "xPos": 6, "yPos": 4,
        "widget": {
          "title": "DLQ size (should be 0)",
          "scorecard": {
            "timeSeriesQuery": {
              "timeSeriesFilter": {
                "filter": "metric.type=\"prometheus.googleapis.com/supervisor_dlq_size/gauge\"",
                "aggregation": {"alignmentPeriod": "60s", "perSeriesAligner": "ALIGN_MEAN"}
              }
            },
            "thresholds": [{"value": 1, "color": "RED"}]
          }
        }
      },
      {
        "width": 12, "height": 4, "xPos": 0, "yPos": 8,
        "widget": {
          "title": "Spawn duration p95 by phase",
          "xyChart": {
            "dataSets": [{
              "timeSeriesQuery": {
                "timeSeriesFilter": {
                  "filter": "metric.type=\"prometheus.googleapis.com/supervisor_spawn_duration_seconds/histogram\"",
                  "aggregation": {
                    "alignmentPeriod": "60s",
                    "perSeriesAligner": "ALIGN_DELTA",
                    "crossSeriesReducer": "REDUCE_PERCENTILE_95",
                    "groupByFields": ["metric.label.phase"]
                  }
                }
              }
            }]
          }
        }
      },
      {
        "width": 12, "height": 4, "xPos": 0, "yPos": 12,
        "widget": {
          "title": "VM host — CPU + memory",
          "xyChart": {
            "dataSets": [
              {
                "timeSeriesQuery": {
                  "timeSeriesFilter": {
                    "filter": "metric.type=\"compute.googleapis.com/instance/cpu/utilization\" resource.label.instance_name=\"claude-code-vm\"",
                    "aggregation": {"alignmentPeriod": "60s", "perSeriesAligner": "ALIGN_MEAN"}
                  }
                }
              },
              {
                "timeSeriesQuery": {
                  "timeSeriesFilter": {
                    "filter": "metric.type=\"agent.googleapis.com/memory/percent_used\" resource.label.instance_id=\"claude-code-vm\"",
                    "aggregation": {"alignmentPeriod": "60s", "perSeriesAligner": "ALIGN_MEAN"}
                  }
                }
              }
            ]
          }
        }
      }
    ]
  }
}
```

- [ ] **Step 2: Commit**

```bash
git add infra/cloud-monitoring-dashboard.json
git commit -m "infra: Cloud Monitoring dashboard JSON (6 widgets for supervisor metrics)"
```

---

## Task 17: Document apply procedure in `infra/DEPLOY.md`

**Files:**
- Modify: `infra/DEPLOY.md`

- [ ] **Step 1: Append a new section after existing section 3**

Append:

```markdown
## 3.5. Metrics — First-Time Setup

### Apply Ops Agent fragment on the VM

```bash
# Copy the fragment
gcloud compute scp infra/ops-agent-prometheus-receiver.yaml \
  claude-code-vm:/tmp/ops-agent-prom-fragment.yaml \
  --tunnel-through-iap --project=devsecops-480902 --zone=asia-southeast2-b

# Back up existing config and merge the fragment
gcloud compute ssh claude-code-vm --tunnel-through-iap \
  --project=devsecops-480902 --zone=asia-southeast2-b -- \
  'sudo cp /etc/google-cloud-ops-agent/config.yaml /etc/google-cloud-ops-agent/config.yaml.prev && \
   sudoedit /etc/google-cloud-ops-agent/config.yaml'
# In the editor: merge the receivers + pipelines from /tmp/ops-agent-prom-fragment.yaml.

# Restart and verify
gcloud compute ssh claude-code-vm --tunnel-through-iap \
  --project=devsecops-480902 --zone=asia-southeast2-b -- \
  'sudo systemctl restart google-cloud-ops-agent && \
   sleep 3 && \
   sudo journalctl -u google-cloud-ops-agent -n 30 --no-pager'
```

### Apply Cloud Monitoring dashboard

```bash
gcloud monitoring dashboards create \
  --config-from-file=infra/cloud-monitoring-dashboard.json \
  --project=devsecops-480902
```

Verify via https://console.cloud.google.com/monitoring/dashboards (dashboard named "Claude VM Supervisor").

### Rollback

```bash
# Ops Agent config
gcloud compute ssh claude-code-vm --tunnel-through-iap \
  --project=devsecops-480902 --zone=asia-southeast2-b -- \
  'sudo cp /etc/google-cloud-ops-agent/config.yaml.prev /etc/google-cloud-ops-agent/config.yaml && \
   sudo systemctl restart google-cloud-ops-agent'

# Dashboard
gcloud monitoring dashboards list --project=devsecops-480902 \
  --format='value(name)' --filter='displayName="Claude VM Supervisor"'
# Then:
gcloud monitoring dashboards delete <dashboard-name> --project=devsecops-480902
```
```

- [ ] **Step 2: Commit**

```bash
git add infra/DEPLOY.md
git commit -m "docs(deploy): Ops Agent fragment + Cloud Monitoring dashboard apply procedures"
```

---

## Task 18: Final local verification

**Files:** none (test-only)

- [ ] **Step 1: Run full test suite with -race**

Run: `go test -race -count=1 ./...`

Expected: all tests pass (baseline was 73, expect ~84+ after this plan). No race warnings.

- [ ] **Step 2: Run go vet**

Run: `go vet ./...`

Expected: no issues.

- [ ] **Step 3: Build for deployment target**

Run: `GOOS=linux GOARCH=amd64 go build -o infra/claude-vm-supervisor ./cmd/supervisor/`

Expected: clean build. Binary ~16–17 MB.

- [ ] **Step 4: End-to-end local smoke test**

```bash
rm -f /tmp/metrics-e2e.db
(LARK_APP_ID=x LARK_APP_SECRET=y LARK_TASKLIST_ID=z \
 DB_PATH=/tmp/metrics-e2e.db METRICS_ADDR=127.0.0.1:9091 \
 ./infra/claude-vm-supervisor 2>&1 &)
sleep 2
curl -s http://127.0.0.1:9091/metrics | grep -E "^(# TYPE|supervisor_)" | head -30
pkill -f "infra/claude-vm-supervisor"
rm -f /tmp/metrics-e2e.db
```

Expected: output includes TYPE headers for all 6 families and zeroed gauges.

---

## Task 19: Open PR for review

**Files:** none (git + gh operations)

- [ ] **Step 1: Push branch**

```bash
git push -u origin feat/supervisor-metrics
```

- [ ] **Step 2: Open PR**

```bash
gh pr create --base main --head feat/supervisor-metrics \
  --title "feat: supervisor metrics endpoint (M1b) — spawns/tokens/dlq/backlog/duration via Ops Agent" \
  --body "$(cat <<'EOF'
## Summary

Implements the M1b metrics feature per spec `docs/superpowers/specs/2026-04-23-supervisor-metrics.md` (approved after 4 Codex review rounds).

- 6 SQL-derived metrics at `/metrics` (default `127.0.0.1:9090`, `METRICS_ADDR` override)
- Ops Agent (already installed) scrapes and forwards to Cloud Monitoring
- New `spawn_duration_ms` column (migration0004) for latency histogram
- `prometheus/client_golang` + `ConstHistogram` pattern — no accumulated Go state
- Partial-failure rule: per-query error → 200 + omit family; all-fail → 503
- `BEGIN DEFERRED` transaction for snapshot semantics across queries

## Test plan

- [x] `go test -race ./...` — all tests pass
- [x] `go vet ./...` — clean
- [x] 9 new unit tests: empty DB, seeded, DLQ, backlog, histogram, partial-fail (200), total-fail (503), panic
- [x] 2 fault-injection tests: GC race (snapshot-consistent), concurrent writes + scrape (<2s budget)
- [x] Local smoke test via curl
- [ ] Deploy to `claude-code-vm` with Ops Agent fragment + dashboard
- [ ] Verify metrics land in Cloud Monitoring within 24h
- [ ] Verify all 6 dashboard widgets render

## Cost

- `prometheus/client_golang`: \$0 (Apache-2.0)
- Cloud Monitoring ingestion: ~120 MB/month — fits 150 MB free tier. Worst case \$3–8/month overage.

## Rollback

- Binary: revert `.prev` on VM
- Migration: harmless (NULL spawn_duration_ms excluded from histogram)
- Ops Agent config: revert `.prev`
- Dashboard: `gcloud monitoring dashboards delete`

Spec revisions: `9bb7bc7 → 3716fff → 00a380f → e993ffe` (4 Codex passes).
EOF
)"
```

Expected: PR URL returned.

---

## Post-merge deployment (human-driven)

Not a plan task — see `infra/DEPLOY.md` §3.5. Key gates:

1. Merge PR to main
2. Build `GOOS=linux GOARCH=amd64` binary
3. SCP + restart supervisor service
4. `curl http://localhost:9090/metrics` on VM
5. Apply Ops Agent fragment + restart agent
6. Apply Cloud Monitoring dashboard
7. Observe 24h → consider bumping `MAX_CONCURRENT_SPAWNS_GLOBAL=2`

---

## Self-review checklist (completed)

- [x] Every task has exact file paths
- [x] Every step has real code or an exact command + expected output
- [x] No "TBD", "TODO", "implement later" placeholders
- [x] Type names consistent across tasks (`SpawnDurationMs`, `Collector`, `scrapeErrors`, query labels `spawns/tokens/dlq/inbox_backlog/spawn_duration`)
- [x] TDD sequencing: test-first for each new behavior
- [x] Spec coverage: migration0004, 6 metrics, partial-failure rule, panic recovery, BEGIN DEFERRED snapshot, METRICS_ADDR env, Ops Agent fragment, dashboard, DEPLOY docs, CLAUDE.md update — all mapped to tasks
- [x] Frequent commits (one per task, often one per step where natural)
