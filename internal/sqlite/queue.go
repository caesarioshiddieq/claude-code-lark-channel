package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/caesarioshiddieq/claude-code-lark-channel/internal/budget"
	"github.com/caesarioshiddieq/claude-code-lark-channel/internal/lark"
	_ "modernc.org/sqlite"
)

// compile-time check: *DB must implement budget.Queuer
var _ budget.Queuer = (*DB)(nil)

const createMigrations = `
CREATE TABLE IF NOT EXISTS schema_migrations (
  version    INTEGER PRIMARY KEY,
  applied_at INTEGER NOT NULL
);`

var migration0002 = []string{
	`ALTER TABLE inbox ADD COLUMN source TEXT NOT NULL DEFAULT 'human'`,
	`ALTER TABLE inbox ADD COLUMN scheduled_for INTEGER`,
	`ALTER TABLE inbox ADD COLUMN defer_count INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE inbox ADD COLUMN phase TEXT NOT NULL DEFAULT 'normal'`,
	`ALTER TABLE inbox ADD COLUMN original_content TEXT`,
	`ALTER TABLE outbox ADD COLUMN phase TEXT NOT NULL DEFAULT 'normal'`,
	`ALTER TABLE outbox ADD COLUMN comment_id TEXT NOT NULL DEFAULT ''`,
	`UPDATE outbox SET comment_id = COALESCE(reply_to_comment_id, '') WHERE comment_id = ''`,
	`CREATE TABLE IF NOT EXISTS turn_usage (
        spawn_id              INTEGER PRIMARY KEY,
        comment_id            TEXT    NOT NULL,
        task_id               TEXT    NOT NULL,
        session_uuid          TEXT    NOT NULL,
        phase                 TEXT    NOT NULL,
        input_tokens          INTEGER NOT NULL DEFAULT 0,
        output_tokens         INTEGER NOT NULL DEFAULT 0,
        cache_read_tokens     INTEGER NOT NULL DEFAULT 0,
        cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
        is_rate_limit_error   INTEGER NOT NULL DEFAULT 0,
        created_at            INTEGER NOT NULL
    )`,
	`CREATE INDEX IF NOT EXISTS turn_usage_created_at_idx ON turn_usage(created_at)`,
	`CREATE INDEX IF NOT EXISTS inbox_scheduled_idx ON inbox(scheduled_for) WHERE scheduled_for IS NOT NULL`,
}

var migration0003 = []string{
	`ALTER TABLE inbox ADD COLUMN compact_entered_at INTEGER`,
}

var migration0004 = []string{
	`CREATE TABLE IF NOT EXISTS implementer_runs (
		id                INTEGER PRIMARY KEY AUTOINCREMENT,
		inbox_comment_id  TEXT    NOT NULL,
		task_id           TEXT    NOT NULL,
		started_at        INTEGER NOT NULL,
		finished_at       INTEGER,
		outcome           TEXT    NOT NULL DEFAULT '',
		gnhf_iterations   INTEGER NOT NULL DEFAULT 0,
		gnhf_commits_made INTEGER NOT NULL DEFAULT 0,
		tokens_used       INTEGER NOT NULL DEFAULT 0,
		worktree_path     TEXT    NOT NULL DEFAULT '',
		branch_name       TEXT    NOT NULL DEFAULT '',
		pr_url            TEXT    NOT NULL DEFAULT '',
		notes_md_excerpt  TEXT    NOT NULL DEFAULT '',
		error             TEXT    NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX IF NOT EXISTS implementer_runs_comment_idx ON implementer_runs(inbox_comment_id)`,
}

func migrate(db *sql.DB) error {
	if _, err := db.Exec(createMigrations); err != nil {
		return fmt.Errorf("create migrations table: %w", err)
	}
	migrations := []struct {
		version int
		stmts   []string
	}{
		{2, migration0002},
		{3, migration0003},
		{4, migration0004},
	}
	for _, m := range migrations {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`,
			m.version).Scan(&count); err != nil {
			return fmt.Errorf("check migration %d: %w", m.version, err)
		}
		if count > 0 {
			continue
		}
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", m.version, err)
		}
		for _, stmt := range m.stmts {
			if _, err := tx.Exec(stmt); err != nil {
				preview := stmt
				if len(preview) > 40 {
					preview = preview[:40]
				}
				_ = tx.Rollback()
				return fmt.Errorf("migration %d stmt %q: %w", m.version, preview, err)
			}
		}
		if _, err := tx.Exec(
			`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
			m.version, time.Now().UnixMilli()); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %d: %w", m.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", m.version, err)
		}
	}
	return nil
}

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
  task_id TEXT PRIMARY KEY,
  session_uuid TEXT,
  created_at INTEGER NOT NULL,
  last_active INTEGER NOT NULL,
  turn_count INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS watermark (
  task_id TEXT PRIMARY KEY,
  last_seen_comment_id TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS inbox (
  comment_id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL,
  content TEXT NOT NULL,
  creator_id TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  processed_at INTEGER,
  attempts INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS outbox (
  content_hash TEXT PRIMARY KEY,
  task_id TEXT NOT NULL,
  reply_to_comment_id TEXT,
  lark_comment_id TEXT,
  created_at INTEGER NOT NULL,
  posted_at INTEGER
);
CREATE TABLE IF NOT EXISTS dlq (
  comment_id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL,
  last_error TEXT,
  moved_at INTEGER NOT NULL
);
`

type InboxRow struct {
	CommentID       string
	TaskID          string
	Content         string
	CreatorID       string
	Source          string // 'human' or 'autonomous'
	Phase           string // 'normal', 'compact', 'answer'
	OriginalContent string // empty string if NULL
}

type DB struct{ db *sql.DB }

// Open opens (or creates) a SQLite DB and applies the schema. Use ":memory:" for tests.
func Open(path string) (*DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("sqlite open: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set WAL mode: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite schema: %w", err)
	}
	d := &DB{db: db}
	if err := migrate(db); err != nil {
		d.Close()
		return nil, fmt.Errorf("schema migration: %w", err)
	}
	return d, nil
}

func (d *DB) Close() error { return d.db.Close() }

func (d *DB) InsertInbox(ctx context.Context, taskID string, c lark.Comment) error {
	_, err := d.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO inbox (comment_id, task_id, content, creator_id, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		c.CommentID, taskID, c.Content, c.Creator.ID, c.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert inbox: %w", err)
	}
	return nil
}

func (d *DB) NextInboxRow(ctx context.Context) (InboxRow, bool, error) {
	const q = `
		SELECT comment_id, task_id, content, creator_id,
		       COALESCE(source,'human'), COALESCE(phase,'normal'),
		       COALESCE(original_content,'')
		FROM inbox
		WHERE processed_at IS NULL
		  AND (scheduled_for IS NULL OR scheduled_for <= ?)
		ORDER BY
		  CASE phase
		    WHEN 'answer'  THEN 1
		    WHEN 'compact' THEN 2
		    ELSE               3
		  END,
		  created_at ASC
		LIMIT 1`
	var r InboxRow
	err := d.db.QueryRowContext(ctx, q, time.Now().UnixMilli()).Scan(
		&r.CommentID, &r.TaskID, &r.Content, &r.CreatorID,
		&r.Source, &r.Phase, &r.OriginalContent)
	if errors.Is(err, sql.ErrNoRows) {
		return InboxRow{}, false, nil
	}
	if err != nil {
		return InboxRow{}, false, fmt.Errorf("next inbox row: %w", err)
	}
	return r, true, nil
}

func (d *DB) NextInboxRowExcluding(ctx context.Context, busyTaskIDs []string) (InboxRow, bool, error) {
	if len(busyTaskIDs) == 0 {
		return d.NextInboxRow(ctx)
	}
	placeholders := make([]string, len(busyTaskIDs))
	args := make([]interface{}, len(busyTaskIDs)+1)
	args[0] = time.Now().UnixMilli()
	for i, id := range busyTaskIDs {
		placeholders[i] = "?"
		args[i+1] = id
	}
	q := `
		SELECT comment_id, task_id, content, creator_id,
		       COALESCE(source,'human'), COALESCE(phase,'normal'),
		       COALESCE(original_content,'')
		FROM inbox
		WHERE processed_at IS NULL
		  AND (scheduled_for IS NULL OR scheduled_for <= ?)
		  AND task_id NOT IN (` + strings.Join(placeholders, ",") + `)
		ORDER BY
		  CASE phase
		    WHEN 'answer'  THEN 1
		    WHEN 'compact' THEN 2
		    ELSE               3
		  END,
		  created_at ASC
		LIMIT 1`
	var r InboxRow
	err := d.db.QueryRowContext(ctx, q, args...).Scan(
		&r.CommentID, &r.TaskID, &r.Content, &r.CreatorID,
		&r.Source, &r.Phase, &r.OriginalContent)
	if errors.Is(err, sql.ErrNoRows) {
		return InboxRow{}, false, nil
	}
	if err != nil {
		return InboxRow{}, false, fmt.Errorf("next inbox row excluding: %w", err)
	}
	return r, true, nil
}

func (d *DB) MarkInboxProcessed(ctx context.Context, commentID string) error {
	_, err := d.db.ExecContext(ctx, `UPDATE inbox SET processed_at = ? WHERE comment_id = ?`,
		time.Now().UnixMilli(), commentID)
	if err != nil {
		return fmt.Errorf("mark inbox processed: %w", err)
	}
	return nil
}

// InsertHumanInbox records a human-originated Lark comment. source='human' is set explicitly.
func (d *DB) InsertHumanInbox(ctx context.Context, taskID string, c lark.Comment) error {
	const q = `INSERT OR IGNORE INTO inbox
		(comment_id, task_id, content, creator_id, created_at, source, phase)
		VALUES (?, ?, ?, ?, ?, 'human', 'normal')`
	_, err := d.db.ExecContext(ctx, q, c.CommentID, taskID, c.Content, c.Creator.ID, c.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert human inbox: %w", err)
	}
	return nil
}

// InsertAutonomousInbox records an autonomous task. source='autonomous' is set explicitly.
func (d *DB) InsertAutonomousInbox(ctx context.Context, taskID, commentID, content string) error {
	const q = `INSERT OR IGNORE INTO inbox
		(comment_id, task_id, content, creator_id, created_at, source, phase)
		VALUES (?, ?, ?, 'autonomous', ?, 'autonomous', 'normal')`
	_, err := d.db.ExecContext(ctx, q, commentID, taskID, content, time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("insert autonomous inbox: %w", err)
	}
	return nil
}

// UpdateInboxPhase transitions an inbox row's phase.
// original_content is only written when non-empty; a blank string preserves the existing value.
// compact_entered_at is set to now only when transitioning into 'compact'.
func (d *DB) UpdateInboxPhase(ctx context.Context, commentID, newPhase, originalContent string) error {
	var oc interface{}
	if originalContent != "" {
		oc = originalContent
	}
	now := time.Now().UnixMilli()
	_, err := d.db.ExecContext(ctx,
		`UPDATE inbox
		 SET phase              = ?,
		     original_content   = COALESCE(?, original_content),
		     compact_entered_at = CASE WHEN ? = 'compact' THEN ? ELSE compact_entered_at END
		 WHERE comment_id = ?`,
		newPhase, oc, newPhase, now, commentID)
	if err != nil {
		return fmt.Errorf("update inbox phase: %w", err)
	}
	return nil
}

// ResetInboxPhase resets a row to phase='normal' and clears original_content.
// Used when a compact spawn fails.
func (d *DB) ResetInboxPhase(ctx context.Context, commentID string) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE inbox SET phase = 'normal', original_content = NULL WHERE comment_id = ?`,
		commentID)
	if err != nil {
		return fmt.Errorf("reset inbox phase: %w", err)
	}
	return nil
}

func (d *DB) MarkDeferred(ctx context.Context, commentID string, scheduledFor int64, originalContent string) error {
	var oc interface{}
	if originalContent != "" {
		oc = originalContent
	}
	_, err := d.db.ExecContext(ctx,
		`UPDATE inbox SET scheduled_for = ?, original_content = ? WHERE comment_id = ?`,
		scheduledFor, oc, commentID)
	if err != nil {
		return fmt.Errorf("mark deferred: %w", err)
	}
	return nil
}

func (d *DB) MarkReadyNow(ctx context.Context, commentID string) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE inbox SET scheduled_for = NULL WHERE comment_id = ?`, commentID)
	if err != nil {
		return fmt.Errorf("mark ready now: %w", err)
	}
	return nil
}

func (d *DB) RescheduleDeferred(ctx context.Context, commentID string, scheduledFor int64) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE inbox SET scheduled_for = ? WHERE comment_id = ?`, scheduledFor, commentID)
	if err != nil {
		return fmt.Errorf("reschedule deferred: %w", err)
	}
	return nil
}

func (d *DB) BumpDeferCount(ctx context.Context, commentID string) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE inbox SET defer_count = defer_count + 1 WHERE comment_id = ?`, commentID)
	if err != nil {
		return fmt.Errorf("bump defer count: %w", err)
	}
	return nil
}

func (d *DB) ListStaleDeferrals(ctx context.Context) ([]budget.DeferredRow, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT comment_id, task_id, defer_count FROM inbox
		 WHERE scheduled_for IS NOT NULL AND scheduled_for < ? AND processed_at IS NULL`,
		time.Now().UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("list stale deferrals: %w", err)
	}
	defer rows.Close()
	var result []budget.DeferredRow
	for rows.Next() {
		var r budget.DeferredRow
		if err := rows.Scan(&r.CommentID, &r.TaskID, &r.DeferCount); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// MoveToDLQ writes a forensic record to the dlq table and marks the inbox row
// as processed so NextInboxRow skips it. Both writes are done in a transaction.
func (d *DB) MoveToDLQ(ctx context.Context, commentID, taskID, reason string) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("move to dlq begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO dlq (comment_id, task_id, last_error, moved_at) VALUES (?,?,?,?)`,
		commentID, taskID, reason, now); err != nil {
		return fmt.Errorf("move to dlq insert: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE inbox SET processed_at = ? WHERE comment_id = ?`,
		now, commentID); err != nil {
		return fmt.Errorf("move to dlq mark processed: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("move to dlq commit: %w", err)
	}
	return nil
}

// OutboxInsertPhased records an outbox entry keyed by (comment_id, phase).
// Returns (true, nil) when a new row was inserted, (false, nil) when the row
// already existed (INSERT OR IGNORE was a no-op), and (false, err) on error.
func (d *DB) OutboxInsertPhased(ctx context.Context, commentID, taskID, replyToCommentID, phase string) (bool, error) {
	hash := commentID + ":" + phase
	res, err := d.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO outbox
		 (content_hash, task_id, reply_to_comment_id, comment_id, phase, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		hash, taskID, replyToCommentID, commentID, phase, time.Now().UnixMilli())
	if err != nil {
		return false, fmt.Errorf("outbox insert phased: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// TurnUsage holds per-spawn telemetry inserted after every Claude invocation.
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
}

func (d *DB) InsertTurnUsage(ctx context.Context, u TurnUsage) error {
	rl := 0
	if u.IsRateLimitError {
		rl = 1
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO turn_usage
		 (comment_id, task_id, session_uuid, phase,
		  input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
		  is_rate_limit_error, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		u.CommentID, u.TaskID, u.SessionUUID, u.Phase,
		u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheCreationTokens,
		rl, time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("insert turn usage: %w", err)
	}
	return nil
}

func (d *DB) DeleteOldTurnUsage(ctx context.Context, olderThanMs int64) (int64, error) {
	res, err := d.db.ExecContext(ctx,
		`DELETE FROM turn_usage WHERE created_at < ?`, olderThanMs)
	if err != nil {
		return 0, fmt.Errorf("delete old turn usage: %w", err)
	}
	return res.RowsAffected()
}

func (d *DB) ListStuckCompactRows(ctx context.Context, olderThanMs int64) ([]string, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT comment_id FROM inbox
		 WHERE phase = 'compact'
		   AND processed_at IS NULL
		   AND compact_entered_at IS NOT NULL
		   AND compact_entered_at < ?`,
		olderThanMs)
	if err != nil {
		return nil, fmt.Errorf("list stuck compact rows: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ResetTurnCount sets sessions.turn_count = 0 after a successful answer phase.
func (d *DB) ResetTurnCount(ctx context.Context, taskID string) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE sessions SET turn_count = 0 WHERE task_id = ?`, taskID)
	if err != nil {
		return fmt.Errorf("reset turn count: %w", err)
	}
	return nil
}

func (d *DB) MoveToDeadLetter(ctx context.Context, commentID, taskID, lastError string) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO dlq (comment_id, task_id, last_error, moved_at) VALUES (?,?,?,?)`,
		commentID, taskID, lastError, time.Now().UnixMilli()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM inbox WHERE comment_id = ?`, commentID); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) GetSession(ctx context.Context, taskID string) (string, bool, error) {
	var uuid sql.NullString
	err := d.db.QueryRowContext(ctx, `SELECT session_uuid FROM sessions WHERE task_id = ?`, taskID).Scan(&uuid)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil || !uuid.Valid {
		return "", false, err
	}
	return uuid.String, true, nil
}

func (d *DB) UpsertSession(ctx context.Context, taskID, sessionUUID string) error {
	now := time.Now().UnixMilli()
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO sessions (task_id, session_uuid, created_at, last_active, turn_count)
		 VALUES (?, ?, ?, ?, 0)
		 ON CONFLICT(task_id) DO UPDATE SET session_uuid=excluded.session_uuid, last_active=excluded.last_active`,
		taskID, sessionUUID, now, now)
	return err
}

func (d *DB) IncrTurnCount(ctx context.Context, taskID string) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE sessions SET turn_count=turn_count+1, last_active=? WHERE task_id=?`,
		time.Now().UnixMilli(), taskID)
	if err != nil {
		return fmt.Errorf("incr turn count: %w", err)
	}
	return nil
}

func (d *DB) GetTurnCount(ctx context.Context, taskID string) (int, error) {
	var n int
	err := d.db.QueryRowContext(ctx, `SELECT turn_count FROM sessions WHERE task_id=?`, taskID).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return n, err
}

func (d *DB) GetWatermark(ctx context.Context, taskID string) (string, bool, error) {
	var id string
	err := d.db.QueryRowContext(ctx, `SELECT last_seen_comment_id FROM watermark WHERE task_id=?`, taskID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return id, true, nil
}

func (d *DB) SetWatermark(ctx context.Context, taskID, commentID string) error {
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO watermark (task_id, last_seen_comment_id) VALUES (?,?)
		 ON CONFLICT(task_id) DO UPDATE SET last_seen_comment_id=excluded.last_seen_comment_id`,
		taskID, commentID)
	return err
}

func (d *DB) OutboxCheck(ctx context.Context, hash string) (string, bool, error) {
	var larkID sql.NullString
	err := d.db.QueryRowContext(ctx, `SELECT lark_comment_id FROM outbox WHERE content_hash=?`, hash).Scan(&larkID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return larkID.String, true, nil
}

func (d *DB) OutboxInsert(ctx context.Context, hash, taskID, replyToCommentID string) error {
	_, err := d.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO outbox (content_hash, task_id, reply_to_comment_id, created_at)
		 VALUES (?,?,?,?)`,
		hash, taskID, replyToCommentID, time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("outbox insert: %w", err)
	}
	return nil
}

func (d *DB) OutboxMarkPosted(ctx context.Context, hash, larkCommentID string) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE outbox SET lark_comment_id=?, posted_at=? WHERE content_hash=?`,
		larkCommentID, time.Now().UnixMilli(), hash)
	if err != nil {
		return fmt.Errorf("outbox mark posted: %w", err)
	}
	return nil
}
