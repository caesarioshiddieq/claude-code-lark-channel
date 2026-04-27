package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ImplementerRun represents a single autonomous-implementer execution against
// a Lark task comment. One row is inserted at spawn time with started_at set
// and outcome empty; on subprocess exit, UpdateImplementerRunOutcome
// finalizes the row with the gnhf-derived stats and the final outcome string.
//
// FinishedAt is *int64 because the column is NULL until the outcome is
// recorded.
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
		worktree_path, branch_name, pr_url, notes_md_excerpt, error
		FROM implementer_runs
		WHERE inbox_comment_id = ?
		ORDER BY id DESC
		LIMIT 1`
	var r ImplementerRun
	var finishedAt sql.NullInt64
	err := d.db.QueryRowContext(ctx, q, commentID).Scan(
		&r.ID, &r.InboxCommentID, &r.TaskID, &r.StartedAt, &finishedAt,
		&r.Outcome, &r.GnhfIterations, &r.GnhfCommitsMade, &r.TokensUsed,
		&r.WorktreePath, &r.BranchName, &r.PRURL,
		&r.NotesMDExcerpt, &r.Error,
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
	return r, true, nil
}
