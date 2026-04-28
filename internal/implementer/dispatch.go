package implementer

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/caesarioshiddieq/claude-code-lark-channel/internal/budget"
	"github.com/caesarioshiddieq/claude-code-lark-channel/internal/sqlite"
)

// DBClient is the narrow interface DispatchImplement needs from *sqlite.DB.
// Defined here (interface-where-used per Go conventions) so *sqlite.DB satisfies
// it without the implementer package importing the concrete type for the interface.
type DBClient interface {
	MarkDeferred(ctx context.Context, commentID string, scheduledFor int64, content string) error
	MarkInboxProcessed(ctx context.Context, commentID string) error
	InsertImplementerRun(ctx context.Context, r sqlite.ImplementerRun) (int64, error)
	FinalizeImplementerRun(ctx context.Context, id int64, fin sqlite.ImplementerRunFinalize) error
	OutboxInsertPhased(ctx context.Context, commentID, taskID, replyTo, phase string) (bool, error)
	OutboxCheck(ctx context.Context, hash string) (larkCommentID string, found bool, err error)
	OutboxMarkPosted(ctx context.Context, hash, larkCommentID string) error
	GetImplementerRunByCommentID(ctx context.Context, commentID string) (sqlite.ImplementerRun, bool, error)
}

// WorktreeClient is the narrow interface DispatchImplement needs from *worktree.Manager.
// Matches the actual Manager method signatures exactly (no repoPath param — Manager holds it).
type WorktreeClient interface {
	EnsureForTask(ctx context.Context, taskID string) (path, branch string, err error)
	Cleanup(ctx context.Context, taskID string, success bool) error
}

// SpawnFunc is the function signature for spawning a gnhf subprocess.
// In production this is SpawnGnhf; in tests it is a fake.
type SpawnFunc func(ctx context.Context, args GnhfArgs) (GnhfResult, error)

// LarkClient is the narrow interface DispatchImplement needs to post a
// formatted summary comment back to the Lark task thread.
// Defined here (interface-where-used) — *lark.Client satisfies it.
type LarkClient interface {
	PostComment(ctx context.Context, taskID, content, replyToCommentID string) (string, error)
}

// Deps holds all external dependencies for DispatchImplement, injected at
// call time to allow test substitution without a global registry.
type Deps struct {
	DB         DBClient
	Worktree   WorktreeClient
	Spawn      SpawnFunc
	LarkClient LarkClient // nil → skip inline post (backward-compat with existing tests)
	RepoPath   string     // canonical managed-repo path on disk
	Now        func() time.Time
	JitterMin  int
}

// deriveOutcome maps a GnhfResult to the legacy outcome string per the
// canonical mapping rules in the Task 5 plan (Step 4.0):
//
//	"success" — Status=Stopped AND Reason=StopWhen AND NoProgress=false
//	"blocked" — Status=Stopped AND Reason=StopWhen AND NoProgress=true
//	"stopped" — Status=Stopped AND Reason=Unknown (regardless of NoProgress)
//	"timeout" — Status=Aborted AND Reason ∈ {MaxIterations, MaxTokens}
//	"failed"  — Status=Aborted AND Reason ∈ {MaxFailures, Signal, Unknown}
//
// Critical: NoProgress does NOT override Aborted outcomes. An aborted-max-tokens
// run with no commits stays "timeout", not "blocked".
func deriveOutcome(r GnhfResult) string {
	switch r.Status {
	case StatusStopped:
		if r.Reason == ReasonStopWhen {
			if r.NoProgress {
				return "blocked"
			}
			return "success"
		}
		return "stopped"
	case StatusAborted:
		switch r.Reason {
		case ReasonMaxIterations, ReasonMaxTokens:
			return "timeout"
		default:
			return "failed"
		}
	default:
		return "failed"
	}
}

// tryOpenPR invokes `gh pr create` if IMPLEMENTER_AUTO_PR=true AND the run
// succeeded with commits. Returns the PR URL on success, "" on any failure.
// gh failures are logged to stderr and never propagated — PR creation is a
// side-effect of success, not part of the outcome (per Risks Accepted table).
func tryOpenPR(ctx context.Context, outcome, branchName, prompt string) string {
	if os.Getenv("IMPLEMENTER_AUTO_PR") != "true" {
		return ""
	}
	if outcome != "success" || branchName == "" {
		return ""
	}

	title := []rune(prompt)
	if len(title) > 60 {
		title = title[:60]
	}
	prTitle := "implement: " + string(title)

	// #nosec G204 — branchName and prTitle come from internal state, not raw HTTP input.
	cmd := exec.CommandContext(ctx, "gh", "pr", "create",
		"--head", branchName,
		"--title", prTitle,
		"--body", "Autonomous implementation via Lark task comment.",
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "implementer: gh pr create: %v\n", err)
		return ""
	}
	return strings.TrimSpace(out.String())
}

// resultFromImplementerRun reconstructs a GnhfResult from a persisted
// implementer_runs row. Used for crash-recovery (case B): the prior run
// completed and finalized but we crashed before MarkInboxProcessed, so we
// re-format and re-post from the persisted telemetry without respawning gnhf.
func resultFromImplementerRun(r sqlite.ImplementerRun) GnhfResult {
	return GnhfResult{
		Status:        Status(r.GnhfStatus),
		Reason:        Reason(r.GnhfReason),
		Iterations:    r.GnhfIterations,
		SuccessCount:  r.GnhfSuccessCount,
		FailCount:     r.GnhfFailCount,
		CommitCount:   r.GnhfCommitsMade,
		InputTokens:   r.GnhfInputTokens,
		OutputTokens:  r.GnhfOutputTokens,
		RunID:         r.GnhfRunID,
		NotesExcerpt:  r.NotesMDExcerpt,
		LastMessage:   r.GnhfLastMessage,
		NoProgress:    r.GnhfNoProgress,
		LogIncomplete: false, // we have a finalized row, log was complete
	}
}

// postSummaryAndMarkProcessed handles the tail of all DispatchImplement paths
// (normal completion, case B recovery, case C recovery): insert (or re-detect)
// the outbox marker, post the formatted summary to Lark when the post hasn't
// happened yet, mark the outbox row posted, and finally MarkInboxProcessed.
// All errors are logged; none propagate.
//
// Codex round-3 #1: case-B recovery is the EXACT crash window where the
// outbox marker was already inserted before the crash, so InsertPhased
// returns inserted=false. Using inserted as the dedup signal would skip the
// post and permanently lose the implementer summary. Instead, on
// inserted=false, OutboxCheck reads lark_comment_id: empty string means the
// post never happened (recovery path → re-post); non-empty means the post
// already succeeded (idempotent re-run → skip). OutboxMarkPosted records the
// returned lark_comment_id on successful post so the next replay sees it.
func postSummaryAndMarkProcessed(
	ctx context.Context,
	deps Deps,
	row sqlite.InboxRow,
	result GnhfResult,
	branchName, prURL string,
	maxIterations int,
	maxTokens int64,
) {
	hash := row.CommentID + ":implement"

	inserted, outboxErr := deps.DB.OutboxInsertPhased(ctx, row.CommentID, row.TaskID, row.CommentID, "implement")
	if outboxErr != nil {
		log.Printf("dispatchImplement: OutboxInsertPhased %s: %v", row.CommentID, outboxErr)
		// Continue: still try to MarkInboxProcessed so the inbox doesn't loop.
		if err := deps.DB.MarkInboxProcessed(ctx, row.CommentID); err != nil {
			log.Printf("dispatchImplement: MarkInboxProcessed %s: %v", row.CommentID, err)
		}
		return
	}

	// Decide whether to post. Two truthful signals:
	//   1. inserted=true  → fresh marker, definitely haven't posted yet.
	//   2. inserted=false → marker existed; check lark_comment_id to disambiguate
	//      between "post already succeeded" and "marker inserted but post never
	//      happened" (case B/C crash window).
	shouldPost := inserted
	if !inserted {
		existingLarkID, found, checkErr := deps.DB.OutboxCheck(ctx, hash)
		switch {
		case checkErr != nil:
			log.Printf("dispatchImplement: OutboxCheck %s: %v (skipping post)", row.CommentID, checkErr)
		case !found:
			// Pathological: InsertPhased said not-inserted, so the row should
			// exist. Log and skip post defensively.
			log.Printf("dispatchImplement: outbox marker missing despite !inserted for %s — skipping post", row.CommentID)
		case existingLarkID == "":
			// Marker exists but never posted (case B/C recovery): re-post.
			log.Printf("dispatchImplement: recovering — outbox marker exists for %s but no lark_comment_id; re-posting", row.CommentID)
			shouldPost = true
		default:
			// existingLarkID != "" → already posted on a prior run, skip.
			log.Printf("dispatchImplement: outbox marker already posted for %s (lark_comment_id=%s) — skipping",
				row.CommentID, existingLarkID)
		}
	}

	if shouldPost && deps.LarkClient != nil {
		msg := FormatImplementerSummary(result, branchName, prURL, maxIterations, maxTokens)
		larkCommentID, postErr := deps.LarkClient.PostComment(ctx, row.TaskID, msg, row.CommentID)
		if postErr != nil {
			log.Printf("dispatchImplement: PostComment %s: %v", row.CommentID, postErr)
			// Don't MarkPosted on failure — next replay will retry via OutboxCheck path.
		} else if markErr := deps.DB.OutboxMarkPosted(ctx, hash, larkCommentID); markErr != nil {
			log.Printf("dispatchImplement: OutboxMarkPosted %s: %v", row.CommentID, markErr)
			// Non-fatal: post succeeded; the next replay would re-post (acceptable
			// over the rare DB hiccup; Lark dedup is best-effort here).
		}
	}

	if err := deps.DB.MarkInboxProcessed(ctx, row.CommentID); err != nil {
		log.Printf("dispatchImplement: MarkInboxProcessed %s: %v", row.CommentID, err)
	}
}

// DispatchImplement executes the full autonomous-implementer flow for a single
// inbox row with phase="implement".
//
// LOCK CONTRACT: the caller MUST hold the per-task flock (worker.LockTask) for
// row.TaskID before calling. This function does NOT acquire the lock. Rationale:
// a release/reacquire handoff (where processOne unlocks before DispatchImplement
// re-locks) leaves a window in which another supervisor process could pick up
// the same comment and double-run. Holding the outer lock through dispatch
// closes that window. See cmd/supervisor/main.go's processOne for the canonical
// caller pattern.
//
// CRASH RECOVERY (codex round-2 #1): before any worktree/spawn work, check
// whether an implementer_runs row already exists for this comment. Three cases:
//
//   - A) no prior row → fresh start, normal flow
//   - B) prior row finalized (finished_at != nil) → crash between Finalize and
//     MarkInboxProcessed. Skip respawn; reformat-and-repost from the persisted
//     row so the operator still sees the result, then MarkInboxProcessed.
//   - C) prior row not finalized (finished_at == nil) → crash mid-spawn. Don't
//     respawn (the worktree may be in partial state). Synthesize a failure
//     finalize on the existing row, post a "supervisor crashed" summary, and
//     MarkInboxProcessed.
//
// Flow (case A, fresh start):
//  1. Budget gate (CanSpawn) — defer on rejection.
//  2. Idempotency guard (GetImplementerRunByCommentID) — short-circuit B/C.
//  3. Worktree ensure (EnsureForTask).
//  4. Insert implementer_runs row (started_at only).
//  5. Spawn gnhf subprocess.
//  6. Finalize run row (all gnhf_* + derived outcome + tokens_used + finished_at).
//  7. Auto-PR (env-gated, OFF by default for MVP).
//  8. Cleanup worktree (preserve if CommitCount>0, remove otherwise).
//  9. Outbox row + inline Lark post + MarkInboxProcessed
//     (postSummaryAndMarkProcessed; shared with crash-recovery paths).
//
// Returns nil in all handled cases including ErrIncompleteLog. Non-nil only
// for unexpected errors (EnsureForTask, InsertImplementerRun) that the caller
// should log and may choose to defer/retry.
func DispatchImplement(ctx context.Context, row sqlite.InboxRow, deps Deps) error {
	now := deps.Now()

	// Step 1: Budget gate.
	if canSpawn, reason := budget.CanSpawn(ctx, row.Source, now); !canSpawn {
		log.Printf("dispatchImplement: gated %s (%s): %s", row.CommentID, row.Source, reason)
		nextNight := budget.JitteredNightStart(now, deps.JitterMin)
		if err := deps.DB.MarkDeferred(ctx, row.CommentID, nextNight.UnixMilli(), row.Content); err != nil {
			log.Printf("dispatchImplement: MarkDeferred %s: %v", row.CommentID, err)
		}
		return nil
	}

	// Resolve ceiling values once — used by all paths (normal + recovery) so
	// the formatter sees the same knobs gnhf would have seen.
	resolved := GnhfArgs{}
	ApplyDefaults(&resolved)
	spawnMaxIterations := resolved.MaxIterations
	spawnMaxTokens := resolved.MaxTokens // 0 = unbounded (no allowance line)

	// Step 2: Idempotency guard — check for a prior implementer_runs row.
	existing, found, lookupErr := deps.DB.GetImplementerRunByCommentID(ctx, row.CommentID)
	if lookupErr != nil {
		// Codex round-3 #2: lookup failure must DEFER the row, not continue as
		// fresh — continuing would risk a duplicate gnhf spawn the moment the
		// transient DB error clears (the prior row would still be there and we
		// just couldn't see it). MarkDeferred + return shifts the retry into
		// the next poll cycle by which point the DB should be healthy.
		log.Printf("dispatchImplement: GetImplementerRunByCommentID %s: %v (deferring)", row.CommentID, lookupErr)
		deferUntil := deps.Now().Add(60 * time.Second).UnixMilli()
		if markErr := deps.DB.MarkDeferred(ctx, row.CommentID, deferUntil, row.Content); markErr != nil {
			log.Printf("dispatchImplement: MarkDeferred (lookup fail): %v", markErr)
		}
		return nil
	}
	if found && existing.FinishedAt != nil {
		// Case B: prior run completed; we crashed before MarkInboxProcessed.
		// Reformat from persisted telemetry and re-post; do not respawn.
		log.Printf("dispatchImplement: recovering completed run for %s (run id=%d) — skipping respawn",
			row.CommentID, existing.ID)
		recoveredResult := resultFromImplementerRun(existing)
		postSummaryAndMarkProcessed(ctx, deps, row, recoveredResult,
			existing.BranchName, existing.PRURL, spawnMaxIterations, spawnMaxTokens)
		return nil
	}
	if found && existing.FinishedAt == nil {
		// Case C: prior run started but never finalized. Worktree may be in
		// partial state. Synthesize a failure finalize on the existing row.
		log.Printf("dispatchImplement: recovering interrupted run for %s (run id=%d) — synthesizing failure",
			row.CommentID, existing.ID)
		synthResult := GnhfResult{
			Status:        StatusAborted,
			Reason:        ReasonUnknown,
			LastMessage:   "supervisor crashed mid-run",
			LogIncomplete: true,
		}
		fin := sqlite.ImplementerRunFinalize{
			FinishedAt:      deps.Now().UnixMilli(),
			Outcome:         deriveOutcome(synthResult),
			GnhfStatus:      string(synthResult.Status),
			GnhfReason:      string(synthResult.Reason),
			GnhfLastMessage: synthResult.LastMessage,
			Error:           "supervisor crashed mid-run; manual replay required",
		}
		if err := deps.DB.FinalizeImplementerRun(ctx, existing.ID, fin); err != nil {
			log.Printf("dispatchImplement: FinalizeImplementerRun (recovery) %s: %v", row.CommentID, err)
			// Non-fatal: still post and mark processed so the inbox doesn't loop.
		}
		postSummaryAndMarkProcessed(ctx, deps, row, synthResult,
			existing.BranchName, "", spawnMaxIterations, spawnMaxTokens)
		return nil
	}

	// Case A (fresh start) continues below.

	// Step 3: Worktree.
	wtPath, branchName, err := deps.Worktree.EnsureForTask(ctx, row.TaskID)
	if err != nil {
		log.Printf("dispatchImplement: EnsureForTask %s: %v", row.TaskID, err)
		return fmt.Errorf("dispatchImplement: EnsureForTask: %w", err)
	}

	// Step 4: Insert run row (start marker only; finalized after spawn).
	// Reuse the captured `now` for started_at — this records WHEN dispatch began,
	// before the spawn-elapsed time is added. finished_at uses a FRESH deps.Now()
	// after Spawn returns, so the duration (finished_at - started_at) is correct.
	runID, err := deps.DB.InsertImplementerRun(ctx, sqlite.ImplementerRun{
		InboxCommentID: row.CommentID,
		TaskID:         row.TaskID,
		StartedAt:      now.Unix(),
		WorktreePath:   wtPath,
		BranchName:     branchName,
	})
	if err != nil {
		log.Printf("dispatchImplement: InsertImplementerRun %s: %v", row.CommentID, err)
		return fmt.Errorf("dispatchImplement: InsertImplementerRun: %w", err)
	}

	// Build the full GnhfArgs for spawn — Prompt/WorktreePath/ExpectedBranch on
	// top of the already-resolved ceilings. ApplyDefaults is idempotent; SpawnGnhf
	// calls it again internally (no-op).
	args := GnhfArgs{
		Prompt:         row.Content,
		WorktreePath:   wtPath,
		ExpectedBranch: branchName,
		MaxIterations:  spawnMaxIterations,
		MaxTokens:      spawnMaxTokens,
	}

	// Step 5: Spawn gnhf subprocess.
	result, spawnErr := deps.Spawn(ctx, args)

	// Step 6: Derive outcome and finalize run row.
	// For ErrIncompleteLog/*ErrAmbiguousRunDir the result is a usable synthesized
	// struct — we always persist it. spawnErr only drives errStr / outcome override.
	outcome := deriveOutcome(result)
	errStr := ""
	if spawnErr != nil {
		errStr = spawnErr.Error()
		// Any spawn error on a result that would otherwise be "success",
		// "blocked", or "stopped" is downgraded to "failed" because the run
		// did not complete cleanly.
		switch outcome {
		case "success", "blocked", "stopped":
			outcome = "failed"
		}
	}

	fin := sqlite.ImplementerRunFinalize{
		Outcome:          outcome,
		GnhfStatus:       string(result.Status),
		GnhfReason:       string(result.Reason),
		GnhfIterations:   result.Iterations,
		GnhfCommitsMade:  result.CommitCount,
		GnhfSuccessCount: result.SuccessCount,
		GnhfFailCount:    result.FailCount,
		GnhfInputTokens:  result.InputTokens,
		GnhfOutputTokens: result.OutputTokens,
		GnhfRunID:        result.RunID,
		GnhfNoProgress:   result.NoProgress,
		GnhfLastMessage:  result.LastMessage,
		TokensUsed:       result.InputTokens + result.OutputTokens,
		NotesMDExcerpt:   result.NotesExcerpt,
		Error:            errStr,
	}

	// Step 7: Auto-PR (env-gated OFF by default for MVP).
	prURL := ""
	if result.CommitCount > 0 {
		prURL = tryOpenPR(ctx, outcome, branchName, row.Content)
	}
	fin.PRURL = prURL

	// Inject the finalize timestamp from a FRESH deps.Now() call — NOT the
	// captured `now` at function entry. Spawn can run for minutes/hours; reusing
	// `now` would record finished_at == started_at and lose the elapsed duration.
	// Schema quirk inherited from migration0004: started_at uses Unix() (seconds),
	// finished_at uses UnixMilli() (milliseconds).
	fin.FinishedAt = deps.Now().UnixMilli()

	if err := deps.DB.FinalizeImplementerRun(ctx, runID, fin); err != nil {
		// On finalize failure, surface pr_url in the log line if we managed to
		// open one — otherwise the URL is permanently lost (the row UPDATE
		// didn't land). Operators can recover by hand from the log.
		// TODO(M3): on persistent DB failure, propagate err so caller applies
		// fast-fail defer instead of silently MarkInboxProcessed-ing. For MVP,
		// log + continue is acceptable — telemetry loss is rare and recoverable
		// from gnhf.log on disk.
		if fin.PRURL != "" {
			log.Printf("dispatchImplement: FinalizeImplementerRun %s failed with pr_url=%s — URL may be lost: %v",
				row.CommentID, fin.PRURL, err)
		} else {
			log.Printf("dispatchImplement: FinalizeImplementerRun %s: %v", row.CommentID, err)
		}
		// Non-fatal: continue so outbox and cleanup still execute.
	}

	// Step 8: Cleanup worktree.
	// Preserve when commits were made; remove otherwise (avoids unbounded disk growth).
	cleanupSuccess := result.CommitCount > 0
	if err := deps.Worktree.Cleanup(ctx, row.TaskID, cleanupSuccess); err != nil {
		log.Printf("dispatchImplement: Cleanup %s: %v", row.TaskID, err)
	}

	// Step 9: Outbox row + inline Lark post + MarkInboxProcessed (shared tail
	// across normal flow + crash-recovery cases B and C).
	postSummaryAndMarkProcessed(ctx, deps, row, result,
		branchName, prURL, spawnMaxIterations, spawnMaxTokens)

	return nil
}
