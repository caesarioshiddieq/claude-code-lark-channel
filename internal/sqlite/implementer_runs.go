package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ImplementerRun represents a single autonomous-implementer execution against
// a Lark task comment. One row is inserted at spawn time with started_at set
// and outcome empty; on subprocess exit, FinalizeImplementerRun finalizes the
// row with all gnhf-derived stats plus the legacy outcome string.
//
// FinishedAt is *int64 because the column is NULL until finalization. The 9
// gnhf_* fields (added by migration0005) hold the native gnhf run telemetry;
// they are written by FinalizeImplementerRun and read by Task 6's Lark
// formatter via GetImplementerRunByCommentID.
type ImplementerRun struct {
	ID              int64
	InboxCommentID  string
	TaskID          string
	StartedAt       int64
	FinishedAt      *int64
	Outcome         string
	GnhfIterations  int
	GnhfCommitsMade int
	TokensUsed      int
	WorktreePath    string
	BranchName      string
	PRURL           string
	NotesMDExcerpt  string
	Error           string

	// gnhf_* native columns (migration0005). Populated by
	// FinalizeImplementerRun; read back via GetImplementerRunByCommentID.
	GnhfStatus       string
	GnhfReason       string
	GnhfSuccessCount int
	GnhfFailCount    int
	GnhfInputTokens  int
	GnhfOutputTokens int
	GnhfRunID        string
	GnhfNoProgress   bool
	GnhfLastMessage  string
}

// ImplementerRunOutcome carries the post-spawn stats written by
// UpdateImplementerRunOutcome. Error is only populated when Outcome is
// "failed" or "timeout".
type ImplementerRunOutcome struct {
	FinishedAt      int64
	Outcome         string
	GnhfIterations  int
	GnhfCommitsMade int
	TokensUsed      int
	PRURL           string
	NotesMDExcerpt  string
	Error           string
}

func (d *DB) InsertImplementerRun(ctx context.Context, r ImplementerRun) (int64, error) {
	const q = `INSERT INTO implementer_runs
		(inbox_comment_id, task_id, started_at, worktree_path, branch_name)
		VALUES (?, ?, ?, ?, ?)`
	res, err := d.db.ExecContext(ctx, q,
		r.InboxCommentID, r.TaskID, r.StartedAt, r.WorktreePath, r.BranchName)
	if err != nil {
		return 0, fmt.Errorf("insert implementer_run: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("implementer_run last insert id: %w", err)
	}
	return id, nil
}

// ImplementerRunFinalize carries the full post-spawn data written by
// FinalizeImplementerRun. It includes all nine gnhf_* native columns from
// migration0005, the derived legacy outcome/tokens_used, pr_url (set later if
// gh pr create succeeds), notes_md_excerpt, and an error string for
// failed/timeout runs.
//
// FinishedAt is caller-injected (unix ms) so tests can use a synthetic clock
// and so it stays consistent with StartedAt, which is also caller-controlled
// via deps.Now in the dispatcher.
type ImplementerRunFinalize struct {
	// Derived legacy columns (computed by dispatcher from GnhfResult).
	FinishedAt     int64
	Outcome        string
	TokensUsed     int
	PRURL          string
	NotesMDExcerpt string
	Error          string

	// gnhf_* native columns (migration0005).
	GnhfStatus       string
	GnhfReason       string
	GnhfIterations   int
	GnhfCommitsMade  int
	GnhfSuccessCount int
	GnhfFailCount    int
	GnhfInputTokens  int
	GnhfOutputTokens int
	GnhfRunID        string
	GnhfNoProgress   bool
	GnhfLastMessage  string
}

// FinalizeImplementerRun writes all gnhf_* native columns, the derived legacy
// outcome/tokens_used, pr_url, notes_md_excerpt, error, and finished_at in a
// single UPDATE. Use this instead of UpdateImplementerRunOutcome for Task 5+.
func (d *DB) FinalizeImplementerRun(ctx context.Context, id int64, f ImplementerRunFinalize) error {
	noProgress := 0
	if f.GnhfNoProgress {
		noProgress = 1
	}
	const query = `UPDATE implementer_runs SET
		finished_at        = ?,
		outcome            = ?,
		gnhf_status        = ?,
		gnhf_reason        = ?,
		gnhf_iterations    = ?,
		gnhf_commits_made  = ?,
		gnhf_success_count = ?,
		gnhf_fail_count    = ?,
		gnhf_input_tokens  = ?,
		gnhf_output_tokens = ?,
		gnhf_run_id        = ?,
		gnhf_no_progress   = ?,
		gnhf_last_message  = ?,
		tokens_used        = ?,
		pr_url             = ?,
		notes_md_excerpt   = ?,
		error              = ?
		WHERE id = ?`
	_, err := d.db.ExecContext(ctx, query,
		f.FinishedAt,
		f.Outcome,
		f.GnhfStatus,
		f.GnhfReason,
		f.GnhfIterations,
		f.GnhfCommitsMade,
		f.GnhfSuccessCount,
		f.GnhfFailCount,
		f.GnhfInputTokens,
		f.GnhfOutputTokens,
		f.GnhfRunID,
		noProgress,
		f.GnhfLastMessage,
		f.TokensUsed,
		f.PRURL,
		f.NotesMDExcerpt,
		f.Error,
		id,
	)
	if err != nil {
		return fmt.Errorf("finalize implementer_run: %w", err)
	}
	return nil
}

// Deprecated: use FinalizeImplementerRun. UpdateImplementerRunOutcome is kept
// for backwards compatibility with Task 1 tests; it writes only the legacy
// columns and does not set the gnhf_* native fields added in migration0005.
func (d *DB) UpdateImplementerRunOutcome(ctx context.Context, id int64, o ImplementerRunOutcome) error {
	const q = `UPDATE implementer_runs SET
		finished_at       = ?,
		outcome           = ?,
		gnhf_iterations   = ?,
		gnhf_commits_made = ?,
		tokens_used       = ?,
		pr_url            = ?,
		notes_md_excerpt  = ?,
		error             = ?
		WHERE id = ?`
	_, err := d.db.ExecContext(ctx, q,
		o.FinishedAt, o.Outcome, o.GnhfIterations, o.GnhfCommitsMade,
		o.TokensUsed, o.PRURL, o.NotesMDExcerpt, o.Error, id)
	if err != nil {
		return fmt.Errorf("update implementer_run outcome: %w", err)
	}
	return nil
}

// GetImplementerRunByCommentID returns the most recent implementer_runs row
// for a given inbox comment_id. Returns (zero-value, false, nil) when no row
// exists. Multiple rows per comment_id are possible (e.g. retry); the highest
// id wins.
func (d *DB) GetImplementerRunByCommentID(ctx context.Context, commentID string) (ImplementerRun, bool, error) {
	const q = `SELECT id, inbox_comment_id, task_id, started_at, finished_at,
		outcome, gnhf_iterations, gnhf_commits_made, tokens_used,
		worktree_path, branch_name, pr_url, notes_md_excerpt, error,
		gnhf_status, gnhf_reason, gnhf_success_count, gnhf_fail_count,
		gnhf_input_tokens, gnhf_output_tokens, gnhf_run_id,
		gnhf_no_progress, gnhf_last_message
		FROM implementer_runs
		WHERE inbox_comment_id = ?
		ORDER BY id DESC
		LIMIT 1`
	var r ImplementerRun
	var finishedAt sql.NullInt64
	var noProgress int
	err := d.db.QueryRowContext(ctx, q, commentID).Scan(
		&r.ID, &r.InboxCommentID, &r.TaskID, &r.StartedAt, &finishedAt,
		&r.Outcome, &r.GnhfIterations, &r.GnhfCommitsMade, &r.TokensUsed,
		&r.WorktreePath, &r.BranchName, &r.PRURL,
		&r.NotesMDExcerpt, &r.Error,
		&r.GnhfStatus, &r.GnhfReason, &r.GnhfSuccessCount, &r.GnhfFailCount,
		&r.GnhfInputTokens, &r.GnhfOutputTokens, &r.GnhfRunID,
		&noProgress, &r.GnhfLastMessage,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ImplementerRun{}, false, nil
	}
	if err != nil {
		return ImplementerRun{}, false, fmt.Errorf("get implementer_run by comment_id: %w", err)
	}
	if finishedAt.Valid {
		v := finishedAt.Int64
		r.FinishedAt = &v
	}
	r.GnhfNoProgress = noProgress != 0
	return r, true, nil
}
