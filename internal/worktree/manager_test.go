package worktree_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caesarioshiddieq/claude-code-lark-channel/internal/worktree"
)

// initTestRepo creates an ephemeral git repo in t.TempDir() with a single
// initial commit and returns the repo path.
func initTestRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	steps := [][]string{
		{"git", "init", "-q", "-b", "main"},
		{"git", "config", "user.name", "test"},
		{"git", "config", "user.email", "test@example.com"},
		{"git", "commit", "--allow-empty", "-q", "-m", "initial"},
	}
	for _, args := range steps {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v failed: %v\n%s", args, err, out)
		}
	}
	return repo
}

func TestBaseDir_Default(t *testing.T) {
	t.Setenv("IMPLEMENTER_WORKTREE_BASE", "")
	if got := worktree.BaseDir(); got != "/var/lib/claude-vm/worktrees" {
		t.Errorf("default BaseDir: got %q want /var/lib/claude-vm/worktrees", got)
	}
}

func TestBaseDir_EnvOverride(t *testing.T) {
	t.Setenv("IMPLEMENTER_WORKTREE_BASE", "/tmp/custom-base-12345")
	if got := worktree.BaseDir(); got != "/tmp/custom-base-12345" {
		t.Errorf("env-override BaseDir: got %q want /tmp/custom-base-12345", got)
	}
}

func TestEnsureForTask_CreatesAndIsIdempotent(t *testing.T) {
	repo := initTestRepo(t)
	base := t.TempDir()
	m := worktree.New(repo, base)

	ctx := context.Background()
	wt1, branch1, err := m.EnsureForTask(ctx, "task-1")
	if err != nil {
		t.Fatalf("first EnsureForTask: %v", err)
	}
	wantPath := filepath.Join(base, "task-1")
	if wt1 != wantPath {
		t.Errorf("path: got %q want %q", wt1, wantPath)
	}
	if !strings.HasPrefix(branch1, "implement/task-1-") {
		t.Errorf("branch: got %q want prefix implement/task-1-", branch1)
	}
	if fi, err := os.Stat(wt1); err != nil || !fi.IsDir() {
		t.Errorf("worktree dir not created: err=%v", err)
	}

	wt2, branch2, err := m.EnsureForTask(ctx, "task-1")
	if err != nil {
		t.Fatalf("second EnsureForTask: %v", err)
	}
	if wt1 != wt2 {
		t.Errorf("non-idempotent path: %q -> %q", wt1, wt2)
	}
	if branch1 != branch2 {
		t.Errorf("non-idempotent branch: %q -> %q", branch1, branch2)
	}
}

func TestCleanup_FailureRemovesWorktree(t *testing.T) {
	repo := initTestRepo(t)
	base := t.TempDir()
	m := worktree.New(repo, base)
	ctx := context.Background()

	wtPath, _, err := m.EnsureForTask(ctx, "task-fail")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Cleanup(ctx, "task-fail", false); err != nil {
		t.Fatalf("Cleanup failure-path: %v", err)
	}
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Errorf("worktree dir should be gone after failure cleanup: err=%v", err)
	}
}

func TestCleanup_SuccessPreservesWorktree(t *testing.T) {
	repo := initTestRepo(t)
	base := t.TempDir()
	m := worktree.New(repo, base)
	ctx := context.Background()

	wtPath, _, err := m.EnsureForTask(ctx, "task-ok")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Cleanup(ctx, "task-ok", true); err != nil {
		t.Fatalf("Cleanup success-path: %v", err)
	}
	if fi, err := os.Stat(wtPath); err != nil || !fi.IsDir() {
		t.Errorf("worktree dir should be preserved on success: err=%v", err)
	}
}

func TestGarbageCollect_RemovesStaleDirs(t *testing.T) {
	repo := initTestRepo(t)
	base := t.TempDir()
	m := worktree.New(repo, base)
	ctx := context.Background()

	freshPath, _, err := m.EnsureForTask(ctx, "task-fresh")
	if err != nil {
		t.Fatal(err)
	}
	stalePath, _, err := m.EnsureForTask(ctx, "task-stale")
	if err != nil {
		t.Fatal(err)
	}

	// Backdate stalePath AND every child recursively. GC walks children
	// for newest mtime (P1 fix), so backdating only the parent leaves
	// fresh children that vote "preserve."
	old := time.Now().Add(-48 * time.Hour)
	if err := filepath.Walk(stalePath, func(path string, _ os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		_ = os.Chtimes(path, old, old)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := m.GarbageCollect(ctx, 24*time.Hour); err != nil {
		t.Fatalf("GarbageCollect: %v", err)
	}

	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Errorf("stale dir should be removed by GC: err=%v", err)
	}
	if _, err := os.Stat(freshPath); err != nil {
		t.Errorf("fresh dir should survive GC: err=%v", err)
	}
}

// TestEnsureForTask_ConcurrentSameTaskID exercises the per-taskID concurrency
// invariant: two goroutines calling EnsureForTask on the same taskID must
// both succeed and observe the same (path, branch) — no TOCTOU race where
// the second `git worktree add` collides with the first.
func TestEnsureForTask_ConcurrentSameTaskID(t *testing.T) {
	repo := initTestRepo(t)
	base := t.TempDir()
	m := worktree.New(repo, base)
	ctx := context.Background()

	const N = 10
	type result struct {
		path, branch string
		err          error
	}
	results := make(chan result, N)
	for i := 0; i < N; i++ {
		go func() {
			p, b, err := m.EnsureForTask(ctx, "task-conc")
			results <- result{p, b, err}
		}()
	}
	var first result
	for i := 0; i < N; i++ {
		r := <-results
		if r.err != nil {
			t.Errorf("goroutine %d: %v", i, r.err)
			continue
		}
		if i == 0 {
			first = r
			continue
		}
		if r.path != first.path {
			t.Errorf("path divergence: %q vs %q", r.path, first.path)
		}
		if r.branch != first.branch {
			t.Errorf("branch divergence: %q vs %q", r.branch, first.branch)
		}
	}
}

// TestEnsureForTask_StaleNonWorktreeDirRecovers exercises the stale-dir
// recovery invariant: if base/<taskID> exists but git doesn't know about it
// as a worktree (e.g. a half-failed prior run, or someone rm -rf'd .git),
// EnsureForTask must remove the orphan and create a fresh worktree rather
// than silently returning an unhealthy "existing" path.
func TestEnsureForTask_StaleNonWorktreeDirRecovers(t *testing.T) {
	repo := initTestRepo(t)
	base := t.TempDir()
	m := worktree.New(repo, base)
	ctx := context.Background()

	stale := filepath.Join(base, "task-stale-recover")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "orphan.txt"), []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}

	wtPath, _, err := m.EnsureForTask(ctx, "task-stale-recover")
	if err != nil {
		t.Fatalf("EnsureForTask should recover from stale dir: %v", err)
	}
	if wtPath != stale {
		t.Errorf("path: got %q want %q", wtPath, stale)
	}
	if _, err := os.Stat(filepath.Join(stale, ".git")); err != nil {
		t.Errorf(".git marker missing after recovery: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stale, "orphan.txt")); !os.IsNotExist(err) {
		t.Errorf("orphan.txt should have been cleaned: stat err=%v", err)
	}
}

// TestGarbageCollect_RespectsChildMTime exercises the active-worktree
// preservation invariant: a worktree whose top-level dir has an old mtime
// but contains a recently-modified child file (simulating an actively
// running gnhf process writing nested files) must NOT be garbage-collected.
// Otherwise a long-running implementer run could be killed mid-flight.
func TestGarbageCollect_RespectsChildMTime(t *testing.T) {
	repo := initTestRepo(t)
	base := t.TempDir()
	m := worktree.New(repo, base)
	ctx := context.Background()

	wtPath, _, err := m.EnsureForTask(ctx, "task-active")
	if err != nil {
		t.Fatal(err)
	}

	// Backdate the parent dir mtime to "old". Children created by
	// git worktree add (.git file, etc.) keep their fresh mtime, so the
	// only signal of "old" is the parent itself — exactly the buggy
	// scenario where a long-running gnhf modifies existing files
	// without touching the directory entry list.
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(wtPath, old, old); err != nil {
		t.Fatal(err)
	}

	if err := m.GarbageCollect(ctx, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wtPath); err != nil {
		t.Errorf("active worktree (with fresh children) should survive GC: %v", err)
	}
}
