# Tasks 1–3 + P1 Review Fixes Shipped — Autonomous Implementer

**Date:** 2026-04-27
**Branch:** `feat/autonomous-implementer` (9 commits ahead of `main`, pushed to origin)
**Status:** Foundation layer complete; **resume at Task 4 (gnhf spawn wrapper)**.
**Plan:** `docs/superpowers/plans/2026-04-27-autonomous-implementer-plan.md`

## What shipped

### Tasks 1–3 (foundation)

| # | Task | RED commit | GREEN commit | New code |
|---|------|------------|--------------|----------|
| 1 | `migration0004` + `implementer_runs` table + 3 CRUD methods | `1bd5438` | `7960ea0` | `internal/sqlite/implementer_runs.go` (115 LoC) + test (166 LoC); `internal/sqlite/queue.go` +25 lines |
| 2 | Leading-verb intent classifier (EN+ID, 5 verbs, false-negative-biased) | `00e92ad` | `68b25d4` | `internal/intent/classifier.go` (56 LoC) + test (64 LoC) |
| 3 | Per-task worktree `Manager` (struct, idempotent ensure + GC) | `4f5ce14` | `07fc52b` | `internal/worktree/manager.go` (initial) + test (149 LoC) |

### P1 Review Fixes (post-checkpoint)

| Finding (source) | Where | RED | GREEN |
|---|---|---|---|
| TOCTOU race in `EnsureForTask` (go-reviewer) | `worktree/manager.go` | `90c20a7` | `8846b33` |
| Idempotency probe is path-only, not worktree-state aware (go-reviewer) | `worktree/manager.go` | `90c20a7` | `8846b33` |
| GC mtime check was top-level only — would kill running tasks (code-reviewer) | `worktree/manager.go` | `90c20a7` | `8846b33` |
| Branch-name fixture drift in Task 1 test (code-reviewer) | `sqlite/implementer_runs_test.go` | `90c20a7` | (cosmetic, no GREEN needed) |

The P1 GREEN commit (`8846b33`) addresses all three worktree fixes together: per-`taskID` `sync.Mutex` map serializes concurrent ops on the same taskID, `git worktree list --porcelain` probe detects stale dirs and triggers recovery (`os.RemoveAll` + fresh creation), `filepath.WalkDir`-based newest-child-mtime check in `GarbageCollect` no longer kills long-running tasks whose gnhf process modifies existing files (which doesn't bump parent dir mtime).

## Plan deviations (intentional, documented in each commit body)

1. **`migration0004` not `migration0005`** — the plan miscounted; real registry only had 0002+0003.
2. **`worktree.Manager` struct vs. plan's free-function sketch** — plan's `Cleanup(taskID, success)` lacked the `repoPath` arg needed for `git worktree remove`. `BaseDir()` stays as a free function (env lookup is naturally stateless).
3. **Step 2.2 (wire classifier into supervisor `processNewComment`) and Step 2.4 (WARN log on PhaseImplement)** deferred to Task 5 (`dispatchImplement`) — the routing fork is where these are load-bearing.
4. **Step 3.3 (systemd `claude-vm.service` `ReadWritePaths` edit)** deferred to Task 8 (deployment).
5. **`internal/intent/README.md`** consolidated into the Go package doc comment in `classifier.go` — single source of truth via godoc.

## Verification (at HEAD `8846b33`)

```text
go vet ./...                         clean
go build ./...                       clean
go test ./... -race -count=1         109 tests pass in 9 packages
                                     (was 84 before this work)
                                       budget   echo   gc   intent  lark
                                       sqlite   worker worktree
                                     no regressions in any pre-existing package
```

## Carry-over risks (still open)

1. **Task 10.4 tripwire references "M1b metrics already shipped"** — but M1b was deferred indefinitely per `.claude/handoffs/2026-04-27-130317-m1b-skipped-n2-bumped.md`. When Task 10 lands, rework the tripwire to read from `queue.db` SQL snapshots (same pattern as `infra/headroom-check.sh`) instead of Prometheus.
2. **`MAX_CONCURRENT_SPAWNS_GLOBAL=2` is live on the VM** since 2026-04-27 (no observed N=2 stability window because M1b was skipped). Task 4–5 will start dispatching `phase=implement` rows alongside existing `phase=normal` traffic; watch `journalctl -u claude-vm` for any contention surprises.

## Task 4 design notes (resume here)

Task 4 = `internal/implementer/spawn.go` — `SpawnGnhf(ctx, args GnhfArgs) (GnhfResult, error)`.

### Decided

- **Pattern:** Mirror `internal/worker/spawn_test.go` — split parsing from I/O. Extract `ParseGnhfOutput(stdout []byte, notesContent string) (GnhfResult, error)` as a pure function tested exhaustively with synthetic bytes. Use a `t.TempDir()` shell-script mock for one happy-path integration test via `t.Setenv("PATH", tmpDir+":"+oldPath)`. Smoke-test cancelled-context path against the same fake (or the real `gnhf` if present).
- **Args struct (per plan Step 4.1):** `Prompt`, `WorktreePath`, `MaxTokens int64`, `MaxIterations int (default 30)`, `StopWhen string (default "all tests pass and the implementation matches the request")`, `Agent string (default "claude")`, `Timeout time.Duration (default 4h)`. Defaults applied in `SpawnGnhf` when zero-valued.
- **Result struct (per Step 4.3):** `Outcome string ∈ {success, blocked, failed, timeout}`, `Iterations int`, `CommitsMade int`, `TokensUsed int`, `BranchName string`, `NotesExcerpt string`, `Error string`.
- **`Outcome` should be a typed string** per the code-reviewer's P2 finding (`type Outcome string` with constants) — promote to type during Task 4 to avoid stringly-typed call sites in Task 5.

### Open questions / things to check while implementing

- gnhf v0.1.26's actual stdout JSON schema — confirm field names from `npm view gnhf` or the gnhf README before pinning `ParseGnhfOutput`.
- `notes.md` location — plan says `<worktreePath>/notes.md`. Confirm with gnhf docs.
- Should `SpawnGnhf` spawn under `setpgid` so a context-cancel sends `SIGTERM` to the whole gnhf subprocess group? Worth doing for clean shutdown but may not be strictly required for v1.

### Test cases for the RED commit

1. `TestParseGnhfOutput_Success` — synthetic stdout with `{"outcome":"success","iterations":7,...}` last line + valid notes.md content → expected `GnhfResult`.
2. `TestParseGnhfOutput_NoNotesMD` — same stdout, empty notesContent → result with empty `NotesExcerpt`, no error.
3. `TestParseGnhfOutput_TimeoutOutcome` / `_BlockedOutcome` / `_FailedOutcome` — table-driven over the 4 outcome strings.
4. `TestParseGnhfOutput_MalformedJSON` — corrupt last line → returns error.
5. `TestSpawnGnhf_HappyPath` — mock gnhf script writes notes.md + JSON, asserts result fields.
6. `TestSpawnGnhf_ContextCancelled` — cancel ctx before spawn, expect immediate error.
7. `TestSpawnGnhf_DefaultsApplied` — pass zero-value Args, assert defaults end up in the cmd line (use `--max-iterations=30` etc. visible in mock script's args dump).

### Don't forget after Task 4

- Code-reviewer P2 #4: `UpdateImplementerRunOutcome` should check `RowsAffected()` and surface `sql.ErrNoRows` when 0. Easy win to land alongside Task 5.
- Code-reviewer P2 #5: Rename `ImplementerRun.Error` → `ErrorMsg` (or `LastError`) — `r.Error` reads as a method call.
- Go-reviewer P2 #5: `t.Setenv("HOME", t.TempDir())` in worktree tests to isolate from `commit.gpgsign=true` global config; will flake on machines with signed commits.
- Go-reviewer P2 #3: Replace `_ = exec…Run()` swallows in `worktree.Cleanup` with a debug-level log line.
- Code-reviewer P2 #6: `TestCleanup_MissingIsNoop` — assert documented "no-op on missing" behavior.

## Branch state

```text
8846b33 fix(worktree): apply P1 review findings (mutex, porcelain probe, child-mtime GC)
90c20a7 test(worktree+sqlite): reproducers for P1 review findings
07fc52b feat(worktree): per-task worktree Manager with idempotent ensure + GC
4f5ce14 test(worktree): add reproducer for Manager (Ensure/Cleanup/GC)
68b25d4 feat(intent): leading-verb classifier (EN + ID, false-negative-biased)
00e92ad test(intent): add reproducer for Classify with EN/ID leading-verb heuristic
7960ea0 feat(sqlite): migration0004 + implementer_runs CRUD
1bd5438 test(sqlite): add reproducer for migration0004 + implementer_runs CRUD
55c876f docs(plan): autonomous implementer subsystem (11 tasks)   ← also on main
```

## Resume instructions

```bash
cd ~/Kerja/claude-code-lark-channel-implementer
git status                                    # should be clean
git log --oneline -10                          # confirm at 8846b33
go test ./... -race -count=1                  # confirm 109/109 still green
# Then begin Task 4: see "Task 4 design notes" above.
```

The worktree at `~/Kerja/claude-code-lark-channel-implementer/` (branch `feat/autonomous-implementer`) is the canonical workspace. The main repo at `~/Kerja/claude-code-lark-channel/` is on `main` and untouched by this work except for `55c876f` (the plan file).
