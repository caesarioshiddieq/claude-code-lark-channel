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
	"github.com/caesarioshiddieq/claude-code-lark-channel/internal/worker"
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

// Deps holds all external dependencies for DispatchImplement, injected at
// call time to allow test substitution without a global registry.
type Deps struct {
	DB        DBClient
	Worktree  WorktreeClient
	Spawn     SpawnFunc
	RepoPath  string // canonical managed-repo path on disk
	Now       func() time.Time
	JitterMin int
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

// DispatchImplement executes the full autonomous-implementer flow for a single
// inbox row with phase="implement". Safe to call from any goroutine and any
// entry point — the budget gate is re-checked internally for defence-in-depth.
//
// Flow:
//  1. Budget gate (CanSpawn) — defer on rejection.
//  2. Per-task flock (LockTask / UnlockTask).
//  3. Worktree ensure (EnsureForTask).
//  4. Insert implementer_runs row (started_at only).
//  5. Spawn gnhf subprocess.
//  6. Finalize run row (all gnhf_* + derived outcome + tokens_used).
//  7. Auto-PR (env-gated, OFF by default for MVP).
//  8. Outbox row (phase="implement").
//  9. Cleanup worktree (preserve if CommitCount>0, remove otherwise).
//  10. MarkInboxProcessed.
//
// Returns nil in all handled cases including ErrIncompleteLog. Non-nil only
// for unexpected errors (LockTask failure, EnsureForTask failure) that the
// caller should log and may choose to defer/retry.
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

	// Step 2: Per-task flock.
	lockFile, err := worker.LockTask(row.TaskID)
	if err != nil {
		log.Printf("dispatchImplement: LockTask %s: %v", row.TaskID, err)
		return fmt.Errorf("dispatchImplement: LockTask: %w", err)
	}
	defer worker.UnlockTask(lockFile)

	// Step 3: Worktree.
	wtPath, branchName, err := deps.Worktree.EnsureForTask(ctx, row.TaskID)
	if err != nil {
		log.Printf("dispatchImplement: EnsureForTask %s: %v", row.TaskID, err)
		return fmt.Errorf("dispatchImplement: EnsureForTask: %w", err)
	}

	// Step 4: Insert run row (start marker only; finalized after spawn).
	// Reuse the captured `now` rather than calling deps.Now() again — keeps
	// timestamps consistent and avoids advancing a synthetic test clock twice.
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

	// Step 5: Spawn gnhf subprocess.
	result, spawnErr := deps.Spawn(ctx, GnhfArgs{
		Prompt:         row.Content,
		WorktreePath:   wtPath,
		ExpectedBranch: branchName,
	})

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

	// Inject the finalize timestamp from the dispatcher's clock so it stays
	// consistent with started_at and tests can assert determinism.
	fin.FinishedAt = deps.Now().UnixMilli()

	if err := deps.DB.FinalizeImplementerRun(ctx, runID, fin); err != nil {
		// On finalize failure, surface pr_url in the log line if we managed to
		// open one — otherwise the URL is permanently lost (the row UPDATE
		// didn't land). Operators can recover by hand from the log.
		if fin.PRURL != "" {
			log.Printf("dispatchImplement: FinalizeImplementerRun %s failed with pr_url=%s — URL may be lost: %v",
				row.CommentID, fin.PRURL, err)
		} else {
			log.Printf("dispatchImplement: FinalizeImplementerRun %s: %v", row.CommentID, err)
		}
		// Non-fatal: continue so outbox and cleanup still execute.
	}

	// Step 8: Outbox row — phased intent marker only, NOT a payload store.
	// The outbox table has no content column; it's a (comment_id, phase) dedup
	// key so OutboxFlush knows a reply is owed. Task 6 composes the actual
	// Lark reply body at flush time by reading implementer_runs via
	// GetImplementerRunByCommentID.
	inserted, outboxErr := deps.DB.OutboxInsertPhased(ctx, row.CommentID, row.TaskID, row.CommentID, "implement")
	if outboxErr != nil {
		log.Printf("dispatchImplement: OutboxInsertPhased %s: %v", row.CommentID, outboxErr)
	} else if !inserted {
		// Existing row → idempotent re-run after crash recovery. Operators
		// looking at duplicate processing should see this signal in the logs.
		log.Printf("dispatchImplement: outbox marker already existed for %s (idempotent re-run)", row.CommentID)
	}

	// Step 9: Cleanup worktree.
	// Preserve when commits were made; remove otherwise (avoids unbounded disk growth).
	cleanupSuccess := result.CommitCount > 0
	if err := deps.Worktree.Cleanup(ctx, row.TaskID, cleanupSuccess); err != nil {
		log.Printf("dispatchImplement: Cleanup %s: %v", row.TaskID, err)
	}

	// Step 10: MarkInboxProcessed.
	if err := deps.DB.MarkInboxProcessed(ctx, row.CommentID); err != nil {
		log.Printf("dispatchImplement: MarkInboxProcessed %s: %v", row.CommentID, err)
	}

	return nil
}
