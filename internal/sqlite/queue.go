package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/caesarioshiddieq/claude-code-lark-channel/internal/lark"
	_ "modernc.org/sqlite"
)

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
	`CREATE UNIQUE INDEX IF NOT EXISTS outbox_event_uniq ON outbox (comment_id, phase)`,
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

func migrate(db *sql.DB) error {
	if _, err := db.Exec(createMigrations); err != nil {
		return fmt.Errorf("create migrations table: %w", err)
	}
	migrations := []struct {
		version int
		stmts   []string
	}{
		{2, migration0002},
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
	CommentID string
	TaskID    string
	Content   string
	CreatorID string
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
	var row InboxRow
	err := d.db.QueryRowContext(ctx,
		`SELECT comment_id, task_id, content, creator_id FROM inbox
		 WHERE processed_at IS NULL ORDER BY created_at ASC LIMIT 1`,
	).Scan(&row.CommentID, &row.TaskID, &row.Content, &row.CreatorID)
	if errors.Is(err, sql.ErrNoRows) {
		return InboxRow{}, false, nil
	}
	if err != nil {
		return InboxRow{}, false, err
	}
	return row, true, nil
}

func (d *DB) MarkInboxProcessed(ctx context.Context, commentID string) error {
	_, err := d.db.ExecContext(ctx, `UPDATE inbox SET processed_at = ? WHERE comment_id = ?`,
		time.Now().Unix(), commentID)
	if err != nil {
		return fmt.Errorf("mark inbox processed: %w", err)
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
		commentID, taskID, lastError, time.Now().Unix()); err != nil {
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
	now := time.Now().Unix()
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
		time.Now().Unix(), taskID)
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
		hash, taskID, replyToCommentID, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("outbox insert: %w", err)
	}
	return nil
}

func (d *DB) OutboxMarkPosted(ctx context.Context, hash, larkCommentID string) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE outbox SET lark_comment_id=?, posted_at=? WHERE content_hash=?`,
		larkCommentID, time.Now().Unix(), hash)
	if err != nil {
		return fmt.Errorf("outbox mark posted: %w", err)
	}
	return nil
}
