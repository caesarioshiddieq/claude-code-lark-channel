package worker_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/caesarioshiddieq/claude-code-lark-channel/internal/lark"
	"github.com/caesarioshiddieq/claude-code-lark-channel/internal/sqlite"
	"github.com/caesarioshiddieq/claude-code-lark-channel/internal/worker"
)

// ---- helpers ----

func newTestDB(t *testing.T) *sqlite.DB {
	t.Helper()
	db, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func insertRow(t *testing.T, db *sqlite.DB, commentID, taskID string, ts int64) {
	t.Helper()
	c := lark.Comment{
		CommentID: commentID,
		Content:   "test",
		CreatedAt: ts,
		Creator:   lark.Creator{ID: "U1"},
	}
	if err := db.InsertHumanInbox(context.Background(), taskID, c, "normal"); err != nil {
		t.Fatalf("insertRow %s: %v", commentID, err)
	}
}

func contains(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

type countingFetcher struct {
	db    *sqlite.DB
	calls atomic.Int32
}

func (c *countingFetcher) NextInboxRowExcluding(ctx context.Context, busy []string) (sqlite.InboxRow, bool, error) {
	c.calls.Add(1)
	return c.db.NextInboxRowExcluding(ctx, busy)
}

// ---- tests ----

// TestPool_GlobalCapRespected verifies at most N spawns run concurrently across all tasks.
func TestPool_GlobalCapRespected(t *testing.T) {
	defer goleak.VerifyNone(t)
	db := newTestDB(t)
	defer db.Close()
	const N = 2
	const numTasks = 10

	for i := 0; i < numTasks; i++ {
		insertRow(t, db, fmt.Sprintf("C%d", i+1), fmt.Sprintf("task-%d", i+1), int64(i+1))
	}

	var peak, current atomic.Int32
	var doneWg sync.WaitGroup
	doneWg.Add(numTasks)

	process := func(ctx context.Context, row sqlite.InboxRow) {
		cur := current.Add(1)
		for {
			old := peak.Load()
			if cur <= old || peak.CompareAndSwap(old, cur) {
				break
			}
		}
		db.MarkInboxProcessed(context.Background(), row.CommentID)
		time.Sleep(80 * time.Millisecond)
		current.Add(-1)
		doneWg.Done()
	}

	ctx, cancel := context.WithCancel(context.Background())
	pool := worker.NewPool(worker.ProcessDeps{DB: db, Process: process}, N)
	runDone := make(chan struct{})
	go func() { pool.Run(ctx); close(runDone) }()

	doneWg.Wait()
	cancel()

	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("pool.Run did not stop")
	}

	if got := peak.Load(); got > N {
		t.Errorf("peak concurrent=%d exceeds N=%d", got, N)
	}
}

// TestPool_BusyTaskExcluded verifies a task's second row is not dispatched while its first is in-flight.
func TestPool_BusyTaskExcluded(t *testing.T) {
	defer goleak.VerifyNone(t)
	db := newTestDB(t)
	defer db.Close()
	insertRow(t, db, "C1", "task-a", 1) // task-a row 1 (dispatched first)
	insertRow(t, db, "C3", "task-b", 2) // task-b row (should run while task-a is busy)
	insertRow(t, db, "C2", "task-a", 3) // task-a row 2 (must wait for C1 to finish)

	c1Started := make(chan struct{}, 1)
	release := make(chan struct{})
	var c1End, c2Start time.Time
	var mu sync.Mutex
	var doneWg sync.WaitGroup
	doneWg.Add(3)

	process := func(ctx context.Context, row sqlite.InboxRow) {
		defer func() {
			db.MarkInboxProcessed(context.Background(), row.CommentID)
			doneWg.Done()
		}()
		if row.CommentID == "C2" {
			mu.Lock()
			c2Start = time.Now()
			mu.Unlock()
		}
		if row.CommentID == "C1" {
			c1Started <- struct{}{}
			<-release
			mu.Lock()
			c1End = time.Now()
			mu.Unlock()
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	pool := worker.NewPool(worker.ProcessDeps{DB: db, Process: process}, 3)
	runDone := make(chan struct{})
	go func() { pool.Run(ctx); close(runDone) }()

	<-c1Started
	close(release)

	doneWg.Wait()
	cancel()

	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("pool.Run did not stop")
	}

	if c2Start.IsZero() || c1End.IsZero() {
		t.Fatal("timing was not recorded")
	}
	if !c2Start.After(c1End) {
		t.Errorf("C2 (task-a row 2) started at %v before C1 ended at %v — per-task exclusion broken",
			c2Start, c1End)
	}
}

// TestPool_NoDeadlock_NPlus1Rows ensures N+1 rows across N+1 distinct tasks all complete (codex deadlock scenario).
func TestPool_NoDeadlock_NPlus1Rows(t *testing.T) {
	defer goleak.VerifyNone(t)
	db := newTestDB(t)
	defer db.Close()
	const N = 2
	const spawnDuration = 30 * time.Millisecond

	insertRow(t, db, "C1", "task-1", 1)
	insertRow(t, db, "C2", "task-2", 2)
	insertRow(t, db, "C3", "task-3", 3)

	var completed atomic.Int32
	var doneWg sync.WaitGroup
	doneWg.Add(3)

	process := func(ctx context.Context, row sqlite.InboxRow) {
		db.MarkInboxProcessed(context.Background(), row.CommentID)
		time.Sleep(spawnDuration)
		completed.Add(1)
		doneWg.Done()
	}

	ctx, cancel := context.WithCancel(context.Background())
	pool := worker.NewPool(worker.ProcessDeps{DB: db, Process: process}, N)
	runDone := make(chan struct{})
	go func() { pool.Run(ctx); close(runDone) }()

	doneWg.Wait()
	cancel()

	select {
	case <-runDone:
	case <-time.After(2*spawnDuration + 2*time.Second):
		t.Fatal("pool deadlocked or did not finish N+1 rows in time")
	}

	if got := completed.Load(); got != 3 {
		t.Errorf("want 3 completed, got %d", got)
	}
}

// TestPool_GracefulShutdown verifies Run waits for in-flight jobs before returning on ctx cancel.
func TestPool_GracefulShutdown(t *testing.T) {
	defer goleak.VerifyNone(t)
	db := newTestDB(t)
	defer db.Close()
	insertRow(t, db, "C1", "task-a", 1)
	insertRow(t, db, "C2", "task-b", 2)

	var started sync.WaitGroup
	started.Add(2)
	var processed atomic.Int32

	process := func(ctx context.Context, row sqlite.InboxRow) {
		processed.Add(1)
		started.Done()
		<-ctx.Done()
	}

	ctx, cancel := context.WithCancel(context.Background())
	pool := worker.NewPool(worker.ProcessDeps{DB: db, Process: process}, 2)
	runDone := make(chan struct{})
	go func() { pool.Run(ctx); close(runDone) }()

	started.Wait()
	cancel()

	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("pool.Run did not stop after ctx cancel")
	}

	if got := processed.Load(); got != 2 {
		t.Errorf("want 2 processed, got %d", got)
	}
}

// TestPool_PanicRecovery verifies a panic in Process is recovered, busy is cleared, and the next
// row for the same task dispatches normally.
func TestPool_PanicRecovery(t *testing.T) {
	defer goleak.VerifyNone(t)
	db := newTestDB(t)
	defer db.Close()
	insertRow(t, db, "C1", "task-a", 1)
	insertRow(t, db, "C2", "task-a", 2)

	var callCount, completed atomic.Int32
	doneCh := make(chan struct{})

	process := func(ctx context.Context, row sqlite.InboxRow) {
		// Mark processed before panicking so C1 is not re-picked after recovery.
		db.MarkInboxProcessed(context.Background(), row.CommentID)
		n := callCount.Add(1)
		if n == 1 {
			panic("intentional test panic")
		}
		completed.Add(1)
		close(doneCh)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool := worker.NewPool(worker.ProcessDeps{DB: db, Process: process}, 1)
	runDone := make(chan struct{})
	go func() { pool.Run(ctx); close(runDone) }()

	select {
	case <-doneCh:
	case <-time.After(5 * time.Second):
		t.Fatal("C2 did not complete after C1 panic recovery")
	}

	cancel()
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("pool.Run did not stop after ctx cancel")
	}

	if got := completed.Load(); got != 1 {
		t.Errorf("want 1 completed (C2), got %d", got)
	}
	if got := callCount.Load(); got != 2 {
		t.Errorf("want 2 calls (C1 panic + C2 success), got %d", got)
	}
}

// TestPool_NoBusyLoopWhenEmpty verifies the pool sleeps between polls when the inbox is empty.
func TestPool_NoBusyLoopWhenEmpty(t *testing.T) {
	defer goleak.VerifyNone(t)
	db := newTestDB(t)
	defer db.Close()
	cf := &countingFetcher{db: db}

	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()

	pool := worker.NewPool(worker.ProcessDeps{
		DB:      cf,
		Process: func(ctx context.Context, row sqlite.InboxRow) {},
	}, 1)
	pool.Run(ctx)

	// 500ms idle sleep → ~5 polls in 2.5s; allow 7 for scheduling jitter.
	if n := cf.calls.Load(); n > 7 {
		t.Errorf("too many DB polls: %d (want ≤ 7 for 500ms idle sleep over 2.5s)", n)
	}
}

// TestPool_FastFailBackoff verifies the pool does not tight-spin on a row whose Process
// returns fast without marking it processed, as long as Process honours the fast-fail contract
// (calling MarkDeferred with a future scheduled_for). Models the real-world fix where
// processOne defers the row on LockTask / resolveSession errors so the dispatcher's next
// NextInboxRowExcluding call skips it via the scheduled_for <= now SQL filter.
func TestPool_FastFailBackoff(t *testing.T) {
	defer goleak.VerifyNone(t)
	db := newTestDB(t)
	defer db.Close()
	insertRow(t, db, "C1", "task-a", 1)

	var calls atomic.Int32
	process := func(ctx context.Context, row sqlite.InboxRow) {
		calls.Add(1)
		// Simulate processOne's fast-fail path: defer ~30s, do NOT mark processed.
		backoff := time.Now().Add(30 * time.Second).UnixMilli()
		if err := db.MarkDeferred(context.Background(), row.CommentID, backoff, row.Content); err != nil {
			t.Errorf("MarkDeferred: %v", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	pool := worker.NewPool(worker.ProcessDeps{DB: db, Process: process}, 1)
	pool.Run(ctx)

	// With the backoff applied, the row should not be re-picked within the 2s window.
	// Without the fix, the pool would re-pick the same row at CPU speed (hundreds of calls).
	if n := calls.Load(); n != 1 {
		t.Errorf("Process called %d times; want exactly 1 (tight-spin regression?)", n)
	}
}

// TestPool_ShutdownWhileBlockedOnSem verifies that when the dispatcher is blocked on the semaphore
// at ctx cancel, it removes the claimed-but-not-dispatched task from the busy map.
func TestPool_ShutdownWhileBlockedOnSem(t *testing.T) {
	defer goleak.VerifyNone(t)
	db := newTestDB(t)
	defer db.Close()
	insertRow(t, db, "C1", "task-a", 1)
	insertRow(t, db, "C2", "task-b", 2)

	release := make(chan struct{})
	var bProcessed atomic.Bool

	process := func(ctx context.Context, row sqlite.InboxRow) {
		if row.TaskID == "task-b" {
			bProcessed.Store(true)
			return
		}
		<-release
		db.MarkInboxProcessed(context.Background(), row.CommentID)
	}

	ctx, cancel := context.WithCancel(context.Background())
	pool := worker.NewPool(worker.ProcessDeps{DB: db, Process: process}, 1)
	runDone := make(chan struct{})
	go func() { pool.Run(ctx); close(runDone) }()

	// Wait until task-a is in-flight (sem full) AND task-b is claimed (dispatcher blocked on sem).
	deadline := time.Now().Add(2 * time.Second)
	for {
		snap := pool.BusySnapshot()
		if contains(snap, "task-a") && contains(snap, "task-b") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out: expected both task-a and task-b in busy map")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// sem is full → dispatcher's sem-select has only ctx.Done ready; no race with sem firing.
	cancel()

	// Wait until the ctx.Done branch in the dispatcher actually cleaned up task-b
	// from the busy map before unblocking task-a. Without this, there's a scheduler
	// race: if the sem slot freed by task-a's goroutine becomes ready at the same
	// instant as ctx.Done, the dispatcher might dispatch task-b before seeing ctx.Done.
	cleanupDeadline := time.Now().Add(time.Second)
	for contains(pool.BusySnapshot(), "task-b") {
		if time.Now().After(cleanupDeadline) {
			t.Fatal("dispatcher did not clean up task-b from busy map after ctx cancel")
		}
		time.Sleep(5 * time.Millisecond)
	}

	close(release) // unblock task-a so wg.Wait() in Run can complete

	select {
	case <-runDone:
	case <-time.After(3 * time.Second):
		t.Fatal("pool.Run did not return after ctx cancel and release")
	}

	snap := pool.BusySnapshot()
	if contains(snap, "task-b") {
		t.Error("task-b should have been removed from busy map by the ctx.Done cleanup branch")
	}
	if bProcessed.Load() {
		t.Error("task-b Process should not have been called")
	}
}
