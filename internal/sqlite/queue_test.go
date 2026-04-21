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

	// content_hash is the PRIMARY KEY — inserting the same hash twice must fail.
	_, err := db.RawDB().Exec(
		`INSERT INTO outbox (content_hash, task_id, reply_to_comment_id, created_at, phase, comment_id)
		 VALUES ('hash1', 'task-1', 'cmt-abc', 1, 'normal', 'cmt-abc')`)
	if err != nil {
		t.Fatalf("insert outbox row: %v", err)
	}
	_, err = db.RawDB().Exec(
		`INSERT INTO outbox (content_hash, task_id, reply_to_comment_id, created_at, phase, comment_id)
		 VALUES ('hash1', 'task-1', 'cmt-abc', 2, 'normal', 'cmt-abc')`)
	if err == nil {
		t.Error("expected PRIMARY KEY violation on duplicate content_hash, got nil")
	}

	// Two rows with the same comment_id are allowed — no unique index on (comment_id, phase).
	// OutboxInsertPhased deduplicates via the content_hash PK, not a secondary index.
	_, err = db.RawDB().Exec(
		`INSERT INTO outbox (content_hash, task_id, reply_to_comment_id, created_at, phase, comment_id)
		 VALUES ('hash2', 'task-1', 'cmt-abc', 2, 'normal', 'cmt-abc')`)
	if err != nil {
		t.Errorf("two rows sharing comment_id should be allowed (no unique index): %v", err)
	}

	// A row with NULL reply_to_comment_id can use comment_id='' without NOT NULL violation.
	_, err = db.RawDB().Exec(
		`INSERT INTO outbox (content_hash, task_id, reply_to_comment_id, created_at, phase, comment_id)
		 VALUES ('hash3', 'task-1', NULL, 3, 'normal', '')`)
	if err != nil {
		t.Fatalf("insert outbox row with NULL reply_to_comment_id: %v", err)
	}
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

func TestMigration0003_NewColumns(t *testing.T) {
	db := openTestDB(t)
	var name string
	if err := db.RawDB().QueryRow(
		`SELECT name FROM pragma_table_info('inbox') WHERE name = 'compact_entered_at'`,
	).Scan(&name); err != nil {
		t.Errorf("inbox column compact_entered_at missing after migration 0003: %v", err)
	}
}

func TestUpdateInboxPhase_SetsCompactEnteredAt(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	before := time.Now()
	db.RawDB().Exec(`INSERT INTO inbox (comment_id,task_id,content,creator_id,created_at,source,phase)
		VALUES ('C1','T1','hello','U1',1,'human','normal')`)

	if err := db.UpdateInboxPhase(ctx, "C1", "compact", "hello"); err != nil {
		t.Fatal(err)
	}
	var ceat *int64
	db.RawDB().QueryRow(`SELECT compact_entered_at FROM inbox WHERE comment_id='C1'`).Scan(&ceat)
	if ceat == nil {
		t.Fatal("compact_entered_at must be set when transitioning to compact")
	}
	if *ceat < before.UnixMilli() {
		t.Errorf("compact_entered_at %d is before test start %d", *ceat, before.UnixMilli())
	}

	// Transitioning to 'answer' must NOT overwrite compact_entered_at.
	prevCeat := *ceat
	if err := db.UpdateInboxPhase(ctx, "C1", "answer", "hello"); err != nil {
		t.Fatal(err)
	}
	db.RawDB().QueryRow(`SELECT compact_entered_at FROM inbox WHERE comment_id='C1'`).Scan(&ceat)
	if ceat == nil || *ceat != prevCeat {
		t.Error("compact_entered_at must be preserved on non-compact phase transitions")
	}
}

func TestListStuckCompactRows_UsesCompactEnteredAt(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()
	twoHoursAgoMs := now - 2*60*60*1000
	oneHourAgoMs := now - 60*60*1000

	// Stuck: compact_entered_at is 2 hours ago (older than 1h threshold).
	db.RawDB().Exec(`INSERT INTO inbox (comment_id,task_id,content,creator_id,created_at,source,phase,compact_entered_at)
		VALUES ('C1','T1','hi','U1',?,  'human','compact',?)`, now, twoHoursAgoMs)
	// Pre-migration row: compact_entered_at IS NULL — must be invisible to watchdog.
	db.RawDB().Exec(`INSERT INTO inbox (comment_id,task_id,content,creator_id,created_at,source,phase)
		VALUES ('C2','T1','hi','U1',?)`, twoHoursAgoMs)
	db.RawDB().Exec(`UPDATE inbox SET phase='compact' WHERE comment_id='C2'`)
	// Recent: compact_entered_at just set — within threshold, must not alert.
	db.RawDB().Exec(`INSERT INTO inbox (comment_id,task_id,content,creator_id,created_at,source,phase,compact_entered_at)
		VALUES ('C3','T1','hi','U1',?, 'human','compact',?)`, now, now)

	ids, err := db.ListStuckCompactRows(ctx, oneHourAgoMs)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "C1" {
		t.Errorf("want [C1], got %v", ids)
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

func TestInsertHumanInbox_SetsSource(t *testing.T) {
	db := openTestDB(t)
	c := lark.Comment{CommentID: "C1", Content: "hello", Creator: lark.Creator{ID: "U1"}, CreatedAt: 1}
	if err := db.InsertHumanInbox(context.Background(), "T1", c); err != nil {
		t.Fatalf("InsertHumanInbox: %v", err)
	}
	var src, phase string
	db.RawDB().QueryRow(`SELECT source, phase FROM inbox WHERE comment_id='C1'`).Scan(&src, &phase)
	if src != "human" {
		t.Errorf("source=%q, want human", src)
	}
	if phase != "normal" {
		t.Errorf("phase=%q, want normal", phase)
	}
}

func TestNextInboxRow_AnswerBeforeNormal(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	db.RawDB().Exec(`INSERT INTO inbox (comment_id,task_id,content,creator_id,created_at,source,phase)
		VALUES ('C1','T1','hello','U1',1,'human','normal')`)
	db.RawDB().Exec(`INSERT INTO inbox (comment_id,task_id,content,creator_id,created_at,source,phase)
		VALUES ('C2','T1','world','U1',2,'human','answer')`)
	row, ok, err := db.NextInboxRow(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected a row")
	}
	if row.CommentID != "C2" {
		t.Errorf("want answer row C2, got %s (phase=%s)", row.CommentID, row.Phase)
	}
}

func TestNextInboxRow_SkipsScheduledFuture(t *testing.T) {
	db := openTestDB(t)
	future := time.Now().Add(24 * time.Hour).UnixMilli()
	db.RawDB().Exec(`INSERT INTO inbox (comment_id,task_id,content,creator_id,created_at,source,phase,scheduled_for)
		VALUES ('C3','T1','hi','U1',1,'autonomous','normal',?)`, future)
	_, ok, err := db.NextInboxRow(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("should not return future-scheduled row")
	}
}

func TestInsertTurnUsage_And_Delete(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	u := q.TurnUsage{CommentID: "C1", TaskID: "T1", SessionUUID: "S1", Phase: "normal", InputTokens: 100}
	if err := db.InsertTurnUsage(ctx, u); err != nil {
		t.Fatal(err)
	}
	var count int
	db.RawDB().QueryRow(`SELECT COUNT(*) FROM turn_usage WHERE comment_id='C1'`).Scan(&count)
	if count != 1 {
		t.Errorf("expected 1 row, got %d", count)
	}

	// Old row should be deleted (created just now, cutoff = now+1s means it's "old")
	n, err := db.DeleteOldTurnUsage(ctx, time.Now().Add(time.Second).UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("expected 1 deleted, got %d", n)
	}
}

func TestOutboxInsertPhased_Idempotency(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	// Insert same (comment_id, phase) twice — should not error, should have exactly 1 row
	inserted1, err := db.OutboxInsertPhased(ctx, "C1", "T1", "reply-1", "compact")
	if err != nil {
		t.Fatal(err)
	}
	if !inserted1 {
		t.Error("first insert should return inserted=true")
	}
	inserted2, err := db.OutboxInsertPhased(ctx, "C1", "T1", "reply-1", "compact")
	if err != nil {
		t.Fatalf("second insert should be idempotent (INSERT OR IGNORE), got: %v", err)
	}
	if inserted2 {
		t.Error("second insert should return inserted=false (duplicate)")
	}
	var count int
	db.RawDB().QueryRow(`SELECT COUNT(*) FROM outbox WHERE comment_id='C1' AND phase='compact'`).Scan(&count)
	if count != 1 {
		t.Errorf("expected 1 row, got %d", count)
	}
}

func TestUpdateAndResetInboxPhase(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	db.RawDB().Exec(`INSERT INTO inbox (comment_id,task_id,content,creator_id,created_at,source,phase)
		VALUES ('C1','T1','original content','U1',1,'human','normal')`)

	// Transition to compact, snapshot original_content
	if err := db.UpdateInboxPhase(ctx, "C1", "compact", "original content"); err != nil {
		t.Fatal(err)
	}
	var phase, oc string
	db.RawDB().QueryRow(`SELECT phase, COALESCE(original_content,'') FROM inbox WHERE comment_id='C1'`).Scan(&phase, &oc)
	if phase != "compact" {
		t.Errorf("phase=%q, want compact", phase)
	}
	if oc != "original content" {
		t.Errorf("original_content=%q, want 'original content'", oc)
	}

	// Reset — phase='normal', original_content=NULL
	if err := db.ResetInboxPhase(ctx, "C1"); err != nil {
		t.Fatal(err)
	}
	db.RawDB().QueryRow(`SELECT phase, COALESCE(original_content,'') FROM inbox WHERE comment_id='C1'`).Scan(&phase, &oc)
	if phase != "normal" {
		t.Errorf("after reset: phase=%q, want normal", phase)
	}
	if oc != "" {
		t.Errorf("after reset: original_content=%q, want empty", oc)
	}
}

func TestListStaleDeferrals_Boundary(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	pastMs := time.Now().Add(-time.Hour).UnixMilli()
	futureMs := time.Now().Add(time.Hour).UnixMilli()

	// Past row (overdue) — should appear
	db.RawDB().Exec(`INSERT INTO inbox (comment_id,task_id,content,creator_id,created_at,source,phase,scheduled_for)
		VALUES ('C1','T1','hi','U1',1,'autonomous','normal',?)`, pastMs)
	// Future row — should NOT appear
	db.RawDB().Exec(`INSERT INTO inbox (comment_id,task_id,content,creator_id,created_at,source,phase,scheduled_for)
		VALUES ('C2','T1','hi','U1',2,'autonomous','normal',?)`, futureMs)
	// Already processed row — should NOT appear even if overdue
	db.RawDB().Exec(`INSERT INTO inbox (comment_id,task_id,content,creator_id,created_at,source,phase,scheduled_for,processed_at)
		VALUES ('C3','T1','hi','U1',3,'autonomous','normal',?,?)`, pastMs, pastMs)

	rows, err := db.ListStaleDeferrals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Errorf("expected 1 stale row, got %d", len(rows))
	}
	if len(rows) > 0 && rows[0].CommentID != "C1" {
		t.Errorf("expected C1, got %s", rows[0].CommentID)
	}
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
