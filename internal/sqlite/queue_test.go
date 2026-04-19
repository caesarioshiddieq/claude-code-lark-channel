package sqlite_test

import (
	"testing"
	"time"

	"github.com/caesarioshiddieq/claude-code-lark-channel/internal/lark"
	q "github.com/caesarioshiddieq/claude-code-lark-channel/internal/sqlite"
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
	db := openTestDB(t)
	c := lark.Comment{CommentID: "c1", Content: "hello", CreatedAt: time.Now().UnixMilli(),
		Creator: lark.Creator{ID: "u1", Type: "user"}}

	if err := db.InsertInbox("task-1", c); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertInbox("task-1", c); err != nil {
		t.Fatalf("duplicate insert should be ignored, got: %v", err)
	}

	row, found, err := db.NextInboxRow()
	if err != nil || !found {
		t.Fatalf("expected row: err=%v found=%v", err, found)
	}
	if row.CommentID != "c1" || row.TaskID != "task-1" {
		t.Fatalf("unexpected row: %+v", row)
	}
}

func TestWatermark_SetAndGet(t *testing.T) {
	db := openTestDB(t)
	_, found, _ := db.GetWatermark("task-1")
	if found {
		t.Fatal("expected no watermark initially")
	}
	if err := db.SetWatermark("task-1", "c42"); err != nil {
		t.Fatal(err)
	}
	wm, found, _ := db.GetWatermark("task-1")
	if !found || wm != "c42" {
		t.Fatalf("want c42, got %s found=%v", wm, found)
	}
}

func TestSession_UpsertAndGet(t *testing.T) {
	db := openTestDB(t)
	_, found, _ := db.GetSession("task-1")
	if found {
		t.Fatal("expected no session initially")
	}
	if err := db.UpsertSession("task-1", "uuid-abc"); err != nil {
		t.Fatal(err)
	}
	uuid, found, _ := db.GetSession("task-1")
	if !found || uuid != "uuid-abc" {
		t.Fatalf("want uuid-abc, got %s found=%v", uuid, found)
	}
}

func TestOutbox_InsertCheckMarkPosted(t *testing.T) {
	db := openTestDB(t)
	_, found, _ := db.OutboxCheck("hash1")
	if found {
		t.Fatal("expected no outbox row initially")
	}
	if err := db.OutboxInsert("hash1", "task-1", "c1"); err != nil {
		t.Fatal(err)
	}
	larkID, found, _ := db.OutboxCheck("hash1")
	if !found {
		t.Fatal("expected outbox row after insert")
	}
	if larkID != "" {
		t.Fatalf("expected null lark_comment_id, got %s", larkID)
	}
	if err := db.OutboxMarkPosted("hash1", "new-c99"); err != nil {
		t.Fatal(err)
	}
	larkID, found, _ = db.OutboxCheck("hash1")
	if !found || larkID != "new-c99" {
		t.Fatalf("want new-c99, got %s found=%v", larkID, found)
	}
}
