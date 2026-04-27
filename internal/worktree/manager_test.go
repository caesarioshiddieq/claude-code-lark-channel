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

	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(stalePath, old, old); err != nil {
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
