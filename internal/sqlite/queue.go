package sqlite

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/caesarioshiddieq/claude-code-lark-channel/internal/lark"
	_ "modernc.org/sqlite"
)

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
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite schema: %w", err)
	}
	return &DB{db: db}, nil
}

func (d *DB) Close() error { return d.db.Close() }

func (d *DB) InsertInbox(taskID string, c lark.Comment) error {
	_, err := d.db.Exec(
		`INSERT OR IGNORE INTO inbox (comment_id, task_id, content, creator_id, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		c.CommentID, taskID, c.Content, c.Creator.ID, c.CreatedAt,
	)
	return err
}

func (d *DB) NextInboxRow() (InboxRow, bool, error) {
	var row InboxRow
	err := d.db.QueryRow(
		`SELECT comment_id, task_id, content, creator_id FROM inbox
		 WHERE processed_at IS NULL ORDER BY created_at ASC LIMIT 1`,
	).Scan(&row.CommentID, &row.TaskID, &row.Content, &row.CreatorID)
	if err == sql.ErrNoRows {
		return InboxRow{}, false, nil
	}
	return row, err == nil, err
}

func (d *DB) MarkInboxProcessed(commentID string) error {
	_, err := d.db.Exec(`UPDATE inbox SET processed_at = ? WHERE comment_id = ?`,
		time.Now().Unix(), commentID)
	return err
}

func (d *DB) MoveToDeadLetter(commentID, taskID, lastError string) error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		`INSERT OR REPLACE INTO dlq (comment_id, task_id, last_error, moved_at) VALUES (?,?,?,?)`,
		commentID, taskID, lastError, time.Now().Unix()); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM inbox WHERE comment_id = ?`, commentID); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) GetSession(taskID string) (string, bool, error) {
	var uuid sql.NullString
	err := d.db.QueryRow(`SELECT session_uuid FROM sessions WHERE task_id = ?`, taskID).Scan(&uuid)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil || !uuid.Valid {
		return "", false, err
	}
	return uuid.String, true, nil
}

func (d *DB) UpsertSession(taskID, sessionUUID string) error {
	now := time.Now().Unix()
	_, err := d.db.Exec(
		`INSERT INTO sessions (task_id, session_uuid, created_at, last_active, turn_count)
		 VALUES (?, ?, ?, ?, 0)
		 ON CONFLICT(task_id) DO UPDATE SET session_uuid=excluded.session_uuid, last_active=excluded.last_active`,
		taskID, sessionUUID, now, now)
	return err
}

func (d *DB) IncrTurnCount(taskID string) error {
	_, err := d.db.Exec(
		`UPDATE sessions SET turn_count=turn_count+1, last_active=? WHERE task_id=?`,
		time.Now().Unix(), taskID)
	return err
}

func (d *DB) GetTurnCount(taskID string) (int, error) {
	var n int
	err := d.db.QueryRow(`SELECT turn_count FROM sessions WHERE task_id=?`, taskID).Scan(&n)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return n, err
}

func (d *DB) GetWatermark(taskID string) (string, bool, error) {
	var id string
	err := d.db.QueryRow(`SELECT last_seen_comment_id FROM watermark WHERE task_id=?`, taskID).Scan(&id)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	return id, err == nil, err
}

func (d *DB) SetWatermark(taskID, commentID string) error {
	_, err := d.db.Exec(
		`INSERT INTO watermark (task_id, last_seen_comment_id) VALUES (?,?)
		 ON CONFLICT(task_id) DO UPDATE SET last_seen_comment_id=excluded.last_seen_comment_id`,
		taskID, commentID)
	return err
}

func (d *DB) OutboxCheck(hash string) (string, bool, error) {
	var larkID sql.NullString
	err := d.db.QueryRow(`SELECT lark_comment_id FROM outbox WHERE content_hash=?`, hash).Scan(&larkID)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return larkID.String, true, nil
}

func (d *DB) OutboxInsert(hash, taskID, replyToCommentID string) error {
	_, err := d.db.Exec(
		`INSERT OR IGNORE INTO outbox (content_hash, task_id, reply_to_comment_id, created_at)
		 VALUES (?,?,?,?)`,
		hash, taskID, replyToCommentID, time.Now().Unix())
	return err
}

func (d *DB) OutboxMarkPosted(hash, larkCommentID string) error {
	_, err := d.db.Exec(
		`UPDATE outbox SET lark_comment_id=?, posted_at=? WHERE content_hash=?`,
		larkCommentID, time.Now().Unix(), hash)
	return err
}
