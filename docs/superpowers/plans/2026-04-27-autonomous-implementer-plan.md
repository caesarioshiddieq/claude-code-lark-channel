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

**Goal:** Add an autonomous implementer subsystem to the `claude-code-vm` supervisor that picks up `phase=implement` rows during the 19:00–05:00 Asia/Jakarta autonomous window, spawns `gnhf` against an isolated git worktree per Lark task, captures gnhf's per-iteration commit log + final result, and posts the outcome (success / blocked / failed) to the Lark task thread via the existing outbox.

**Architecture:** The supervisor's worker dispatch fork grows a third branch alongside `dispatchAnswer` and `dispatchCompact`: **`dispatchImplement`**. For `phase=implement` rows, the worker (i) computes the per-window token allowance via the existing `budget.CanSpawn()` math, (ii) materializes a per-task git worktree under `/var/lib/claude-vm/worktrees/<task_id>/`, (iii) invokes `gnhf --agent claude --max-tokens <budget> --stop-when "<NL completion phrase>" --worktree <path>` as a subprocess, (iv) parses gnhf's stdout JSON + reads `notes.md` for human-readable summary, (v) writes outcome to a new `implementer_runs` table and to the existing `outbox`. PR opening uses `gh pr create` against the gnhf-produced branch. All changes are additive — `dispatchAnswer` (existing `claude -p` path) and `dispatchCompact` are untouched.

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
| `internal/sqlite/queue.go` | Modify | Add `migration0005`: extend `inbox.phase` enum to include `implement`; create `implementer_runs` table |
| `internal/sqlite/queue_test.go` | Modify | Tests for `migration0005`, `phase=implement` round-trip, `implementer_runs` CRUD |
| `internal/intent/classifier.go` | Create | `Classify(comment string) Phase` — heuristic mapper from Lark comment text to `Phase{normal, implement}`. MVP: keyword + leading verb match. Hard-mode classifier defers to a future PRD. |
| `internal/intent/classifier_test.go` | Create | Table-driven tests for keyword classifier |
| `internal/worktree/manager.go` | Create | `EnsureForTask(taskID, repoPath) (string, error)`, `Cleanup(taskID, success bool) error`, `BaseDir()` helpers |
| `internal/worktree/manager_test.go` | Create | Tests against a real ephemeral git repo (in-test `git init` + `git worktree add`) |
| `internal/implementer/spawn.go` | Create | `SpawnGnhf(ctx, args GnhfArgs) (GnhfResult, error)` — wraps `os/exec.Command("gnhf", ...)` with output streaming, timeout, panic recovery. Parses stdout JSON + reads `notes.md`. |
| `internal/implementer/spawn_test.go` | Create | Tests against a mock `gnhf` shell script (mirrors how spawn.go tests already mock `claude`) |
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

### Task 1: Schema migration — `phase=implement` + `implementer_runs` table

- [ ] **Step 1.1:** Add `migration0005` to `internal/sqlite/queue.go`. The migration: (a) widens any application-layer enum check on `inbox.phase` to accept `implement`; (b) creates `implementer_runs` table. Schema sketch (refine in TDD):
  - `id INTEGER PRIMARY KEY`
  - `inbox_comment_id TEXT NOT NULL` (FK relationship via `(comment_id, phase=implement)`)
  - `task_id TEXT NOT NULL`
  - `started_at INTEGER NOT NULL`
  - `finished_at INTEGER`
  - `outcome TEXT` (one of `success | blocked | failed | timeout`)
  - `gnhf_iterations INTEGER`
  - `gnhf_commits_made INTEGER`
  - `tokens_used INTEGER`
  - `worktree_path TEXT`
  - `branch_name TEXT`
  - `pr_url TEXT`
  - `notes_md_excerpt TEXT` (truncated)
  - `error TEXT`
- [ ] **Step 1.2:** Add CRUD: `InsertImplementerRun`, `UpdateImplementerRunOutcome`, `GetImplementerRunByCommentID`. Mirror existing queue.go style (named consts, `ctx`-aware, `sql.Tx` parameter).
- [ ] **Step 1.3:** TestMigration0005, TestImplementerRunRoundTrip, TestImplementerRunOutcomeUpdate. Pass `-race`.

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

- [ ] **Step 4.1:** Create `internal/implementer/spawn.go`. Args struct:
  - `Prompt string` (the Lark comment that triggered the implement)
  - `WorktreePath string`
  - `MaxTokens int64`
  - `MaxIterations int` (env-tunable ceiling, default 30)
  - `StopWhen string` (env-tunable, default `"all tests pass and the implementation matches the request"`)
  - `Agent string` (default `"claude"`)
  - `Timeout time.Duration` (env-tunable, default 4h — covers a full overnight window with margin)
- [ ] **Step 4.2:** `SpawnGnhf(ctx, args)` does: `os/exec.CommandContext(ctx, "gnhf", "--agent", args.Agent, "--max-tokens", ..., "--max-iterations", ..., "--stop-when", args.StopWhen, "--worktree", args.WorktreePath, args.Prompt)`. Streams stdout to a buffer + the supervisor log; on exit reads `<worktreePath>/notes.md` if present; parses gnhf's last JSON line for iteration count and tokens used.
- [ ] **Step 4.3:** Result struct: `GnhfResult{Outcome, Iterations, CommitsMade, TokensUsed, BranchName, NotesExcerpt, Error}`. `Outcome` is one of `success | blocked | failed | timeout`.
- [ ] **Step 4.4:** Tests use a mock `gnhf` shell script (shell-out pattern already used in `internal/worker/spawn_test.go` for the `claude` mock). Cover: clean success, mid-run timeout via `ctx.Done()`, hard agent error, missing notes.md.

### Task 5: `dispatchImplement` handler

- [ ] **Step 5.1:** Create `internal/implementer/dispatch.go`. Top-level: `DispatchImplement(ctx, row InboxRow, deps Deps) error`. Steps:
  1. Compute token allowance via `deps.Budget.TokenAllowance(ctx, now)`. If 0, defer the row by N minutes (existing `MarkDeferred` pattern) with reason `"token allowance exhausted for current window"` and return without spawning.
  2. Ensure worktree via `deps.Worktree.EnsureForTask(...)`.
  3. Insert `implementer_runs` row with `outcome=NULL`, `started_at=now`.
  4. Spawn gnhf. Stream output to log.
  5. On exit: update `implementer_runs` with full result. If success and a branch was produced, optionally `gh pr create` (gated by env `IMPLEMENTER_AUTO_PR=true`, default `false` for MVP — first ship lets the user manually open the PR after reviewing the worktree).
  6. Queue an outbox row keyed `(inbox_comment_id, "implement")` containing the formatted `ImplementerSummary` (5–10 line summary + truncated `notes.md` excerpt + PR link if opened + branch name).
  7. Cleanup worktree on `outcome=failed` or `outcome=timeout`. Preserve on `success` and `blocked`.
- [ ] **Step 5.2:** Wire dispatcher in `cmd/supervisor/main.go`: when `NextInboxRow` returns `phase=implement`, route to `DispatchImplement` instead of the existing `dispatchNormal`/`dispatchCompact` paths.
- [ ] **Step 5.3:** Update `NextInboxRow` priority: `answer > compact > implement > normal`. Implement is *lower* than answer/compact (user-facing) and *higher* than normal (because once an implement is queued, it shouldn't starve).
- [ ] **Step 5.4:** Tests: integration-style with fake `Deps`. Cover (a) zero allowance → defer + no spawn, (b) successful spawn → outbox row queued + run row updated, (c) failure → worktree cleaned + outbox row with failure summary, (d) priority ordering preserved.

### Task 6: Lark formatter for implementer summaries

- [ ] **Step 6.1:** `PostImplementerSummary` formatter in `internal/lark/client.go`. Compact format suitable for a Lark task thread. Examples below — refine during implementation:

```
🤖 implementer (gnhf) finished — outcome: success
  • iterations: 8 / 30
  • commits: 5
  • tokens used: 412k / 500k allowance
  • branch: implement/ab4b49a4-...
  • PR: https://github.com/...  ← if IMPLEMENTER_AUTO_PR=true
  • notes (excerpt): "Reverted ad-hoc shimming in step 4; rewrote
    queue. Tests pass. Linter clean."
```

```
🤖 implementer (gnhf) blocked — outcome: blocked
  • iterations: 12 / 30
  • notes: "Cannot proceed without API key for
    THIRD_PARTY_SERVICE. Halting per --stop-when condition
    'do not invent fake credentials'."
```

- [ ] **Step 6.2:** Tests for the formatter (table-driven, success / blocked / failed / timeout).

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
| `gh pr create` fails (auth, network) | Wrapped in retry-with-backoff; on persistent failure, marks run as `success_no_pr` and the user opens the PR manually from the preserved worktree | Cleaner UX than tying success status to PR-opening result |

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
