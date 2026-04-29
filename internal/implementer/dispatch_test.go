package implementer_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caesarioshiddieq/claude-code-lark-channel/internal/implementer"
	"github.com/caesarioshiddieq/claude-code-lark-channel/internal/sqlite"
)

// ---- fake implementations ----

type fakeDB struct {
	mu sync.Mutex

	deferredCalls  []deferredCall
	processedCalls []string
	insertedRuns   []sqlite.ImplementerRun
	insertRunID    int64
	finalizedRuns  map[int64]sqlite.ImplementerRunFinalize
	outboxRows     []outboxRow

	// Idempotency-guard support (codex round-2 #1).
	// Tests seed seedImplementerRun to exercise crash-recovery cases B/C.
	// lookupErr forces GetImplementerRunByCommentID to fail (operator pager path).
	seedImplementerRun *sqlite.ImplementerRun
	lookupErr          error

	// Outbox lark-id tracking (codex round-3 #1).
	// outboxLarkIDs maps content_hash -> lark_comment_id. "" means inserted but
	// never posted (the case-B/C crash window). Non-empty means PostComment
	// already succeeded on a prior run. Tests can pre-seed entries to exercise
	// recovery paths. OutboxInsertPhased records the new hash with "" so the
	// normal flow's MarkPosted call has somewhere to write.
	outboxLarkIDs   map[string]string
	outboxCheckErr  error // forces OutboxCheck to fail
	markPostedCalls []markPostedCall
	markPostedErr   error
	insertPhasedErr error // forces OutboxInsertPhased to fail

	insertRunErr    error
	markDeferredErr error
}

type markPostedCall struct {
	Hash          string
	LarkCommentID string
}

type deferredCall struct {
	CommentID    string
	ScheduledFor int64
	Content      string
}

type outboxRow struct {
	CommentID string
	TaskID    string
	ReplyTo   string
	Phase     string
}

func newFakeDB() *fakeDB {
	return &fakeDB{
		finalizedRuns: make(map[int64]sqlite.ImplementerRunFinalize),
		outboxLarkIDs: make(map[string]string),
	}
}

func (f *fakeDB) MarkDeferred(ctx context.Context, commentID string, scheduledFor int64, content string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deferredCalls = append(f.deferredCalls, deferredCall{commentID, scheduledFor, content})
	return f.markDeferredErr
}

func (f *fakeDB) MarkInboxProcessed(ctx context.Context, commentID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.processedCalls = append(f.processedCalls, commentID)
	return nil
}

func (f *fakeDB) InsertImplementerRun(ctx context.Context, r sqlite.ImplementerRun) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.insertRunErr != nil {
		return 0, f.insertRunErr
	}
	f.insertRunID++
	r.ID = f.insertRunID
	f.insertedRuns = append(f.insertedRuns, r)
	return f.insertRunID, nil
}

func (f *fakeDB) FinalizeImplementerRun(ctx context.Context, id int64, fin sqlite.ImplementerRunFinalize) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finalizedRuns[id] = fin
	return nil
}

func (f *fakeDB) OutboxInsertPhased(ctx context.Context, commentID, taskID, replyTo, phase string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.insertPhasedErr != nil {
		return false, f.insertPhasedErr
	}
	hash := commentID + ":" + phase
	if f.outboxLarkIDs == nil {
		f.outboxLarkIDs = make(map[string]string)
	}
	// If a pre-seeded entry already exists (test simulating prior crash), report
	// inserted=false and leave the seeded lark_comment_id in place.
	if _, exists := f.outboxLarkIDs[hash]; exists {
		f.outboxRows = append(f.outboxRows, outboxRow{commentID, taskID, replyTo, phase})
		return false, nil
	}
	// Fresh insert: register the row with empty lark_comment_id (will be filled
	// in by OutboxMarkPosted when post succeeds).
	f.outboxLarkIDs[hash] = ""
	f.outboxRows = append(f.outboxRows, outboxRow{commentID, taskID, replyTo, phase})
	return true, nil
}

func (f *fakeDB) OutboxCheck(ctx context.Context, hash string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.outboxCheckErr != nil {
		return "", false, f.outboxCheckErr
	}
	larkID, found := f.outboxLarkIDs[hash]
	return larkID, found, nil
}

func (f *fakeDB) OutboxMarkPosted(ctx context.Context, hash, larkCommentID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.markPostedCalls = append(f.markPostedCalls, markPostedCall{hash, larkCommentID})
	if f.markPostedErr != nil {
		return f.markPostedErr
	}
	if f.outboxLarkIDs == nil {
		f.outboxLarkIDs = make(map[string]string)
	}
	f.outboxLarkIDs[hash] = larkCommentID
	return nil
}

func (f *fakeDB) GetImplementerRunByCommentID(ctx context.Context, commentID string) (sqlite.ImplementerRun, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lookupErr != nil {
		return sqlite.ImplementerRun{}, false, f.lookupErr
	}
	if f.seedImplementerRun != nil && f.seedImplementerRun.InboxCommentID == commentID {
		return *f.seedImplementerRun, true, nil
	}
	return sqlite.ImplementerRun{}, false, nil
}

// fakeWorktree records calls and returns configured path/branch.
type fakeWorktree struct {
	mu           sync.Mutex
	ensurePath   string
	ensureBranch string
	ensureErr    error
	cleanupCalls []cleanupCall
}

type cleanupCall struct {
	TaskID  string
	Success bool
}

func (w *fakeWorktree) EnsureForTask(ctx context.Context, taskID string) (string, string, error) {
	if w.ensureErr != nil {
		return "", "", w.ensureErr
	}
	return w.ensurePath, w.ensureBranch, nil
}

func (w *fakeWorktree) Cleanup(ctx context.Context, taskID string, success bool) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cleanupCalls = append(w.cleanupCalls, cleanupCall{taskID, success})
	return nil
}

// compile-time interface checks
var _ implementer.DBClient = (*fakeDB)(nil)
var _ implementer.WorktreeClient = (*fakeWorktree)(nil)

// ---- helpers ----

func makeRow(commentID, taskID, content, source string) sqlite.InboxRow {
	return sqlite.InboxRow{
		CommentID: commentID,
		TaskID:    taskID,
		Content:   content,
		Source:    source,
		Phase:     "implement",
	}
}

// nightEnvOn forces CanSpawn to allow autonomous spawns by setting the night
// window to cover the full 24-hour clock (NIGHT_START=0, NIGHT_END=23).
// LOCK_DIR is set defensively in case any indirect helper still touches it,
// even though DispatchImplement no longer acquires its own flock (codex #2).
func nightEnvOn(t *testing.T) {
	t.Helper()
	t.Setenv("NIGHT_START", "0")
	t.Setenv("NIGHT_END", "23")
	t.Setenv("LOCK_DIR", t.TempDir())
}

// dayEnvOn forces CanSpawn to reject autonomous spawns by placing the night
// window far in the future (NIGHT_START=22, NIGHT_END=6, TZ=UTC; CI runs
// midday UTC so the current hour is almost never in 22–06).
func dayEnvOn(t *testing.T) {
	t.Helper()
	t.Setenv("TZ", "UTC")
	t.Setenv("NIGHT_START", "22")
	t.Setenv("NIGHT_END", "6")
}

// baseDeps builds a Deps with the given components.
func baseDeps(db implementer.DBClient, wt implementer.WorktreeClient, spawn implementer.SpawnFunc) implementer.Deps {
	return implementer.Deps{
		DB:        db,
		Worktree:  wt,
		Spawn:     spawn,
		RepoPath:  "/repo",
		Now:       time.Now,
		JitterMin: 0,
	}
}

// assertOutboxMarker verifies that exactly one outbox row was inserted via
// OutboxInsertPhased with the canonical phased-intent-marker arg shape:
//
//	(commentID = row.CommentID, taskID = row.TaskID,
//	 replyTo  = row.CommentID, phase = "implement")
//
// The outbox table has no content column — Task 6 composes the actual Lark
// reply at flush time by reading implementer_runs. So the test asserts only
// that the marker is present and correctly addressed.
func assertOutboxMarker(t *testing.T, db *fakeDB, row sqlite.InboxRow) {
	t.Helper()
	if len(db.outboxRows) != 1 {
		t.Fatalf("expected 1 outbox marker row, got %d", len(db.outboxRows))
	}
	got := db.outboxRows[0]
	if got.CommentID != row.CommentID {
		t.Errorf("outbox commentID: got %q want %q (must equal row.CommentID, NOT a JSON payload)",
			got.CommentID, row.CommentID)
	}
	if got.TaskID != row.TaskID {
		t.Errorf("outbox taskID: got %q want %q", got.TaskID, row.TaskID)
	}
	if got.ReplyTo != row.CommentID {
		t.Errorf("outbox replyTo: got %q want %q (replies into the same comment thread)",
			got.ReplyTo, row.CommentID)
	}
	if got.Phase != "implement" {
		t.Errorf("outbox phase: got %q want implement", got.Phase)
	}
}

// ---- test cases ----

// (a) Day-window CanSpawn=false → MarkDeferred called, no Spawn, no run row.
func TestDispatchImplement_DayWindow_Deferred(t *testing.T) {
	dayEnvOn(t)
	db := newFakeDB()
	wt := &fakeWorktree{ensurePath: "/wt/t1", ensureBranch: "implement/t1-aabb"}
	spawnCalled := false
	deps := baseDeps(db, wt, func(ctx context.Context, args implementer.GnhfArgs) (implementer.GnhfResult, error) {
		spawnCalled = true
		return implementer.GnhfResult{}, nil
	})
	// Pin Now() to 12:00 UTC — guaranteed outside the 22-06 night window
	// regardless of when the test actually runs. Without this, the test failed
	// any time the wall clock landed inside 22-06 UTC (e.g. 04:55 UTC).
	deps.Now = func() time.Time { return time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC) }
	row := makeRow("c1", "task-1", "implement feature X", "autonomous")

	err := implementer.DispatchImplement(context.Background(), row, deps)
	if err != nil {
		t.Fatalf("DispatchImplement returned err: %v", err)
	}
	if spawnCalled {
		t.Error("Spawn must not be called in day window")
	}
	if len(db.deferredCalls) != 1 {
		t.Errorf("expected 1 MarkDeferred call, got %d", len(db.deferredCalls))
	}
	if len(db.insertedRuns) != 0 {
		t.Errorf("expected 0 InsertImplementerRun calls, got %d", len(db.insertedRuns))
	}
}

// (b) Successful spawn: Status=Stopped, Reason=StopWhen, CommitCount=3 →
// outcome="success", outbox row queued, worktree preserved, MarkInboxProcessed called.
func TestDispatchImplement_Success(t *testing.T) {
	nightEnvOn(t)
	db := newFakeDB()
	wt := &fakeWorktree{ensurePath: "/wt/task-2", ensureBranch: "implement/task-2-deadbeef"}
	deps := baseDeps(db, wt, func(ctx context.Context, args implementer.GnhfArgs) (implementer.GnhfResult, error) {
		return implementer.GnhfResult{
			Status:       implementer.StatusStopped,
			Reason:       implementer.ReasonStopWhen,
			CommitCount:  3,
			InputTokens:  1000,
			OutputTokens: 500,
			Iterations:   5,
		}, nil
	})
	row := makeRow("c2", "task-2", "impl task 2", "autonomous")

	if err := implementer.DispatchImplement(context.Background(), row, deps); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	if len(db.insertedRuns) != 1 {
		t.Fatalf("expected 1 InsertImplementerRun, got %d", len(db.insertedRuns))
	}
	if len(db.finalizedRuns) != 1 {
		t.Fatalf("expected 1 FinalizeImplementerRun, got %d", len(db.finalizedRuns))
	}
	fin := db.finalizedRuns[1]
	if fin.Outcome != "success" {
		t.Errorf("outcome: got %q want success", fin.Outcome)
	}
	if fin.TokensUsed != 1500 {
		t.Errorf("tokens_used: got %d want 1500 (1000+500)", fin.TokensUsed)
	}
	// Dispatcher must inject finished_at via deps.Now (not the SQL layer's clock).
	if fin.FinishedAt == 0 {
		t.Error("FinishedAt must be set by the dispatcher (deps.Now), got zero")
	}
	assertOutboxMarker(t, db, row)
	// CommitCount=3 → worktree preserved (Cleanup called with success=true).
	if len(wt.cleanupCalls) != 1 {
		t.Fatalf("expected 1 Cleanup call, got %d", len(wt.cleanupCalls))
	}
	if !wt.cleanupCalls[0].Success {
		t.Error("Cleanup(success=true) expected for CommitCount>0")
	}
	if len(db.processedCalls) != 1 {
		t.Errorf("expected MarkInboxProcessed called once, got %d", len(db.processedCalls))
	}
}

// (c) Failed spawn: Status=Aborted, Reason=MaxFailures →
// outcome="failed", outbox queued, worktree cleaned (CommitCount=0).
func TestDispatchImplement_Failed(t *testing.T) {
	nightEnvOn(t)
	db := newFakeDB()
	wt := &fakeWorktree{ensurePath: "/wt/task-3", ensureBranch: "implement/task-3-deadbeef"}
	deps := baseDeps(db, wt, func(ctx context.Context, args implementer.GnhfArgs) (implementer.GnhfResult, error) {
		return implementer.GnhfResult{
			Status:       implementer.StatusAborted,
			Reason:       implementer.ReasonMaxFailures,
			CommitCount:  0,
			InputTokens:  200,
			OutputTokens: 100,
			NoProgress:   true,
		}, nil
	})
	row := makeRow("c3", "task-3", "impl task 3", "autonomous")

	if err := implementer.DispatchImplement(context.Background(), row, deps); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	fin := db.finalizedRuns[1]
	if fin.Outcome != "failed" {
		t.Errorf("outcome: got %q want failed", fin.Outcome)
	}
	// CommitCount=0 → worktree cleaned (success=false).
	if len(wt.cleanupCalls) != 1 || wt.cleanupCalls[0].Success {
		t.Errorf("expected Cleanup(success=false), calls=%v", wt.cleanupCalls)
	}
	assertOutboxMarker(t, db, row)
}

// (d) StopWhen match but no commits → outcome="blocked", worktree cleaned.
func TestDispatchImplement_Blocked(t *testing.T) {
	nightEnvOn(t)
	db := newFakeDB()
	wt := &fakeWorktree{ensurePath: "/wt/task-4", ensureBranch: "implement/task-4-deadbeef"}
	deps := baseDeps(db, wt, func(ctx context.Context, args implementer.GnhfArgs) (implementer.GnhfResult, error) {
		return implementer.GnhfResult{
			Status:      implementer.StatusStopped,
			Reason:      implementer.ReasonStopWhen,
			CommitCount: 0,
			NoProgress:  true,
		}, nil
	})
	row := makeRow("c4", "task-4", "impl task 4", "autonomous")

	if err := implementer.DispatchImplement(context.Background(), row, deps); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	fin := db.finalizedRuns[1]
	if fin.Outcome != "blocked" {
		t.Errorf("outcome: got %q want blocked", fin.Outcome)
	}
	if len(wt.cleanupCalls) != 1 || wt.cleanupCalls[0].Success {
		t.Errorf("expected Cleanup(success=false) for blocked, calls=%v", wt.cleanupCalls)
	}
	assertOutboxMarker(t, db, row)
}

// (e) NoProgress does NOT override Aborted: Status=Aborted, Reason=MaxTokens,
// CommitCount=0 → outcome="timeout" (NOT "blocked").
func TestDispatchImplement_Timeout_NoProgressDoesNotOverride(t *testing.T) {
	nightEnvOn(t)
	db := newFakeDB()
	wt := &fakeWorktree{ensurePath: "/wt/task-5", ensureBranch: "implement/task-5-deadbeef"}
	deps := baseDeps(db, wt, func(ctx context.Context, args implementer.GnhfArgs) (implementer.GnhfResult, error) {
		return implementer.GnhfResult{
			Status:      implementer.StatusAborted,
			Reason:      implementer.ReasonMaxTokens,
			CommitCount: 0,
			NoProgress:  true,
		}, nil
	})
	row := makeRow("c5", "task-5", "impl task 5", "autonomous")

	if err := implementer.DispatchImplement(context.Background(), row, deps); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	fin := db.finalizedRuns[1]
	if fin.Outcome != "timeout" {
		t.Errorf("outcome: got %q want timeout (NoProgress must not override Aborted)", fin.Outcome)
	}
	assertOutboxMarker(t, db, row)
}

// (f) Spawn returns ErrIncompleteLog with synthesized result →
// run row finalized with outcome="failed"; processing continues without panic.
func TestDispatchImplement_ErrIncompleteLog(t *testing.T) {
	nightEnvOn(t)
	db := newFakeDB()
	wt := &fakeWorktree{ensurePath: "/wt/task-6", ensureBranch: "implement/task-6-deadbeef"}
	deps := baseDeps(db, wt, func(ctx context.Context, args implementer.GnhfArgs) (implementer.GnhfResult, error) {
		synth := implementer.GnhfResult{
			Status:        implementer.StatusAborted,
			Reason:        implementer.ReasonUnknown,
			LogIncomplete: true,
		}
		return synth, implementer.ErrIncompleteLog
	})
	row := makeRow("c6", "task-6", "impl task 6", "autonomous")

	if err := implementer.DispatchImplement(context.Background(), row, deps); err != nil {
		t.Fatalf("ErrIncompleteLog must not propagate: %v", err)
	}
	if len(db.finalizedRuns) != 1 {
		t.Fatalf("expected finalized run, got %d", len(db.finalizedRuns))
	}
	fin := db.finalizedRuns[1]
	if fin.Outcome != "failed" {
		t.Errorf("outcome: got %q want failed for incomplete log", fin.Outcome)
	}
	if fin.Error == "" {
		t.Error("Error field must be non-empty for ErrIncompleteLog")
	}
	// Outbox and MarkProcessed should still be called even on incomplete log.
	assertOutboxMarker(t, db, row)
	if len(db.processedCalls) != 1 {
		t.Errorf("expected MarkInboxProcessed even on incomplete log, got %d", len(db.processedCalls))
	}
}

// (g) Priority ordering test is in queue_test.go (Step 5.3 — sqlite layer).

// (h-a) Auto-PR: env=true, success outcome, CommitCount>0, mock gh succeeds → pr_url written.
func TestDispatchImplement_AutoPR_Success(t *testing.T) {
	nightEnvOn(t)
	t.Setenv("IMPLEMENTER_AUTO_PR", "true")

	binDir := t.TempDir()
	ghScript := "#!/bin/sh\necho 'https://github.com/example/repo/pull/99'\nexit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "gh"), []byte(ghScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	db := newFakeDB()
	wt := &fakeWorktree{ensurePath: "/wt/task-pr", ensureBranch: "implement/task-pr-aabbccdd"}
	deps := baseDeps(db, wt, func(ctx context.Context, args implementer.GnhfArgs) (implementer.GnhfResult, error) {
		return implementer.GnhfResult{
			Status:       implementer.StatusStopped,
			Reason:       implementer.ReasonStopWhen,
			CommitCount:  2,
			InputTokens:  100,
			OutputTokens: 50,
		}, nil
	})
	row := makeRow("c-pr", "task-pr", "impl pr task", "autonomous")

	if err := implementer.DispatchImplement(context.Background(), row, deps); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	fin := db.finalizedRuns[1]
	if fin.Outcome != "success" {
		t.Errorf("outcome: got %q want success", fin.Outcome)
	}
	if fin.PRURL != "https://github.com/example/repo/pull/99" {
		t.Errorf("pr_url: got %q want https://github.com/example/repo/pull/99", fin.PRURL)
	}
}

// (h-b) Auto-PR: env=true, gh fails → outcome stays "success", pr_url empty, no error propagation.
func TestDispatchImplement_AutoPR_GhFails(t *testing.T) {
	nightEnvOn(t)
	t.Setenv("IMPLEMENTER_AUTO_PR", "true")

	binDir := t.TempDir()
	ghScript := "#!/bin/sh\necho 'gh error: auth failed' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(binDir, "gh"), []byte(ghScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	db := newFakeDB()
	wt := &fakeWorktree{ensurePath: "/wt/task-pr2", ensureBranch: "implement/task-pr2-aabbccdd"}
	deps := baseDeps(db, wt, func(ctx context.Context, args implementer.GnhfArgs) (implementer.GnhfResult, error) {
		return implementer.GnhfResult{
			Status:       implementer.StatusStopped,
			Reason:       implementer.ReasonStopWhen,
			CommitCount:  1,
			InputTokens:  100,
			OutputTokens: 50,
		}, nil
	})
	row := makeRow("c-pr2", "task-pr2", "impl pr task 2", "autonomous")

	if err := implementer.DispatchImplement(context.Background(), row, deps); err != nil {
		t.Fatalf("gh failure must not propagate: %v", err)
	}

	fin := db.finalizedRuns[1]
	if fin.Outcome != "success" {
		t.Errorf("outcome must remain success even when gh fails, got %q", fin.Outcome)
	}
	if fin.PRURL != "" {
		t.Errorf("pr_url must be empty when gh fails, got %q", fin.PRURL)
	}
}

// TestDispatchImplement_StoppedUnknown: Status=Stopped, Reason=Unknown → outcome="stopped".
func TestDispatchImplement_StoppedUnknown(t *testing.T) {
	nightEnvOn(t)
	db := newFakeDB()
	wt := &fakeWorktree{ensurePath: "/wt/task-su", ensureBranch: "implement/task-su-aabb"}
	deps := baseDeps(db, wt, func(ctx context.Context, args implementer.GnhfArgs) (implementer.GnhfResult, error) {
		return implementer.GnhfResult{
			Status:      implementer.StatusStopped,
			Reason:      implementer.ReasonUnknown,
			CommitCount: 0,
		}, nil
	})
	row := makeRow("c-su", "task-su", "impl su task", "autonomous")

	if err := implementer.DispatchImplement(context.Background(), row, deps); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	fin := db.finalizedRuns[1]
	if fin.Outcome != "stopped" {
		t.Errorf("outcome: got %q want stopped", fin.Outcome)
	}
}

// ---- fakeLarkClient ----

type postCall struct {
	TaskID           string
	Content          string
	ReplyToCommentID string
}

type fakeLarkClient struct {
	mu        sync.Mutex
	calls     []postCall
	returnErr error
	returnID  string
}

func (f *fakeLarkClient) PostComment(ctx context.Context, taskID, content, replyToCommentID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, postCall{taskID, content, replyToCommentID})
	if f.returnErr != nil {
		return "", f.returnErr
	}
	id := f.returnID
	if id == "" {
		id = "comment-posted"
	}
	return id, nil
}

var _ implementer.LarkClient = (*fakeLarkClient)(nil)

// baseDepsWithLark builds Deps with a LarkClient injected.
func baseDepsWithLark(db implementer.DBClient, wt implementer.WorktreeClient, spawn implementer.SpawnFunc, lc implementer.LarkClient) implementer.Deps {
	d := baseDeps(db, wt, spawn)
	d.LarkClient = lc
	return d
}

// ---- inline post wiring tests ----

// TestDispatchImplement_InlinePost_InsertedTrue: outbox inserted=true →
// PostComment called once with correct args; content contains the headline.
func TestDispatchImplement_InlinePost_InsertedTrue(t *testing.T) {
	nightEnvOn(t)
	db := newFakeDB()
	lc := &fakeLarkClient{}
	wt := &fakeWorktree{ensurePath: "/wt/task-lc", ensureBranch: "implement/task-lc-aabb"}
	deps := baseDepsWithLark(db, wt, func(ctx context.Context, args implementer.GnhfArgs) (implementer.GnhfResult, error) {
		return implementer.GnhfResult{
			Status:       implementer.StatusStopped,
			Reason:       implementer.ReasonStopWhen,
			CommitCount:  1,
			InputTokens:  200,
			OutputTokens: 100,
			Iterations:   3,
		}, nil
	}, lc)
	row := makeRow("c-lc", "task-lc", "impl lc task", "autonomous")

	if err := implementer.DispatchImplement(context.Background(), row, deps); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	lc.mu.Lock()
	calls := lc.calls
	lc.mu.Unlock()

	if len(calls) != 1 {
		t.Fatalf("expected 1 PostComment call, got %d", len(calls))
	}
	call := calls[0]
	if call.TaskID != row.TaskID {
		t.Errorf("PostComment taskID: got %q want %q", call.TaskID, row.TaskID)
	}
	if call.ReplyToCommentID != row.CommentID {
		t.Errorf("PostComment replyToCommentID: got %q want %q", call.ReplyToCommentID, row.CommentID)
	}
	if !strings.Contains(call.Content, "finished — stop-when condition met") {
		t.Errorf("PostComment content missing headline, got: %q", call.Content)
	}
}

// TestDispatchImplement_InlinePost_InsertedFalse: outbox row already exists
// AND lark_comment_id is non-empty (post already happened on a prior run) →
// PostComment must NOT be called. Codex round-3 #1 distinguishes this from
// the case-B/C recovery path where the marker exists but lark_comment_id="".
func TestDispatchImplement_InlinePost_InsertedFalse(t *testing.T) {
	nightEnvOn(t)

	// Pre-seed the outbox map so OutboxInsertPhased returns inserted=false
	// AND OutboxCheck returns a non-empty lark_comment_id (post already done).
	db := newFakeDB()
	db.outboxLarkIDs["c-idem:implement"] = "lark-comment-from-prior-run"
	lc := &fakeLarkClient{}
	wt := &fakeWorktree{ensurePath: "/wt/task-idem", ensureBranch: "implement/task-idem-aabb"}
	deps := baseDepsWithLark(db, wt, func(ctx context.Context, args implementer.GnhfArgs) (implementer.GnhfResult, error) {
		return implementer.GnhfResult{
			Status:      implementer.StatusStopped,
			Reason:      implementer.ReasonStopWhen,
			CommitCount: 1,
		}, nil
	}, lc)
	row := makeRow("c-idem", "task-idem", "impl idem task", "autonomous")

	if err := implementer.DispatchImplement(context.Background(), row, deps); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	lc.mu.Lock()
	calls := lc.calls
	lc.mu.Unlock()

	if len(calls) != 0 {
		t.Errorf("PostComment must not be called when prior run already posted (lark_comment_id non-empty), got %d calls", len(calls))
	}
	if len(db.markPostedCalls) != 0 {
		t.Errorf("OutboxMarkPosted must not be called when no post happened, got %d calls", len(db.markPostedCalls))
	}
}

// TestDispatchImplement_InlinePost_PostError: PostComment returns error →
// dispatch returns nil (error logged, not propagated); outbox row remains.
func TestDispatchImplement_InlinePost_PostError(t *testing.T) {
	nightEnvOn(t)
	db := newFakeDB()
	lc := &fakeLarkClient{returnErr: fmt.Errorf("lark network timeout")}
	wt := &fakeWorktree{ensurePath: "/wt/task-pe", ensureBranch: "implement/task-pe-aabb"}
	deps := baseDepsWithLark(db, wt, func(ctx context.Context, args implementer.GnhfArgs) (implementer.GnhfResult, error) {
		return implementer.GnhfResult{
			Status:      implementer.StatusStopped,
			Reason:      implementer.ReasonStopWhen,
			CommitCount: 1,
		}, nil
	}, lc)
	row := makeRow("c-pe", "task-pe", "impl pe task", "autonomous")

	err := implementer.DispatchImplement(context.Background(), row, deps)
	if err != nil {
		t.Fatalf("PostComment error must not propagate from DispatchImplement, got: %v", err)
	}

	// Outbox marker must still be present (not rolled back on post failure).
	if len(db.outboxRows) != 1 {
		t.Errorf("expected outbox row to remain after post failure, got %d rows", len(db.outboxRows))
	}
	// MarkInboxProcessed should still be called (pipeline continues).
	if len(db.processedCalls) != 1 {
		t.Errorf("expected MarkInboxProcessed after post error, got %d calls", len(db.processedCalls))
	}
}

// TestDispatchImplement_InlinePost_NilLarkClient: LarkClient=nil →
// dispatch must not panic; outbox row inserted, no post attempted.
func TestDispatchImplement_InlinePost_NilLarkClient(t *testing.T) {
	nightEnvOn(t)
	db := newFakeDB()
	wt := &fakeWorktree{ensurePath: "/wt/task-nil", ensureBranch: "implement/task-nil-aabb"}
	// baseDeps leaves LarkClient as nil — existing tests rely on this behaviour.
	deps := baseDeps(db, wt, func(ctx context.Context, args implementer.GnhfArgs) (implementer.GnhfResult, error) {
		return implementer.GnhfResult{
			Status:      implementer.StatusStopped,
			Reason:      implementer.ReasonStopWhen,
			CommitCount: 1,
		}, nil
	})

	row := makeRow("c-nil", "task-nil", "impl nil task", "autonomous")
	if err := implementer.DispatchImplement(context.Background(), row, deps); err != nil {
		t.Fatalf("nil LarkClient must not cause error, got: %v", err)
	}
	assertOutboxMarker(t, db, row)
}

// TestDispatchImplement_FinishedAtAfterStartedAt verifies that finished_at
// is captured AFTER Spawn returns (a fresh deps.Now() call), not from the
// captured `now` at function entry. Regression test for the codex review #1
// finding: reusing `now` for both timestamps would set duration = 0 even
// when spawn took minutes/hours.
func TestDispatchImplement_FinishedAtAfterStartedAt(t *testing.T) {
	nightEnvOn(t)

	// Synthetic clock: each call advances by 1 second. The dispatcher calls
	// deps.Now() at entry (started_at) and again after Spawn returns
	// (finished_at) — at minimum 2 calls. With a per-call advance, the
	// difference is provable regardless of wall-clock jitter.
	base := time.Date(2026, 4, 28, 22, 0, 0, 0, time.UTC)
	var nowCallCount int
	clock := func() time.Time {
		nowCallCount++
		return base.Add(time.Duration(nowCallCount) * time.Second)
	}

	db := newFakeDB()
	wt := &fakeWorktree{ensurePath: "/wt/task-clock", ensureBranch: "implement/task-clock-aabb"}
	deps := implementer.Deps{
		DB:       db,
		Worktree: wt,
		Spawn: func(ctx context.Context, args implementer.GnhfArgs) (implementer.GnhfResult, error) {
			// Simulate spawn-elapsed time by advancing the clock once during Spawn.
			// (In production this is real elapsed wall time across SpawnGnhf.)
			_ = clock()
			return implementer.GnhfResult{
				Status:       implementer.StatusStopped,
				Reason:       implementer.ReasonStopWhen,
				CommitCount:  1,
				InputTokens:  100,
				OutputTokens: 50,
			}, nil
		},
		RepoPath:  "/repo",
		Now:       clock,
		JitterMin: 0,
	}

	row := makeRow("c-clock", "task-clock", "impl clock task", "autonomous")
	if err := implementer.DispatchImplement(context.Background(), row, deps); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	if len(db.insertedRuns) != 1 {
		t.Fatalf("expected 1 inserted run, got %d", len(db.insertedRuns))
	}
	if len(db.finalizedRuns) != 1 {
		t.Fatalf("expected 1 finalized run, got %d", len(db.finalizedRuns))
	}
	startedAt := db.insertedRuns[0].StartedAt    // seconds
	finishedAt := db.finalizedRuns[1].FinishedAt // milliseconds

	// finished_at (ms) must be strictly greater than started_at (s) converted
	// to ms — i.e., the dispatcher captured a FRESH timestamp after Spawn.
	startedAtMs := startedAt * 1000
	if finishedAt <= startedAtMs {
		t.Errorf("finished_at must be after started_at: started_at=%d s (%d ms), finished_at=%d ms — same `now` reused?",
			startedAt, startedAtMs, finishedAt)
	}
}

// TestDispatchImplement_RecoverFromCompletedCrash verifies case B of the
// idempotency guard (codex round-2 #1): when a prior implementer_runs row
// is finalized (finished_at != nil) but the supervisor crashed before
// MarkInboxProcessed, the dispatcher must skip respawn, reformat-and-repost
// from the persisted telemetry, and mark the inbox row processed.
//
// Codex round-3 #1 sub-case: this test seeds the outbox marker with
// lark_comment_id="" (the post never happened — operator is in the case-B
// crash window). We assert PostComment IS called and OutboxMarkPosted
// records the returned lark_comment_id.
func TestDispatchImplement_RecoverFromCompletedCrash(t *testing.T) {
	nightEnvOn(t)

	db := newFakeDB()
	finishedAt := int64(1746000000000) // arbitrary finalized timestamp (ms)
	db.seedImplementerRun = &sqlite.ImplementerRun{
		ID:               42,
		InboxCommentID:   "c-recoverB",
		TaskID:           "task-recoverB",
		StartedAt:        1746000000,
		FinishedAt:       &finishedAt,
		Outcome:          "success",
		BranchName:       "implement/task-recoverB-aabb",
		PRURL:            "https://github.com/example/repo/pull/77",
		NotesMDExcerpt:   "all good",
		GnhfStatus:       "stopped",
		GnhfReason:       "stop_when",
		GnhfIterations:   4,
		GnhfCommitsMade:  2,
		GnhfSuccessCount: 4,
		GnhfFailCount:    0,
		GnhfInputTokens:  300,
		GnhfOutputTokens: 150,
		GnhfRunID:        "run-recovered-b",
		GnhfNoProgress:   false,
		GnhfLastMessage:  "stop condition met",
	}
	// Seed outbox marker WITHOUT lark_comment_id — the prior run inserted the
	// marker but crashed before PostComment / OutboxMarkPosted (case B).
	db.outboxLarkIDs["c-recoverB:implement"] = ""

	wt := &fakeWorktree{ensurePath: "/wt/should-not-be-called", ensureBranch: "should/not"}
	lc := &fakeLarkClient{returnID: "lark-c-recoverB"}

	spawnCalled := false
	deps := implementer.Deps{
		DB:       db,
		Worktree: wt,
		Spawn: func(ctx context.Context, args implementer.GnhfArgs) (implementer.GnhfResult, error) {
			spawnCalled = true
			return implementer.GnhfResult{}, nil
		},
		LarkClient: lc,
		RepoPath:   "/repo",
		Now:        time.Now,
		JitterMin:  0,
	}
	row := makeRow("c-recoverB", "task-recoverB", "impl recoverB task", "autonomous")

	if err := implementer.DispatchImplement(context.Background(), row, deps); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	// Spawn must NOT have been called — recovery, not respawn.
	if spawnCalled {
		t.Error("Spawn must not be called when prior finalized row exists (case B)")
	}
	// No new run row inserted.
	if len(db.insertedRuns) != 0 {
		t.Errorf("expected 0 InsertImplementerRun calls (recovery skips insert), got %d",
			len(db.insertedRuns))
	}
	// No new finalize either — the existing row stays.
	if len(db.finalizedRuns) != 0 {
		t.Errorf("expected 0 FinalizeImplementerRun calls (case B does not re-finalize), got %d",
			len(db.finalizedRuns))
	}

	// MarkInboxProcessed must have happened.
	if len(db.processedCalls) != 1 {
		t.Errorf("expected MarkInboxProcessed called once, got %d", len(db.processedCalls))
	}

	lc.mu.Lock()
	calls := lc.calls
	lc.mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("expected 1 PostComment call (case-B re-post via OutboxCheck path), got %d", len(calls))
	}
	if !strings.Contains(calls[0].Content, "finished — stop-when condition met") {
		t.Errorf("PostComment content missing recovered headline, got: %q", calls[0].Content)
	}
	if !strings.Contains(calls[0].Content, "https://github.com/example/repo/pull/77") {
		t.Errorf("PostComment content missing recovered PR URL, got: %q", calls[0].Content)
	}

	// OutboxMarkPosted must have been called with the lark_comment_id returned
	// by PostComment, so a future replay sees lark_comment_id != "" and skips.
	if len(db.markPostedCalls) != 1 {
		t.Fatalf("expected 1 OutboxMarkPosted call after recovery post, got %d", len(db.markPostedCalls))
	}
	gotMP := db.markPostedCalls[0]
	if gotMP.Hash != "c-recoverB:implement" {
		t.Errorf("OutboxMarkPosted hash: got %q want %q", gotMP.Hash, "c-recoverB:implement")
	}
	if gotMP.LarkCommentID != "lark-c-recoverB" {
		t.Errorf("OutboxMarkPosted lark_comment_id: got %q want %q", gotMP.LarkCommentID, "lark-c-recoverB")
	}
}

// TestDispatchImplement_RecoverFromCompletedCrash_SkipsWhenLarkIDPresent
// verifies the OTHER case-B branch: a prior run finalized AND posted, but
// crashed before MarkInboxProcessed. On replay, OutboxCheck returns a
// non-empty lark_comment_id so the post is skipped (idempotency).
func TestDispatchImplement_RecoverFromCompletedCrash_SkipsWhenLarkIDPresent(t *testing.T) {
	nightEnvOn(t)

	db := newFakeDB()
	finishedAt := int64(1746000000000)
	db.seedImplementerRun = &sqlite.ImplementerRun{
		ID:             43,
		InboxCommentID: "c-recoverB-posted",
		TaskID:         "task-recoverB-posted",
		StartedAt:      1746000000,
		FinishedAt:     &finishedAt,
		Outcome:        "success",
		BranchName:     "implement/task-aabb",
		GnhfStatus:     "stopped",
		GnhfReason:     "stop_when",
	}
	// Seed the outbox marker WITH a non-empty lark_comment_id (post already done).
	db.outboxLarkIDs["c-recoverB-posted:implement"] = "lark-prior-success"

	wt := &fakeWorktree{ensurePath: "/wt/should-not-be-called", ensureBranch: "should/not"}
	lc := &fakeLarkClient{}

	spawnCalled := false
	deps := implementer.Deps{
		DB:       db,
		Worktree: wt,
		Spawn: func(ctx context.Context, args implementer.GnhfArgs) (implementer.GnhfResult, error) {
			spawnCalled = true
			return implementer.GnhfResult{}, nil
		},
		LarkClient: lc,
		RepoPath:   "/repo",
		Now:        time.Now,
		JitterMin:  0,
	}
	row := makeRow("c-recoverB-posted", "task-recoverB-posted", "impl recoverB-posted task", "autonomous")

	if err := implementer.DispatchImplement(context.Background(), row, deps); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	if spawnCalled {
		t.Error("Spawn must not be called (recovery)")
	}

	lc.mu.Lock()
	calls := lc.calls
	lc.mu.Unlock()
	if len(calls) != 0 {
		t.Errorf("PostComment must NOT be called when lark_comment_id already set, got %d calls", len(calls))
	}
	if len(db.markPostedCalls) != 0 {
		t.Errorf("OutboxMarkPosted must NOT be called when no post happened, got %d calls", len(db.markPostedCalls))
	}
	if len(db.processedCalls) != 1 {
		t.Errorf("MarkInboxProcessed must still be called, got %d", len(db.processedCalls))
	}
}

// TestDispatchImplement_RecoverFromInterruptedCrash verifies case C of the
// idempotency guard: a prior row started but never finalized (finished_at
// == nil). The dispatcher must NOT respawn (worktree may be partial),
// must finalize the EXISTING row with a synthetic failure outcome, post a
// "supervisor crashed" summary, and mark the inbox processed.
func TestDispatchImplement_RecoverFromInterruptedCrash(t *testing.T) {
	nightEnvOn(t)

	db := newFakeDB()
	db.seedImplementerRun = &sqlite.ImplementerRun{
		ID:             99,
		InboxCommentID: "c-recoverC",
		TaskID:         "task-recoverC",
		StartedAt:      1746000000,
		FinishedAt:     nil, // interrupted — never finalized
		BranchName:     "implement/task-recoverC-aabb",
	}

	wt := &fakeWorktree{ensurePath: "/wt/should-not-be-called", ensureBranch: "should/not"}
	lc := &fakeLarkClient{}

	spawnCalled := false
	deps := implementer.Deps{
		DB:       db,
		Worktree: wt,
		Spawn: func(ctx context.Context, args implementer.GnhfArgs) (implementer.GnhfResult, error) {
			spawnCalled = true
			return implementer.GnhfResult{}, nil
		},
		LarkClient: lc,
		RepoPath:   "/repo",
		Now:        time.Now,
		JitterMin:  0,
	}
	row := makeRow("c-recoverC", "task-recoverC", "impl recoverC task", "autonomous")

	if err := implementer.DispatchImplement(context.Background(), row, deps); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	if spawnCalled {
		t.Error("Spawn must not be called when prior interrupted row exists (case C)")
	}
	// Recovery finalizes the EXISTING row (id=99), no new insert.
	if len(db.insertedRuns) != 0 {
		t.Errorf("expected 0 InsertImplementerRun calls in case C, got %d", len(db.insertedRuns))
	}
	fin, ok := db.finalizedRuns[99]
	if !ok {
		t.Fatalf("expected case-C finalize on existing run id=99, got finalizedRuns=%v", db.finalizedRuns)
	}
	if fin.Outcome != "failed" {
		t.Errorf("case C outcome: got %q want failed", fin.Outcome)
	}
	if !strings.Contains(fin.Error, "supervisor crashed") {
		t.Errorf("case C error must mention supervisor crash, got %q", fin.Error)
	}
	if fin.GnhfStatus != "aborted" {
		t.Errorf("case C gnhf_status: got %q want aborted", fin.GnhfStatus)
	}
	if fin.FinishedAt == 0 {
		t.Error("case C finished_at must be set by recovery path")
	}

	assertOutboxMarker(t, db, row)
	if len(db.processedCalls) != 1 {
		t.Errorf("expected MarkInboxProcessed called once, got %d", len(db.processedCalls))
	}

	lc.mu.Lock()
	calls := lc.calls
	lc.mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("expected 1 PostComment call, got %d", len(calls))
	}
	if !strings.Contains(calls[0].Content, "aborted — see notes for context") {
		t.Errorf("case C summary should reflect aborted/unknown headline, got: %q", calls[0].Content)
	}
	if !strings.Contains(calls[0].Content, "log incomplete") {
		t.Errorf("case C summary should include log-incomplete suffix, got: %q", calls[0].Content)
	}
}

// TestDispatchImplement_LookupErrorDefers verifies codex round-3 #2: a
// transient DB failure in GetImplementerRunByCommentID must NOT silently
// fall through to a fresh-start respawn (which could double-spawn gnhf
// once the DB recovers and the prior row becomes visible). Instead the
// dispatcher MarkDeferred-s the row with a future timestamp and returns nil.
func TestDispatchImplement_LookupErrorDefers(t *testing.T) {
	nightEnvOn(t)

	db := newFakeDB()
	db.lookupErr = fmt.Errorf("database is locked")

	wt := &fakeWorktree{ensurePath: "/wt/should-not-be-called", ensureBranch: "should/not"}
	lc := &fakeLarkClient{}

	spawnCalled := false
	// Use a fixed clock so the deferUntil assertion is deterministic.
	fixedNow := time.Date(2026, 4, 28, 22, 0, 0, 0, time.UTC)
	deps := implementer.Deps{
		DB:       db,
		Worktree: wt,
		Spawn: func(ctx context.Context, args implementer.GnhfArgs) (implementer.GnhfResult, error) {
			spawnCalled = true
			return implementer.GnhfResult{}, nil
		},
		LarkClient: lc,
		RepoPath:   "/repo",
		Now:        func() time.Time { return fixedNow },
		JitterMin:  0,
	}
	row := makeRow("c-lookuperr", "task-lookuperr", "impl lookuperr", "autonomous")

	err := implementer.DispatchImplement(context.Background(), row, deps)
	if err != nil {
		t.Fatalf("lookup error must not propagate (deferred), got: %v", err)
	}

	if spawnCalled {
		t.Error("Spawn must not be called on lookup error — risks double-spawn once DB recovers")
	}
	if len(db.insertedRuns) != 0 {
		t.Errorf("expected 0 InsertImplementerRun calls on lookup error, got %d", len(db.insertedRuns))
	}
	if len(db.deferredCalls) != 1 {
		t.Fatalf("expected 1 MarkDeferred call on lookup error, got %d", len(db.deferredCalls))
	}
	got := db.deferredCalls[0]
	if got.CommentID != row.CommentID {
		t.Errorf("MarkDeferred commentID: got %q want %q", got.CommentID, row.CommentID)
	}
	expectedDefer := fixedNow.Add(60 * time.Second).UnixMilli()
	if got.ScheduledFor != expectedDefer {
		t.Errorf("MarkDeferred scheduled_for: got %d want %d (now + 60s)", got.ScheduledFor, expectedDefer)
	}
	// MarkInboxProcessed must NOT be called — the row stays deferred.
	if len(db.processedCalls) != 0 {
		t.Errorf("MarkInboxProcessed must not be called on lookup error, got %d", len(db.processedCalls))
	}
}
