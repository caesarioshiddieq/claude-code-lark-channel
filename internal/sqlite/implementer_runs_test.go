package sqlite_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	q "github.com/caesarioshiddieq/claude-code-lark-channel/internal/sqlite"
	_ "modernc.org/sqlite"
)

// TestMigration0004_CreatesImplementerRunsTable verifies that opening a fresh
// DB applies migration0004 (registers version 4 in schema_migrations and
// creates the implementer_runs table with the 14-column schema spec'd in
// docs/superpowers/plans/2026-04-27-autonomous-implementer-plan.md Step 1.1).
func TestMigration0004_CreatesImplementerRunsTable(t *testing.T) {
	db := openTestDB(t)
	raw := db.RawDB()

	var version int
	err := raw.QueryRow(`SELECT version FROM schema_migrations WHERE version = 4`).Scan(&version)
	if err != nil {
		t.Fatalf("expected schema_migrations row for version 4, got: %v", err)
	}
	if version != 4 {
		t.Fatalf("want version 4, got %d", version)
	}

	var name string
	err = raw.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='implementer_runs'`,
	).Scan(&name)
	if err != nil {
		t.Fatalf("implementer_runs table not found: %v", err)
	}

	rows, err := raw.Query(`PRAGMA table_info(implementer_runs)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	cols := map[string]bool{}
	for rows.Next() {
		var cid int
		var nm, ty string
		var notnull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &nm, &ty, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		cols[nm] = true
	}

	expected := []string{
		"id", "inbox_comment_id", "task_id", "started_at", "finished_at",
		"outcome", "gnhf_iterations", "gnhf_commits_made", "tokens_used",
		"worktree_path", "branch_name", "pr_url", "notes_md_excerpt", "error",
	}
	for _, c := range expected {
		if !cols[c] {
			t.Errorf("missing column: %s", c)
		}
	}
}

// TestImplementerRun_RoundTrip exercises the insert -> get-by-comment-id path.
// A freshly-inserted run has empty outcome and nil finished_at until the
// outcome update lands (asserted in TestImplementerRun_OutcomeUpdate).
func TestImplementerRun_RoundTrip(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	if _, found, err := db.GetImplementerRunByCommentID(ctx, "missing"); err != nil {
		t.Fatalf("pre-insert get err: %v", err)
	} else if found {
		t.Fatal("pre-insert: expected found=false for missing comment")
	}

	started := time.Now().UnixMilli()
	run := q.ImplementerRun{
		InboxCommentID: "c-impl-1",
		TaskID:         "task-impl-1",
		StartedAt:      started,
		WorktreePath:   "/var/lib/claude-vm/worktrees/task-impl-1",
		BranchName:     "implement/task-impl-1-deadbeef",
	}

	id, err := db.InsertImplementerRun(ctx, run)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if id == 0 {
		t.Fatal("expected non-zero auto-increment id")
	}

	got, found, err := db.GetImplementerRunByCommentID(ctx, "c-impl-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !found {
		t.Fatal("expected found=true after insert")
	}
	if got.ID != id {
		t.Errorf("ID: got %d want %d", got.ID, id)
	}
	if got.InboxCommentID != "c-impl-1" {
		t.Errorf("InboxCommentID: got %q want %q", got.InboxCommentID, "c-impl-1")
	}
	if got.TaskID != "task-impl-1" {
		t.Errorf("TaskID: got %q want %q", got.TaskID, "task-impl-1")
	}
	if got.StartedAt != started {
		t.Errorf("StartedAt: got %d want %d", got.StartedAt, started)
	}
	if got.WorktreePath != "/var/lib/claude-vm/worktrees/task-impl-1" {
		t.Errorf("WorktreePath: got %q", got.WorktreePath)
	}
	if got.BranchName != "implement/task-impl-1-deadbeef" {
		t.Errorf("BranchName: got %q", got.BranchName)
	}
	if got.Outcome != "" {
		t.Errorf("Outcome: want empty pre-completion, got %q", got.Outcome)
	}
	if got.FinishedAt != nil {
		t.Errorf("FinishedAt: want nil pre-completion, got %v", got.FinishedAt)
	}
}

// TestImplementerRun_OutcomeUpdate exercises the post-spawn finalization path:
// supervisor calls UpdateImplementerRunOutcome with the gnhf-derived stats
// once the subprocess exits, and the row is then visible with the new state.
func TestImplementerRun_OutcomeUpdate(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	started := time.Now().UnixMilli()
	id, err := db.InsertImplementerRun(ctx, q.ImplementerRun{
		InboxCommentID: "c-impl-2",
		TaskID:         "task-impl-2",
		StartedAt:      started,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	finished := started + 1_234_567
	outcome := q.ImplementerRunOutcome{
		FinishedAt:      finished,
		Outcome:         "success",
		GnhfIterations:  7,
		GnhfCommitsMade: 5,
		TokensUsed:      123_456,
		PRURL:           "https://github.com/example/repo/pull/42",
		NotesMDExcerpt:  "All tests pass. 7 iterations.",
	}
	if err := db.UpdateImplementerRunOutcome(ctx, id, outcome); err != nil {
		t.Fatalf("update outcome: %v", err)
	}

	got, found, err := db.GetImplementerRunByCommentID(ctx, "c-impl-2")
	if err != nil || !found {
		t.Fatalf("get: err=%v found=%v", err, found)
	}
	if got.Outcome != "success" {
		t.Errorf("Outcome: got %q want success", got.Outcome)
	}
	if got.FinishedAt == nil {
		t.Fatalf("FinishedAt: want non-nil after update, got nil")
	}
	if *got.FinishedAt != finished {
		t.Errorf("FinishedAt: got %d want %d", *got.FinishedAt, finished)
	}
	if got.GnhfIterations != 7 {
		t.Errorf("GnhfIterations: got %d want 7", got.GnhfIterations)
	}
	if got.GnhfCommitsMade != 5 {
		t.Errorf("GnhfCommitsMade: got %d want 5", got.GnhfCommitsMade)
	}
	if got.TokensUsed != 123_456 {
		t.Errorf("TokensUsed: got %d want 123456", got.TokensUsed)
	}
	if got.PRURL != "https://github.com/example/repo/pull/42" {
		t.Errorf("PRURL: got %q", got.PRURL)
	}
	if got.NotesMDExcerpt != "All tests pass. 7 iterations." {
		t.Errorf("NotesMDExcerpt: got %q", got.NotesMDExcerpt)
	}
}
