package sqlite_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/caesarioshiddieq/claude-code-lark-channel/internal/lark"
	q "github.com/caesarioshiddieq/claude-code-lark-channel/internal/sqlite"
	_ "modernc.org/sqlite"
)

func openTestDB(t *testing.T) *q.DB {
	t.Helper()
	db, err := q.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestInbox_InsertAndFetch(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	c := lark.Comment{CommentID: "c1", Content: "hello", CreatedAt: time.Now().UnixMilli(),
		Creator: lark.Creator{ID: "u1", Type: "user"}}

	if err := db.InsertInbox(ctx, "task-1", c); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertInbox(ctx, "task-1", c); err != nil {
		t.Fatalf("duplicate insert should be ignored, got: %v", err)
	}

	row, found, err := db.NextInboxRow(ctx)
	if err != nil || !found {
		t.Fatalf("expected row: err=%v found=%v", err, found)
	}
	if row.CommentID != "c1" || row.TaskID != "task-1" {
		t.Fatalf("unexpected row: %+v", row)
	}
}

func TestWatermark_SetAndGet(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	_, found, err := db.GetWatermark(ctx, "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("expected no watermark initially")
	}
	if err := db.SetWatermark(ctx, "task-1", "c42"); err != nil {
		t.Fatal(err)
	}
	wm, found, err := db.GetWatermark(ctx, "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if !found || wm != "c42" {
		t.Fatalf("want c42, got %s found=%v", wm, found)
	}
}

func TestSession_UpsertAndGet(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	_, found, err := db.GetSession(ctx, "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("expected no session initially")
	}
	if err := db.UpsertSession(ctx, "task-1", "uuid-abc"); err != nil {
		t.Fatal(err)
	}
	uuid, found, err := db.GetSession(ctx, "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if !found || uuid != "uuid-abc" {
		t.Fatalf("want uuid-abc, got %s found=%v", uuid, found)
	}
}

func TestOutbox_InsertCheckMarkPosted(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	_, found, err := db.OutboxCheck(ctx, "hash1")
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("expected no outbox row initially")
	}
	if err := db.OutboxInsert(ctx, "hash1", "task-1", "c1"); err != nil {
		t.Fatal(err)
	}
	larkID, found, err := db.OutboxCheck(ctx, "hash1")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("expected outbox row after insert")
	}
	if larkID != "" {
		t.Fatalf("expected null lark_comment_id, got %s", larkID)
	}
	if err := db.OutboxMarkPosted(ctx, "hash1", "new-c99"); err != nil {
		t.Fatal(err)
	}
	larkID, found, err = db.OutboxCheck(ctx, "hash1")
	if err != nil {
		t.Fatal(err)
	}
	if !found || larkID != "new-c99" {
		t.Fatalf("want new-c99, got %s found=%v", larkID, found)
	}
}

func TestMigration0002_OutboxBackfill(t *testing.T) {
	db := openTestDB(t)

	// Insert an outbox row with a non-empty reply_to_comment_id to verify the
	// unique index (comment_id, phase) works correctly after the COALESCE backfill.
	_, err := db.RawDB().Exec(
		`INSERT INTO outbox (content_hash, task_id, reply_to_comment_id, created_at, phase, comment_id)
		 VALUES ('hash1', 'task-1', 'cmt-abc', 1, 'normal', 'cmt-abc')`)
	if err != nil {
		t.Fatalf("insert outbox row: %v", err)
	}

	// A second row with the same (comment_id, phase) must violate the unique index.
	_, err = db.RawDB().Exec(
		`INSERT INTO outbox (content_hash, task_id, reply_to_comment_id, created_at, phase, comment_id)
		 VALUES ('hash2', 'task-1', 'cmt-abc', 2, 'normal', 'cmt-abc')`)
	if err == nil {
		t.Error("expected UNIQUE constraint violation on (comment_id, phase), got nil")
	}

	// A row with NULL reply_to_comment_id must get comment_id = '' (COALESCE result)
	// after backfill; verify we can insert it without NOT NULL violation.
	_, err = db.RawDB().Exec(
		`INSERT INTO outbox (content_hash, task_id, reply_to_comment_id, created_at, phase, comment_id)
		 VALUES ('hash3', 'task-1', NULL, 3, 'normal', '')`)
	if err != nil {
		t.Fatalf("insert outbox row with NULL reply_to_comment_id: %v", err)
	}

	// Verify the row's comment_id is empty string (what COALESCE(NULL, '') would produce).
	var commentID string
	if err := db.RawDB().QueryRow(
		`SELECT comment_id FROM outbox WHERE content_hash = 'hash3'`,
	).Scan(&commentID); err != nil {
		t.Fatalf("query hash3: %v", err)
	}
	if commentID != "" {
		t.Errorf("expected comment_id='', got %q", commentID)
	}
}

func TestMigration0002_NewColumns(t *testing.T) {
	db := openTestDB(t)

	for _, col := range []string{"source", "scheduled_for", "defer_count", "phase", "original_content"} {
		var name string
		if err := db.RawDB().QueryRow(
			`SELECT name FROM pragma_table_info('inbox') WHERE name = ?`, col,
		).Scan(&name); err != nil {
			t.Errorf("inbox column %q missing: %v", col, err)
		}
	}
	for _, col := range []string{"phase", "comment_id"} {
		var name string
		if err := db.RawDB().QueryRow(
			`SELECT name FROM pragma_table_info('outbox') WHERE name = ?`, col,
		).Scan(&name); err != nil {
			t.Errorf("outbox column %q missing: %v", col, err)
		}
	}
	var tableName string
	if err := db.RawDB().QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='turn_usage'`,
	).Scan(&tableName); err != nil {
		t.Errorf("turn_usage table missing: %v", err)
	}

	// Idempotency: re-open same DB must not error
	path := filepath.Join(t.TempDir(), "rerun.db")
	db1, err := q.Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	db1.Close()
	db2, err := q.Open(path)
	if err != nil {
		t.Fatalf("second Open (idempotency): %v", err)
	}
	db2.Close()
}

func TestMigration0002_BackfillUpdatesExistingRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backfill_real.db")

	// Open raw SQLite without our Open() wrapper (no migration yet)
	rawDB, err := sql.Open("sqlite", "file:"+path+"?_foreign_keys=on")
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	defer rawDB.Close()

	// Apply v1 schema only (no migration)
	if _, err := rawDB.Exec(q.SchemaForTest); err != nil {
		t.Fatalf("apply v1 schema: %v", err)
	}

	// Insert pre-migration outbox rows (no comment_id column yet in v1)
	_, err = rawDB.Exec(`INSERT INTO outbox (content_hash, task_id, reply_to_comment_id, created_at)
		VALUES ('hash-a', 'task-1', 'cmt-xyz', 1)`)
	if err != nil {
		t.Fatalf("insert row with reply_to_comment_id: %v", err)
	}
	_, err = rawDB.Exec(`INSERT INTO outbox (content_hash, task_id, reply_to_comment_id, created_at)
		VALUES ('hash-b', 'task-1', NULL, 2)`)
	if err != nil {
		t.Fatalf("insert row with NULL reply_to_comment_id: %v", err)
	}

	// Run migration
	if err := q.MigrateForTest(rawDB); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Verify backfill: reply_to_comment_id='cmt-xyz' → comment_id='cmt-xyz'
	var commentID string
	if err := rawDB.QueryRow(`SELECT comment_id FROM outbox WHERE reply_to_comment_id = 'cmt-xyz'`).Scan(&commentID); err != nil {
		t.Fatalf("query after migration: %v", err)
	}
	if commentID != "cmt-xyz" {
		t.Errorf("expected comment_id='cmt-xyz', got %q", commentID)
	}

	// Verify NULL case: reply_to_comment_id=NULL → comment_id=''
	var commentIDNull string
	if err := rawDB.QueryRow(`SELECT comment_id FROM outbox WHERE content_hash = 'hash-b'`).Scan(&commentIDNull); err != nil {
		t.Fatalf("query null row after migration: %v", err)
	}
	if commentIDNull != "" {
		t.Errorf("expected comment_id='' for NULL reply_to_comment_id, got %q", commentIDNull)
	}
}
