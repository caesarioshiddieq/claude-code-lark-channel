// Package worktree provides per-task git-worktree lifecycle management for
// the autonomous implementer subsystem. Each Lark task that classifies as
// PhaseImplement is materialized as an isolated worktree under BaseDir(),
// branched off the parent supervisor checkout's HEAD. The worktree is
// preserved on success (gnhf commits + PR opening can inspect it later) and
// removed on failure (avoids unbounded disk growth from one-shot retries).
//
// API shape deviates from the plan's free-function sketch in one place: a
// Manager struct retains the parent repoPath, because Cleanup needs it to
// run `git worktree remove --force`. BaseDir() stays as a free function —
// env lookup is naturally stateless. See
// docs/superpowers/plans/2026-04-27-autonomous-implementer-plan.md Task 3.
package worktree

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	envBaseDir     = "IMPLEMENTER_WORKTREE_BASE"
	defaultBaseDir = "/var/lib/claude-vm/worktrees"
)

// BaseDir returns the configured worktree base directory. Reads
// IMPLEMENTER_WORKTREE_BASE; falls back to /var/lib/claude-vm/worktrees.
func BaseDir() string {
	if v := os.Getenv(envBaseDir); v != "" {
		return v
	}
	return defaultBaseDir
}

// Manager owns the lifecycle of per-task worktrees rooted at base, branched
// from the repo at repoPath.
type Manager struct {
	repoPath string
	base     string
}

// New creates a Manager bound to a parent git repo at repoPath and a
// worktree base directory. If base is "", BaseDir() is used.
func New(repoPath, base string) *Manager {
	if base == "" {
		base = BaseDir()
	}
	return &Manager{repoPath: repoPath, base: base}
}

// EnsureForTask returns (worktreePath, branchName, nil) for the given
// taskID, lazily creating both. Idempotent: if base/<taskID> already exists
// and is a healthy worktree, returns the existing path and the branch name
// read from `git rev-parse --abbrev-ref HEAD` inside it.
//
// Branch naming: implement/<taskID>-<8hex>. The 8-hex suffix prevents
// branch collision when a task is retried after a force cleanup.
func (m *Manager) EnsureForTask(ctx context.Context, taskID string) (string, string, error) {
	wtPath := filepath.Join(m.base, taskID)
	if fi, err := os.Stat(wtPath); err == nil && fi.IsDir() {
		cmd := exec.CommandContext(ctx, "git", "-C", wtPath, "rev-parse", "--abbrev-ref", "HEAD")
		out, err := cmd.Output()
		if err != nil {
			return "", "", fmt.Errorf("read existing worktree branch at %q: %w", wtPath, err)
		}
		return wtPath, strings.TrimSpace(string(out)), nil
	}

	if err := os.MkdirAll(m.base, 0o755); err != nil {
		return "", "", fmt.Errorf("mkdir worktree base %q: %w", m.base, err)
	}

	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return "", "", fmt.Errorf("generate branch suffix: %w", err)
	}
	branch := fmt.Sprintf("implement/%s-%s", taskID, hex.EncodeToString(suffix))

	cmd := exec.CommandContext(ctx, "git", "-C", m.repoPath, "worktree", "add", "-b", branch, wtPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", "", fmt.Errorf("git worktree add %q -> %q: %w (output: %s)", branch, wtPath, err, out)
	}
	return wtPath, branch, nil
}

// Cleanup removes the worktree for a task on failure; preserves it on
// success so gnhf-produced commits remain inspectable. No-op on a missing
// worktree (idempotent).
func (m *Manager) Cleanup(ctx context.Context, taskID string, success bool) error {
	if success {
		return nil
	}
	wtPath := filepath.Join(m.base, taskID)
	if _, err := os.Stat(wtPath); os.IsNotExist(err) {
		return nil
	}

	// Best-effort: let git remove its worktree metadata, then rm -rf
	// guarantees the directory is gone even if git lost track of it.
	_ = exec.CommandContext(ctx, "git", "-C", m.repoPath, "worktree", "remove", "--force", wtPath).Run()
	if err := os.RemoveAll(wtPath); err != nil {
		return fmt.Errorf("remove worktree dir %q: %w", wtPath, err)
	}
	_ = exec.CommandContext(ctx, "git", "-C", m.repoPath, "worktree", "prune").Run()
	return nil
}

// GarbageCollect removes any subdirectory of base whose mtime is older than
// olderThan. Best-effort prune of dangling git-worktree metadata follows.
func (m *Manager) GarbageCollect(ctx context.Context, olderThan time.Duration) error {
	entries, err := os.ReadDir(m.base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("readdir %q: %w", m.base, err)
	}
	cutoff := time.Now().Add(-olderThan)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(m.base, e.Name())
		fi, err := os.Stat(path)
		if err != nil {
			continue
		}
		if fi.ModTime().Before(cutoff) {
			_ = exec.CommandContext(ctx, "git", "-C", m.repoPath, "worktree", "remove", "--force", path).Run()
			_ = os.RemoveAll(path)
		}
	}
	_ = exec.CommandContext(ctx, "git", "-C", m.repoPath, "worktree", "prune").Run()
	return nil
}
