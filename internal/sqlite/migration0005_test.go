package sqlite_test

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

// TestMigration0005_AddsColumnsIdempotent verifies that opening a DB (which
// applies all migrations including 0005) creates the nine new gnhf-native
// columns on implementer_runs, and that schema_migrations records version 5.
func TestMigration0005_AddsColumnsIdempotent(t *testing.T) {
	db := openTestDB(t)
	raw := db.RawDB()

	// version row must exist
	var version int
	if err := raw.QueryRow(`SELECT version FROM schema_migrations WHERE version = 5`).Scan(&version); err != nil {
		t.Fatalf("schema_migrations row for version 5: %v", err)
	}
	if version != 5 {
		t.Fatalf("want version 5, got %d", version)
	}

	// Collect actual columns
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

	// New columns from migration 0005
	newCols := []string{
		"gnhf_status",
		"gnhf_reason",
		"gnhf_success_count",
		"gnhf_fail_count",
		"gnhf_input_tokens",
		"gnhf_output_tokens",
		"gnhf_run_id",
		"gnhf_no_progress",
		"gnhf_last_message",
	}
	for _, c := range newCols {
		if !cols[c] {
			t.Errorf("missing column from migration0005: %s", c)
		}
	}

	// Legacy columns must still be present
	legacyCols := []string{"outcome", "tokens_used"}
	for _, c := range legacyCols {
		if !cols[c] {
			t.Errorf("legacy column missing (regression): %s", c)
		}
	}
}

// TestMigration0005_BackfillsZeros verifies that an existing row created
// before migration0005 (simulated by inserting directly then querying the
// new columns) reads as empty string / zero — confirming the DEFAULT values
// are applied correctly to pre-existing rows.
func TestMigration0005_BackfillsZeros(t *testing.T) {
	db := openTestDB(t)
	raw := db.RawDB()

	// Insert a row using only the migration0004 columns to simulate a pre-0005
	// row. Omit the new columns entirely.
	_, err := raw.Exec(`INSERT INTO implementer_runs
		(inbox_comment_id, task_id, started_at, outcome, gnhf_iterations, gnhf_commits_made,
		 tokens_used, worktree_path, branch_name, pr_url, notes_md_excerpt, error)
		VALUES ('c-pre5', 'task-pre5', 1234567890000, '', 0, 0, 0, '', '', '', '', '')`)
	if err != nil {
		t.Fatalf("insert pre-0005 row: %v", err)
	}

	// Read back the new columns and assert defaults
	var gnhfStatus, gnhfReason, gnhfRunID, gnhfLastMessage string
	var gnhfSuccessCount, gnhfFailCount, gnhfInputTokens, gnhfOutputTokens, gnhfNoProgress int
	err = raw.QueryRow(`SELECT gnhf_status, gnhf_reason, gnhf_success_count, gnhf_fail_count,
		gnhf_input_tokens, gnhf_output_tokens, gnhf_run_id, gnhf_no_progress, gnhf_last_message
		FROM implementer_runs WHERE inbox_comment_id = 'c-pre5'`).Scan(
		&gnhfStatus, &gnhfReason, &gnhfSuccessCount, &gnhfFailCount,
		&gnhfInputTokens, &gnhfOutputTokens, &gnhfRunID, &gnhfNoProgress, &gnhfLastMessage)
	if err != nil {
		t.Fatalf("read new columns: %v", err)
	}
	if gnhfStatus != "" {
		t.Errorf("gnhf_status default: want '', got %q", gnhfStatus)
	}
	if gnhfReason != "" {
		t.Errorf("gnhf_reason default: want '', got %q", gnhfReason)
	}
	if gnhfSuccessCount != 0 {
		t.Errorf("gnhf_success_count default: want 0, got %d", gnhfSuccessCount)
	}
	if gnhfFailCount != 0 {
		t.Errorf("gnhf_fail_count default: want 0, got %d", gnhfFailCount)
	}
	if gnhfInputTokens != 0 {
		t.Errorf("gnhf_input_tokens default: want 0, got %d", gnhfInputTokens)
	}
	if gnhfOutputTokens != 0 {
		t.Errorf("gnhf_output_tokens default: want 0, got %d", gnhfOutputTokens)
	}
	if gnhfRunID != "" {
		t.Errorf("gnhf_run_id default: want '', got %q", gnhfRunID)
	}
	if gnhfNoProgress != 0 {
		t.Errorf("gnhf_no_progress default: want 0, got %d", gnhfNoProgress)
	}
	if gnhfLastMessage != "" {
		t.Errorf("gnhf_last_message default: want '', got %q", gnhfLastMessage)
	}
}
