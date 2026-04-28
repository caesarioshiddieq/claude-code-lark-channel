---
type: plan
date: 2026-04-27
status: draft
project: claude-code-vm
design: ../../obsidian-doc-claude/designs/claude-vm-token-budget-design.md
decision: ../../obsidian-doc-claude/decisions/2026-04-27-claude-code-vm-autonomous-implementer-loop-choice.md
tags: [claude-code-vm, autonomous-implementer, gnhf, go, sqlite, lark, implementation]
---

# Nightly Autonomous Implementer — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` (recommended) or `superpowers:executing-plans` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an autonomous implementer subsystem to the `claude-code-vm` supervisor that picks up `phase=implement` rows during the 19:00–05:00 Asia/Jakarta autonomous window, spawns `gnhf` against an isolated git worktree per Lark task, captures gnhf's per-iteration JSONL log + final orchestrator state, and posts the outcome (Status × Reason × NoProgress) to the Lark task thread via the existing outbox.

**Architecture:** The supervisor's worker dispatch fork grows a third branch alongside `dispatchAnswer` and `dispatchCompact`: **`dispatchImplement`**. For `phase=implement` rows, the worker (i) computes the per-window token allowance via the existing `budget.CanSpawn()` math, (ii) materializes a per-task git worktree under `/var/lib/claude-vm/worktrees/<task_id>/` via the `worktree.Manager` (Task 3), (iii) invokes `gnhf --agent claude --max-tokens <budget> --max-iterations 30 --stop-when "<NL completion phrase>"` as a subprocess with `cmd.Dir = <worktree path>` and prompt via stdin (NO `--worktree` flag — that's a boolean and conflicts with our pre-created worktree), (iv) waits for graceful shutdown (SIGTERM→grace→SIGKILL), discovers the run dir via name-set difference on `<wt>/.gnhf/runs/`, parses `<runDir>/gnhf.log` JSONL (last `event:"run:complete"` line) for the orchestrator's final state and reads `<runDir>/notes.md` for human-readable summary, (v) writes the gnhf-native fields (`gnhf_status`, `gnhf_reason`, `gnhf_no_progress`, `gnhf_run_id`, ...) plus a derived legacy `outcome` to the `implementer_runs` table and to the existing `outbox`. PR opening uses `gh pr create` against the gnhf-produced branch. All changes are additive — `dispatchAnswer` (existing `claude -p` path) and `dispatchCompact` are untouched.

**Tech Stack:** Go 1.26 (matches existing supervisor), `modernc.org/sqlite`, `os/exec`, `gnhf` v0.1.26+ (npm), `gh` CLI for PR opening, `git worktree` (native).

**Module:** `github.com/caesarioshiddieq/claude-code-lark-channel`

**Sub-skill required (per existing plans):** `superpowers:subagent-driven-development`.

---

## Handoff context (continues from)

- **Latest shipped:** `2026-04-23-200121-worker-pool-shipped.md` — PR3 `feat/worker-pool` deployed; `MAX_CONCURRENT_SPAWNS_GLOBAL` in place, default 1.
- **Currently in-flight:** `2026-04-23-supervisor-metrics-plan.md` (M1b Prometheus `/metrics`, 19 tasks). **This plan is sequenced AFTER M1b** so observability is in place before bumping autonomous workload.
- **Earlier "next" pointer:** `2026-04-21-201303-autonomous-implementer-next.md` (this plan supersedes that note's intent).

**Sequencing:** M1b metrics → N=2 stability window → **this plan** → eventually M2 multi-task config.

---

## Locked decisions (carried from related design + decision docs)

| # | Decision | Source |
|---|---|---|
| L1 | Loop body = gnhf (NOT ralph-loop, NOT agent-orchestrator) | `decisions/2026-04-27-claude-code-vm-autonomous-implementer-loop-choice.md` |
| L2 | Autonomous spawns gated to 19:00–05:00 Asia/Jakarta via `budget.CanSpawn()` (already shipped) | `designs/claude-vm-token-budget-design.md` D2 |
| L3 | Subscription safety, not $/day; `--max-tokens` enforced by supervisor pre-spawn | `designs/claude-vm-token-budget-design.md` D1 |
| L4 | Phase column on `inbox` is single source of truth (no parallel session flags) | `designs/claude-vm-token-budget-design.md` R2-B2 |
| L5 | Outbox key is `(comment_id, phase)` to support multiple outbox rows per task thread | `designs/claude-vm-token-budget-design.md` R2-B1 |
| L6 | Dispatch priority `answer > compact > implement > normal`; implementer is lower priority than user-facing answer/compact phases | This plan (extends R2-M5) |
| L7 | One worktree per `task_id` under `/var/lib/claude-vm/worktrees/<task_id>/`; preserved on success, GC'd on failure | This plan |
| L8 | gnhf default `--agent claude` (Claude Max OAuth, single shared subscription); multi-agent fallback deferred to M3 | This plan |
| L9 | Implementer-produced PRs use `gh pr create` against gnhf-produced branch; reviewer-routing reactor (CI failures auto-back-to-agent) is **out of scope for this plan** (M3) | This plan |

---

## File map

| File | Action | Responsibility |
|---|---|---|
| `internal/sqlite/queue.go` | Modify | `migration0004` (Task 1, **shipped**): create `implementer_runs` v1 + extend `inbox.phase` for `implement`. `migration0005` (Task 4 prep, Step 4.0): extend `implementer_runs` with gnhf-native columns (`gnhf_status`, `gnhf_reason`, `gnhf_input_tokens`, `gnhf_output_tokens`, `gnhf_success_count`, `gnhf_fail_count`, `gnhf_run_id`, `gnhf_no_progress`, `gnhf_last_message`). The original `outcome` and `tokens_used` columns are retained as derived/legacy. |
| `internal/sqlite/queue_test.go` | Modify | Tests for `migration0005` (idempotency, default backfill, no regression of Task 1 round-trip CRUD) |
| `internal/intent/classifier.go` | Create | `Classify(comment string) Phase` — heuristic mapper from Lark comment text to `Phase{normal, implement}`. MVP: keyword + leading verb match. Hard-mode classifier defers to a future PRD. |
| `internal/intent/classifier_test.go` | Create | Table-driven tests for keyword classifier |
| `internal/worktree/manager.go` | Create | `EnsureForTask(taskID, repoPath) (string, error)`, `Cleanup(taskID, success bool) error`, `BaseDir()` helpers |
| `internal/worktree/manager_test.go` | Create | Tests against a real ephemeral git repo (in-test `git init` + `git worktree add`) |
| `internal/implementer/spawn.go` | Create | `SpawnGnhf(ctx, args GnhfArgs) (GnhfResult, error)` — wraps `os/exec.Command("gnhf", ...)` with cwd=worktree path (no `--worktree` flag), stdin-piped prompt, snapshot-based runId discovery, SIGTERM→grace→SIGKILL graceful cancel, and post-run JSONL parse via `parse.go`. |
| `internal/implementer/parse.go` | Create | Pure function `ParseGnhfLog(jsonl []byte) (GnhfResult, error)` — extracts the last `event:"run:complete"` line, derives `Reason` from `lastMessage` heuristics, derives `NoProgress`. Mirrors the parse/IO split from `internal/worker/spawn.go`'s `ParseClaudeOutput`. |
| `internal/implementer/spawn_test.go` | Create | Integration tests against a mock `gnhf` shell script (mirrors `internal/worker/spawn_test.go`) — covers happy path, ctx cancel + graceful SIGTERM, runId discovery fallback, preflight failures, defaults applied. |
| `internal/implementer/parse_test.go` | Create | Pure-function table-driven tests for all `(Status, Reason)` mappings + malformed/missing JSONL + lookalike-event filtering. |
| `internal/implementer/dispatch.go` | Create | `DispatchImplement(ctx, row InboxRow, deps Deps) error` — top-level handler called by the dispatcher. Computes token allowance, ensures worktree, invokes spawn, writes `implementer_runs`, queues outbox row. |
| `internal/implementer/dispatch_test.go` | Create | Integration-style tests with fake `Deps` (fake budget gate, fake spawn, in-memory SQLite) |
| `internal/budget/gate.go` | Modify | Add `TokenAllowance(ctx, now time.Time) (int64, error)` returning the per-window remaining allowance. Existing `CanSpawn` becomes thin wrapper over `TokenAllowance > 0` to avoid duplicate logic. |
| `internal/budget/gate_test.go` | Modify | Tests for `TokenAllowance` boundary cases (window start, near-end, fresh window) |
| `internal/lark/client.go` | Modify | Add `PostImplementerSummary(taskID string, summary ImplementerSummary) error` — formats gnhf result for Lark thread (compact, links to PR if opened). |
| `internal/lark/client_test.go` | Modify | Tests for the formatter |
| `cmd/supervisor/main.go` | Modify | Wire `dispatchImplement` into the dispatcher fork; thread the new `Phase{implement}` priority into `NextInboxRow`; add env vars (`GNHF_BIN`, `IMPLEMENTER_WORKTREE_BASE`, `IMPLEMENTER_DEFAULT_REPO`) |
| `infra/env.example` | Modify | Document new env vars |
| `infra/claude-vm.service` | Modify | Add `ReadWritePaths=/var/lib/claude-vm/worktrees` so systemd allows worktree creation |
| `infra/setup-gnhf.sh` | Create | Idempotent install: `npm install -g gnhf@<pinned>`, verify `gnhf --version`, optional `--dry-run` |
| `.claude/CLAUDE.md` | Modify | Document `gnhf` dependency, the 4 new env vars, and the worktree base path |

---

## Tasks

### Task 0: Pre-flight — confirm sequencing dependencies

- [ ] Verify M1b metrics plan (`2026-04-23-supervisor-metrics-plan.md`) is **shipped** before starting implementation. Autonomous implementer needs `spawn_total{phase=implement}`, `spawn_duration_seconds{phase=implement}`, and a new `implementer_runs_total{outcome=success|blocked|failed}` for safe rollout.
- [ ] Confirm N=2 stability window has elapsed without incident (per worker-pool handoff: ~1 day uneventful at N=1 first, then bump). If still at N=1, pause this plan until that gate clears.
- [ ] Verify `gnhf` installs cleanly on the VM (`asia-southeast2-b` Spot e2-medium): `npm install -g gnhf@v0.1.26` followed by `gnhf --version`. If install fails, debug Node version (gnhf needs Node 20+) before proceeding.
- [ ] Verify `gh` CLI is installed and authenticated on the VM with a token that has PR-open permissions on the relevant repos.

### Task 1: Schema migration — `phase=implement` + `implementer_runs` table — **SHIPPED as `migration0004`**

> **Migration numbering correction (Round-4 review):** This task originally said `migration0005`, but the live schema registry only had migrations 0002+0003. What actually shipped (handoff `2026-04-27-tasks-1-3-and-p1-fixes-shipped.md`, commit `7960ea0`) is **`migration0004`**. The schema-extension migration in **Step 4.0** is therefore `migration0005` (the next free number). The original "outcome enum" listed below (`success|blocked|failed|timeout`) is what the v1 table shipped with; Step 4.0 retains it as a derived legacy column and adds the gnhf-native columns alongside.

- [x] **Step 1.1:** Add `migration0004` to `internal/sqlite/queue.go`. The migration: (a) widens any application-layer enum check on `inbox.phase` to accept `implement`; (b) creates `implementer_runs` table. Schema (as shipped):
  - `id INTEGER PRIMARY KEY`
  - `inbox_comment_id TEXT NOT NULL`
  - `task_id TEXT NOT NULL`
  - `started_at INTEGER NOT NULL`
  - `finished_at INTEGER`
  - `outcome TEXT` (legacy enum — Step 4.0 adds `gnhf_status` + `gnhf_reason` as the source-of-truth alongside; `outcome` becomes a derived column)
  - `gnhf_iterations INTEGER` (legacy single counter — Step 4.0 adds `gnhf_input_tokens` + `gnhf_output_tokens` for the actual gnhf split)
  - `gnhf_commits_made INTEGER`
  - `tokens_used INTEGER` (derived from gnhf_input_tokens + gnhf_output_tokens after Step 4.0)
  - `worktree_path TEXT`
  - `branch_name TEXT`
  - `pr_url TEXT`
  - `notes_md_excerpt TEXT` (truncated)
  - `error TEXT`
- [x] **Step 1.2:** CRUD shipped: `InsertImplementerRun`, `UpdateImplementerRunOutcome`, `GetImplementerRunByCommentID`. Note Step 4.0 will extend `UpdateImplementerRunOutcome` to take the new gnhf-native fields.
- [x] **Step 1.3:** Tests shipped: `TestMigration0004`, `TestImplementerRunRoundTrip`, `TestImplementerRunOutcomeUpdate`. Pass `-race`.

### Task 2: Intent classifier — `phase=normal` vs `phase=implement`

- [ ] **Step 2.1:** Create `internal/intent/classifier.go`. MVP heuristic: comment text starts with one of `["implement", "build", "ship", "kerjain", "buatin"]` (case-insensitive, EN/ID bilingual per existing project preference) → `phase=implement`. Otherwise `phase=normal`.
- [ ] **Step 2.2:** Wire classifier into the poller (`cmd/supervisor/main.go` `processNewComment` path). Set `inbox.phase` at insert time based on classifier output. Default fallback `normal` (today's behavior preserved).
- [ ] **Step 2.3:** Table-driven tests covering all 5 verbs in both languages, plus negatives ("can you implement…" should still be `normal` because of the leading "can"). Document the heuristic's known limits in `internal/intent/README.md` (false negatives are *better* than false positives — a missed implement intent gets a `claude -p` answer the user can re-trigger; a false-positive implement intent burns budget).
- [ ] **Step 2.4:** Add log line at WARN level when classifier picks `implement`, so cold-start observers see autonomous spawns in the journal.

### Task 3: Worktree manager

- [ ] **Step 3.1:** Create `internal/worktree/manager.go`. Functions:
  - `BaseDir() string` — returns `IMPLEMENTER_WORKTREE_BASE` env (default `/var/lib/claude-vm/worktrees`)
  - `EnsureForTask(ctx, taskID, repoPath) (worktreePath string, branchName string, err error)` — generates `<base>/<task_id>/`, runs `git worktree add -b implement/<task_id>-<short_uuid>` (idempotent: returns existing path if dir already exists and is healthy), returns the worktree path
  - `Cleanup(ctx, taskID string, success bool) error` — on `success=true`, leave worktree (commits preserved per gnhf semantics + we may need to inspect later); on `success=false`, `git worktree remove --force` + `rm -rf`
  - `GarbageCollect(ctx, olderThan time.Duration) error` — periodic GC for stale worktrees
- [ ] **Step 3.2:** Tests against an ephemeral repo created in `t.TempDir()`. Cover: idempotent ensure, cleanup on failure, GC of stale dirs.
- [ ] **Step 3.3:** Update `infra/claude-vm.service` to add `/var/lib/claude-vm/worktrees` to `ReadWritePaths`. Verify systemd reload picks it up before subsystem ships.

### Task 4: gnhf spawn wrapper

> **Revised 2026-04-27 after empirical gnhf v0.1.26 review** (Codex second-opinion). The original sketch assumed `--worktree <path>` (boolean in reality), assumed gnhf emits a final JSON line on stdout (it's a TUI — no machine-readable stdout), assumed an `Outcome ∈ {success,blocked,failed,timeout}` enum (gnhf's real `status` enum is `{running,waiting,stopped,aborted}`), and assumed `notes.md` exists at the worktree root (it lives at `.gnhf/runs/<runId>/notes.md`). Corrected contract below; superseded sub-bullets reproduced strikethrough at the end of this task for traceability.

**Authoritative outcome source** (from `gnhf/dist/cli.mjs:4404`): `<cwd>/.gnhf/runs/<runId>/gnhf.log` (JSONL). Final event:
```json
{"event":"run:complete","status":"stopped|aborted","iterations":N,
 "successCount":N,"failCount":N,"totalInputTokens":N,"totalOutputTokens":N,
 "commitCount":N,"worktreePath":"..."}
```
Reason is derived from `lastMessage` heuristics (`"max iterations reached"`, `"max tokens reached"`, `"max consecutive failures"`, or stop-when match).

- [ ] **Step 4.0:** **Schema extension — `migration0005`.** Task 1's shipped `implementer_runs` table (migration0004) used the now-fictional `outcome ∈ {success,blocked,failed,timeout}` enum and a single `tokens_used` column. We need gnhf's real fields stored faithfully (otherwise analytics queries cannot distinguish stop-when success from no-progress, or input vs output tokens). Add migration0005 in `internal/sqlite/queue.go`:
  ```sql
  ALTER TABLE implementer_runs ADD COLUMN gnhf_status        TEXT    NOT NULL DEFAULT '';
  ALTER TABLE implementer_runs ADD COLUMN gnhf_reason        TEXT    NOT NULL DEFAULT '';
  ALTER TABLE implementer_runs ADD COLUMN gnhf_success_count INTEGER NOT NULL DEFAULT 0;
  ALTER TABLE implementer_runs ADD COLUMN gnhf_fail_count    INTEGER NOT NULL DEFAULT 0;
  ALTER TABLE implementer_runs ADD COLUMN gnhf_input_tokens  INTEGER NOT NULL DEFAULT 0;
  ALTER TABLE implementer_runs ADD COLUMN gnhf_output_tokens INTEGER NOT NULL DEFAULT 0;
  ALTER TABLE implementer_runs ADD COLUMN gnhf_run_id        TEXT    NOT NULL DEFAULT '';
  ALTER TABLE implementer_runs ADD COLUMN gnhf_no_progress   INTEGER NOT NULL DEFAULT 0;
  ALTER TABLE implementer_runs ADD COLUMN gnhf_last_message  TEXT    NOT NULL DEFAULT '';
  ```
  Keep the original `outcome` and `tokens_used` columns as **derived legacy** for backwards compat with any future dashboard/export consumer — DO NOT use them as control-plane truth (the supervisor reads `gnhf_status` / `gnhf_reason` / `gnhf_no_progress` directly). Mapping (applied at write time in Task 5; primary key is `(status, reason)`, `no_progress` is a refinement only inside `stopped`):
  - `outcome = "success"` when `gnhf_status='stopped' AND gnhf_reason='stop_when' AND gnhf_no_progress=0`
  - `outcome = "blocked"` when `gnhf_status='stopped' AND gnhf_reason='stop_when' AND gnhf_no_progress=1` (stop-when matched but no commits — agent gave up cleanly. Round-3 review tightened this; previously `stopped+no_progress` would swallow `stopped+unknown+no_progress`, contradicting the "(status, reason) primary key" rule.)
  - `outcome = "stopped"` when `gnhf_status='stopped' AND gnhf_reason='unknown'` (regardless of `no_progress` — orchestrator returned without explicit reason; "stopped" without a clean cause)
  - `outcome = "timeout"` when `gnhf_status='aborted' AND gnhf_reason IN ('max_iterations','max_tokens')`
  - `outcome = "failed"` when `gnhf_status='aborted' AND gnhf_reason IN ('max_failures','signal','unknown')`
  - `tokens_used = gnhf_input_tokens + gnhf_output_tokens`
  Critical: `no_progress` does NOT override aborted outcomes — an aborted-max-tokens run with no commits stays `outcome="timeout"`, not `"blocked"`. (Round-2 review caught the previous override pattern as hiding tripwire signal.)
  Tests: `TestMigration0005_AddsColumnsIdempotent`, `TestMigration0005_BackfillsZeros` (existing rows must end up with sensible defaults — they don't have gnhf data, but defaults must not break the round-trip CRUD added in Task 1). Re-run Task 1's `TestImplementerRunRoundTrip` to confirm no regression.

- [ ] **Step 4.1:** Create `internal/implementer/spawn.go` + `internal/implementer/parse.go` (split parse from I/O, mirrors `internal/worker/spawn.go`'s `ParseClaudeOutput` pattern). Types:
  ```go
  type Status string
  const (StatusStopped Status = "stopped"; StatusAborted Status = "aborted")

  type Reason string
  const (
      ReasonStopWhen      Reason = "stop_when"
      ReasonMaxIterations Reason = "max_iterations"
      ReasonMaxTokens     Reason = "max_tokens"
      ReasonMaxFailures   Reason = "max_failures"
      ReasonSignal        Reason = "signal"
      ReasonUnknown       Reason = "unknown"
  )

  type GnhfArgs struct {
      Prompt        string         // delivered via stdin
      WorktreePath  string         // cmd.Dir; cwd for the gnhf process
      ExpectedBranch string         // preflight check; e.g. "implement/<task_id>"
      MaxTokens     int64
      MaxIterations int            // default 30
      StopWhen      string         // default "all tests pass and the implementation matches the request"
      Agent         string         // default "claude"
      Timeout       time.Duration  // default 4h
      GracePeriod   time.Duration  // default 30s — SIGTERM→grace→SIGKILL window
  }

  type GnhfResult struct {
      Status        Status
      Reason        Reason
      Iterations    int
      SuccessCount  int
      FailCount     int
      CommitCount   int
      InputTokens   int
      OutputTokens  int
      WorktreePath  string  // gnhf-reported (may be empty when --worktree absent)
      RunID         string  // .gnhf/runs/<runID>/ directory name
      NotesExcerpt  string  // first ~512 bytes of <runDir>/notes.md, if present
      LastMessage   string  // free-text from orchestrator (used to derive Reason)
      NoProgress    bool    // derived: CommitCount == 0 (operator-visible artifact only; SuccessCount is agent-self-reported, can be lost to git reset, and is therefore unreliable)
      LogIncomplete bool    // set when run:complete event was never written (synthesized result; see Step 4.2)
  }
  ```
- [ ] **Step 4.2:** `ParseGnhfLog(jsonl []byte) (GnhfResult, error)` — pure function. Iterates lines, finds the last `event:"run:complete"` line, populates `GnhfResult`, derives `Reason` from `lastMessage` substrings, derives `NoProgress = (CommitCount == 0)`. No I/O; trivially unit-testable with synthetic JSONL fixtures.
  **Crash-resilience contract:** if no `run:complete` line is found (gnhf or its agent crashed mid-flush), do NOT hard-error. Return `GnhfResult{Status: StatusAborted, Reason: ReasonUnknown, LastMessage: "missing run:complete event", LogIncomplete: true}` along with a typed `ErrIncompleteLog`. Caller (Task 5 dispatcher) receives a complete-enough struct to persist `implementer_runs` and decide retry policy without leaving a NULL row. Same contract on malformed JSONL beyond the last parseable record.
- [ ] **Step 4.3:** `SpawnGnhf(ctx, args) (GnhfResult, error)` flow:
  1. **Preflight** (`args.WorktreePath` must exist + be a git worktree, HEAD non-detached, branch matches `args.ExpectedBranch` if set). Return error before spawning if any check fails.
  2. Compute the exclude path via `git -C <args.WorktreePath> rev-parse --path-format=absolute --git-path info/exclude`. **Note:** `info/exclude` is part of the *common* git dir, not per-worktree, so on a linked worktree this resolves to `<main_repo>/.git/info/exclude` (NOT `<main>/.git/worktrees/<name>/info/exclude`). The `--path-format=absolute` flag is required to avoid relative-path ambiguity on different git versions. Append `.gnhf/` to that resolved path (idempotent — skip if already present) so gnhf runtime artifacts never get committed. (Round-3 review caught the previous `<wt>/.git/info/exclude` assumption as wrong for linked worktrees AND a mistaken test expectation in the previous revision.)
  3. **Snapshot** `<WorktreePath>/.gnhf/runs/` directory entries — record the **set of names** present pre-spawn (this is the authoritative discriminator for runId discovery; mtime is not reliable because pre-existing dirs can be touched by unrelated activity post-spawn). Also record `spawnStart := time.Now()` for diagnostic logging only — NOT for filtering.
  4. Construct `exec.Command("gnhf", "--agent", args.Agent, "--max-iterations", ..., "--max-tokens", ..., "--stop-when", args.StopWhen)` (no `--worktree` flag). Set `cmd.Dir = args.WorktreePath`, `cmd.Stdin = strings.NewReader(args.Prompt)`, `cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}` (so we can signal the whole process group).
  5. Start. Poll on `ctx.Done()` and `args.Timeout`. **On cancellation:** send `SIGTERM` to `-cmd.Process.Pid` (the process group), wait `args.GracePeriod` for the orchestrator to flush `run:complete`, then `SIGKILL` if still running. The default `exec.CommandContext` hard-kill loses the final log line — explicit graceful shutdown is required for reliable outcome reporting.
  6. After `Wait()`: re-snapshot `.gnhf/runs/`. Compute `newDirs := postSnapshotNames \ preSnapshotNames` (set difference on **directory names**, not mtime). This is robust against pre-existing dirs whose mtime gets bumped by unrelated activity. Cases:
     - `len(newDirs) == 1` → use it.
     - `len(newDirs) > 1` → look for exactly one whose `gnhf.log` ends with a parseable `run:complete` and use that. If still ambiguous (zero or >1 with parseable run:complete), return `ErrAmbiguousRunDir{Candidates: [...]}` AND synthesize a result with `LogIncomplete=true` so the dispatcher can persist + escalate to operator triage. **Do NOT mtime-guess** — wrong attribution is worse than explicit failure (Round-4 review). In practice this case shouldn't occur (preflight + per-task lock prevent concurrent gnhf in the same cwd), but defense-in-depth matters because consequences of misattribution are high (wrong notes.md / wrong commit count to operator).
     - `len(newDirs) == 0` → return `ErrRunDirNotFound` AND synthesize `GnhfResult{Status: StatusAborted, Reason: ReasonUnknown, LogIncomplete: true}` so dispatcher can still persist a final row. (Round-3 review pinned this: name-set difference is the primary discriminator; mtime is diagnostic-only.)
  7. Read `<runDir>/gnhf.log`, pass to `ParseGnhfLog`. Read `<runDir>/notes.md` if present; populate `NotesExcerpt`. Return `GnhfResult`.
- [ ] **Step 4.4:** Tests:
  - **`ParseGnhfLog` (table-driven, no I/O):** stopped + stop-when match → `(Stopped, StopWhen)`; stopped + no stop-when → `(Stopped, Unknown)`; aborted + "max iterations reached" → `(Aborted, MaxIterations)`; aborted + "max tokens reached" → `(Aborted, MaxTokens)`; aborted + "max consecutive failures" → `(Aborted, MaxFailures)`; aborted + signal lastMessage → `(Aborted, Signal)`; missing `run:complete` → `(Aborted, Unknown, LogIncomplete=true) + ErrIncompleteLog`; truly malformed JSONL with no parseable records → same synthesized result + `ErrIncompleteLog`; ignore non-final `run:complete`-lookalikes (e.g. `event:"iteration:complete"`).
  - **`NoProgress` derivation:** `commitCount=0` → true (regardless of `successCount`); `commitCount=3` → false; explicit case `successCount=3,commitCount=0` → true (covers the agent-claimed-then-reset path Codex flagged).
  - **`SpawnGnhf` integration via shell-script mock** (pattern from `internal/worker/spawn_test.go`): mock `gnhf` writes a synthetic `.gnhf/runs/<runId>/gnhf.log` + sleeps + exits 0; assert returned `GnhfResult` matches.
  - **Preflight failures:** non-existent path, non-worktree path, detached HEAD, branch mismatch — each returns a typed error and does NOT spawn gnhf.
  - **Context cancellation:** mock script traps SIGTERM and writes a `run:complete` line with `status=aborted, lastMessage="signal"` before exiting; assert we capture that result (proves graceful-shutdown flow works).
  - **Crash mid-flush (synthesized result):** mock writes a partial `gnhf.log` ending with `event:"iteration:complete"` (no `run:complete`); assert returned struct is `(Aborted, Unknown, LogIncomplete=true)` AND error is `ErrIncompleteLog` AND dispatcher can still persist the row.
  - **Worktree-aware exclude path:** test inside a real linked worktree (created via `git worktree add` against an ephemeral parent repo); assert `.gnhf/` lands in the **common** `<parent>/.git/info/exclude` (NOT in `<parent>/.git/worktrees/<name>/info/exclude` — `info/exclude` is part of the common git dir, not per-worktree), AND that calling `SpawnGnhf` twice does NOT duplicate the `.gnhf/` line (idempotency).
  - **Defaults applied:** zero-value `GnhfArgs.MaxIterations` → command-line contains `--max-iterations 30`; zero-value `Timeout` → 4h ceiling; zero-value `GracePeriod` → 30s; zero-value `Agent` → `claude`.
  - **runId discovery — happy path via name-set diff:** seed `.gnhf/runs/` with two pre-existing dirs `A` and `B`; mock creates `C` (post-spawn); separately, after spawn, touch dir `A` so its mtime is newer than `C`'s. Assert we pick `C` (set-difference) and NOT `A` (which would be wrong if we'd used newest-mtime). This is the regression test for the round-3 finding.
  - **runId discovery — multiple new dirs, exactly one parseable:** seed `.gnhf/runs/` with one pre-existing dir; mock creates two new dirs `X` and `Y`, only `X` has a parseable `run:complete`; assert we pick `X`. (Defense-in-depth against impossible-but-theoretical concurrent gnhf invocations.)
  - **runId discovery — multiple new dirs, zero or >1 parseable:** mock creates `X` and `Y`, both have parseable `run:complete`; assert `SpawnGnhf` returns `ErrAmbiguousRunDir{Candidates:[X,Y]}` AND a synthesized `LogIncomplete=true` result (do NOT mtime-guess; surface for operator triage per Round-4 review).
  - **runId discovery — no new dir:** mock fails to create any new `.gnhf/runs/` entry; assert `SpawnGnhf` returns `ErrRunDirNotFound` AND a synthesized `(Aborted, Unknown, LogIncomplete=true)` result (dispatcher must still record state).

<details><summary>Original Step 4.1–4.4 (superseded — kept for traceability)</summary>

> ~~**Step 4.1:** Args struct: `Prompt`, `WorktreePath`, `MaxTokens int64`, `MaxIterations int (default 30)`, `StopWhen string (default ...)`, `Agent string (default "claude")`, `Timeout time.Duration (default 4h)`.~~
> ~~**Step 4.2:** `os/exec.CommandContext(ctx, "gnhf", ..., "--worktree", args.WorktreePath, args.Prompt)` — parses gnhf's last JSON line from stdout for iteration count + tokens.~~
> ~~**Step 4.3:** `GnhfResult{Outcome, Iterations, CommitsMade, TokensUsed, BranchName, NotesExcerpt, Error}` with `Outcome ∈ {success, blocked, failed, timeout}`.~~
> ~~**Step 4.4:** Mock gnhf shell script. Cover clean success, mid-run timeout, hard agent error, missing notes.md.~~

</details>

### Task 5: `dispatchImplement` handler

- [ ] **Step 5.1:** Create `internal/implementer/dispatch.go`. Top-level: `DispatchImplement(ctx, row InboxRow, deps Deps) error`. Steps:
  1. Compute token allowance via `deps.Budget.TokenAllowance(ctx, now)`. If 0, defer the row by N minutes (existing `MarkDeferred` pattern) with reason `"token allowance exhausted for current window"` and return without spawning.
  2. Ensure worktree via `deps.Worktree.EnsureForTask(...)`.
  3. Insert `implementer_runs` row with `gnhf_status=''`, `started_at=now`.
  4. Spawn gnhf via `SpawnGnhf` (Task 4). Stream stdout to the supervisor log via a Tee writer.
  5. On exit: update `implementer_runs` with the full `GnhfResult` — write all `gnhf_*` columns (Step 4.0) plus the derived `outcome` and `tokens_used` (legacy compat columns) using the mapping rules from Step 4.0. If `Status=Stopped AND Reason=StopWhen AND CommitCount>0` and a branch was produced, optionally `gh pr create` (gated by env `IMPLEMENTER_AUTO_PR=true`, default `false` for MVP — first ship lets the user manually open the PR after reviewing the worktree).
  6. Queue an outbox row keyed `(inbox_comment_id, "implement")` containing the formatted `ImplementerSummary` (5–10 line summary + truncated `notes.md` excerpt + PR link if opened + branch name + `(Status, Reason, NoProgress)` tuple). Formatting handled in Task 6.
  7. **Cleanup policy** (gnhf preserves the worktree iff it made commits; we mirror that): preserve worktree if `CommitCount > 0` (operator can still inspect or land the branch); cleanup worktree if `CommitCount == 0` regardless of `Status`/`Reason` (no artifact worth keeping). This collapses the original "cleanup on failed/timeout, preserve on success/blocked" rule into a single commit-count check, which matches gnhf's own worktree-preservation logic at `cli.mjs:4416-4423`.
- [ ] **Step 5.2:** Wire dispatcher in `cmd/supervisor/main.go`: when `NextInboxRow` returns `phase=implement`, route to `DispatchImplement` instead of the existing `dispatchNormal`/`dispatchCompact` paths.
- [ ] **Step 5.3:** Update `NextInboxRow` priority: `answer > compact > implement > normal`. Implement is *lower* than answer/compact (user-facing) and *higher* than normal (because once an implement is queued, it shouldn't starve).
- [ ] **Step 5.4:** Tests: integration-style with fake `Deps`. Cover (a) zero allowance → defer + no spawn, (b) successful spawn → outbox row queued + run row updated, (c) failure → worktree cleaned + outbox row with failure summary, (d) priority ordering preserved.

### Task 6: Lark formatter for implementer summaries

- [ ] **Step 6.1:** `PostImplementerSummary` formatter in `internal/lark/client.go`. Input: `GnhfResult` (from Task 4) + branch + PR URL + allowance ceiling. Compact format suitable for a Lark task thread. Headline maps `(Status, Reason, NoProgress)` to a human verb:

  | Status | Reason | NoProgress | Headline verb |
  |---|---|---|---|
  | stopped | stop_when | false | "finished — stop-when condition met" |
  | stopped | stop_when | true | "halted — stop-when matched but no commits made" |
  | stopped | unknown | * | "stopped — orchestrator returned without explicit reason" |
  | aborted | max_iterations | * | "timed out — max iterations reached" |
  | aborted | max_tokens | * | "timed out — token ceiling reached" |
  | aborted | max_failures | * | "failed — max consecutive iteration failures" |
  | aborted | signal | * | "interrupted — supervisor cancelled" |
  | aborted | unknown | * | "aborted — see notes for context" |
  | aborted | unknown | * | "aborted — gnhf.log incomplete (process crashed before flushing run:complete)" — applies when `LogIncomplete=true` and trumps the row above; operators need to distinguish "real reason unknown" from "log truncated". |

  When `LogIncomplete=true`, append a `⚠ log incomplete` suffix to the headline regardless of which row matched, so operators can spot the synthesized cases at a glance (Round-3). In addition, increment a `implementer_runs_log_incomplete_total` counter when M1b metrics are wired so the tripwire (Step 10.4) can alert on a sustained crash-rate trend (Round-4 — both human-visible and machine-tractable signals).

  Body lines:
  ```
  🤖 implementer (gnhf) <headline>
    • iterations: <Iterations> / <MaxIterations>
    • commits: <CommitCount>
    • tokens used: <InputTokens+OutputTokens> (in: <InputTokens>, out: <OutputTokens>) / <MaxTokens> allowance
    • branch: <BranchName>
    • PR: <PRURL>                ← only if IMPLEMENTER_AUTO_PR=true and a PR was opened
    • notes (excerpt): "<NotesExcerpt — first ~512 bytes of notes.md, line-wrapped>"
  ```
- [ ] **Step 6.2:** Tests for the formatter (table-driven over the 8 `(Status, Reason, NoProgress)` combinations above + edge cases: empty notes, missing branch, no PR, very long notes that must be truncated, zero iterations, `LogIncomplete=true` headline suffix appears regardless of which row matched).

### Task 7: Token allowance refactor

- [ ] **Step 7.1:** Refactor `internal/budget/gate.go`: extract `TokenAllowance(ctx, now)` returning `int64`. The existing `CanSpawn` calls it and checks `> 0`. Existing `CanSpawn` semantics unchanged for callers; new explicit value available for the implementer.
- [ ] **Step 7.2:** Tests for `TokenAllowance`: full-window-fresh returns full budget, mid-window returns remaining, near-end returns small positive, post-window returns 0.

### Task 8: Setup script + service file

- [ ] **Step 8.1:** Create `infra/setup-gnhf.sh`. Idempotent. Pin `gnhf@v0.1.26` (or whichever is latest at deployment time — record in script comment). Verify `gnhf --version` post-install. `--dry-run` mode for CI.
- [ ] **Step 8.2:** Update `infra/claude-vm.service`: add `ReadWritePaths=/var/lib/claude-vm/worktrees` and document the new env vars in a comment.
- [ ] **Step 8.3:** Update `infra/env.example` with: `GNHF_BIN=/usr/local/bin/gnhf`, `IMPLEMENTER_WORKTREE_BASE=/var/lib/claude-vm/worktrees`, `IMPLEMENTER_DEFAULT_REPO=/path/to/managed/repo`, `IMPLEMENTER_AUTO_PR=false`, `IMPLEMENTER_MAX_ITERATIONS=30`, `IMPLEMENTER_STOP_WHEN=...`.
- [ ] **Step 8.4:** Update `.claude/CLAUDE.md`: document the new env vars list in the "Key env vars (Go supervisor)" section.

### Task 9: Self-review + dry-run

- [ ] **Step 9.1:** Run full test suite with `-race`: `go test -race ./...`. Vet: `go vet ./...`. All green.
- [ ] **Step 9.2:** End-to-end dry-run on Caesario's local machine: `claude-vm-supervisor` against an ephemeral SQLite + a small test repo + a real `gnhf` invocation with `--max-tokens 10000` ceiling and a trivial prompt. Confirm worktree appears, gnhf runs, outbox row is queued.
- [ ] **Step 9.3:** Open PR following existing convention (`feat(implementer): autonomous nightly implementer subsystem`). Pre-commit: `code-reviewer` + `adversarial-reviewer` agents per CLAUDE.md review-escalation rule (multi-step feature, multiple files, new domain logic).

### Task 10: Deployment + canary window

- [ ] **Step 10.1:** Deploy via the existing pattern from worker-pool handoff: stop service → backup `.prev` → scp binary → `sudo cp` → `sudo chown` → start service → verify `active (running)`.
- [ ] **Step 10.2:** Run `infra/setup-gnhf.sh` on the VM to install gnhf.
- [ ] **Step 10.3:** Set `IMPLEMENTER_AUTO_PR=false` for the first 2-week canary period — the supervisor produces commits in worktrees but does NOT open PRs automatically. Caesario manually opens PRs after reviewing worktree state. This is the safety latch for first production runs.
- [ ] **Step 10.4:** Define a "tripwire" in M1b metrics (already shipped by then): if `implementer_runs_total{outcome=failed}` ever exceeds 30% of `implementer_runs_total` over a rolling 7-day window, the subsystem auto-disables (env flag flipped, alert posted to Lark). Implement the auto-disable check in `cmd/supervisor/main.go` boot path.
- [ ] **Step 10.5:** After 2 weeks of canary success, flip `IMPLEMENTER_AUTO_PR=true`. Document the flip + measured outcome in a new handoff `2026-XX-XX-implementer-auto-pr-enabled.md`.

---

## Risks accepted (MVP)

| Risk | Mitigation | Why accept |
|------|------------|------------|
| gnhf hangs and doesn't honor SIGINT | `os/exec.CommandContext` + Timeout (4h default) → kills subprocess. Worktree cleanup on timeout. | gnhf v0.1.26 has explicit timeout handling per its README; trust + verify |
| Implementer burns full per-window budget on one bad task | `--max-tokens` cap is enforced pre-spawn. Future M3 can add per-task caps below window cap. | Honest tradeoff: simpler MVP > perfect budget partitioning |
| Worktree base dir grows unbounded | Periodic GC (Task 3 step 3.1) + manual prune option | Disk on e2-medium is 30GB; worktrees are typically <100MB; weeks of headroom |
| Wrong intent classifier picks `implement` for non-implement comments | Conservative heuristic (leading verb match, not whole-comment); WARN log + manual review | False positive burns one window's budget then user can `/cancel-ralph`-style intervene; not catastrophic |
| `gh pr create` fails (auth, network) | Wrapped in retry-with-backoff; on persistent failure, the run keeps its real `outcome` (e.g., `success`) and `pr_url` stays NULL — the formatter shows the run as success-without-PR (operator opens the PR manually from the preserved worktree). PR-creation is a side-effect of `success`, not part of the outcome. | Cleaner UX than tying success status to PR-opening result; metric `implementer_pr_create_failed_total` tracks operationally without changing the outcome enum. |

## Out of scope (M3+ backlog — separate plan)

- **PR review-comment reactor** — auto-route GitHub PR review comments back to the implementer agent for follow-up commits. (This is the agent-orchestrator pattern worth borrowing later.)
- **CI failure reactor** — auto-detect CI failure on the implementer's PR and re-spawn gnhf with the failure log appended to the prompt.
- **Multi-agent fallback** — when Claude rate-limits, retry with `--agent codex` or `--agent copilot` (requires those CLIs installed and authenticated).
- **Per-task token caps** — divide per-window allowance across queued implements, instead of FCFS-take-all.
- **Implementer cost dashboard** — Grafana panel showing implementer-only token usage trends.

## Self-Review checklist

- [ ] All file paths in the file map exist or are explicitly marked Create
- [ ] Each task has at least one test step
- [ ] No task changes existing `dispatchAnswer` or `dispatchCompact` paths
- [ ] All new env vars are documented in `env.example` AND `.claude/CLAUDE.md`
- [ ] The decision note `decisions/2026-04-27-claude-code-vm-autonomous-implementer-loop-choice.md` is referenced from this plan's frontmatter
- [ ] Sequencing is explicit: M1b ships → N=2 stability window → this plan starts
- [ ] Risks accepted are listed with mitigations
- [ ] Out-of-scope items are explicitly named so future "next" handoffs can pick them up

## Related

- Decision: `obsidian-doc-claude/decisions/2026-04-27-claude-code-vm-autonomous-implementer-loop-choice.md`
- Research: `obsidian-doc-claude/research/2026-04-27-ralph-vs-agent-orchestrator.md`
- Design: `obsidian-doc-claude/designs/claude-vm-token-budget-design.md`
- Design: `obsidian-doc-claude/designs/claude-code-vm-multi-session-design.md` (especially "Out of Scope" section, where this subsystem was named)
- Prior plan (shipped): `docs/superpowers/plans/2026-04-20-token-budget-envelope.md`
- Prior plan (in-flight): `docs/superpowers/plans/2026-04-23-supervisor-metrics-plan.md` (M1b — this plan starts AFTER M1b ships)
- Latest handoff: `.claude/handoffs/2026-04-23-200121-worker-pool-shipped.md`
- Earlier "next" pointer: `.claude/handoffs/2026-04-21-201303-autonomous-implementer-next.md`
