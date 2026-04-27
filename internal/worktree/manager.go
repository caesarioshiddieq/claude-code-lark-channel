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
//
// Concurrency: per-taskID mutex map serializes EnsureForTask and Cleanup
// for the same taskID so concurrent dispatch (Task 5) cannot race through
// `git worktree add` for the same path. Different taskIDs proceed in
// parallel.
package worktree

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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
// from the repo at repoPath. Safe for concurrent use across distinct
// taskIDs; serializes operations on the same taskID.
type Manager struct {
	repoPath string
	base     string

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// New creates a Manager bound to a parent git repo at repoPath and a
// worktree base directory. If base is "", BaseDir() is used.
func New(repoPath, base string) *Manager {
	if base == "" {
		base = BaseDir()
	}
	return &Manager{
		repoPath: repoPath,
		base:     base,
		locks:    make(map[string]*sync.Mutex),
	}
}

// lockFor returns a per-taskID mutex, lazily allocating it.
func (m *Manager) lockFor(taskID string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	if l, ok := m.locks[taskID]; ok {
		return l
	}
	l := &sync.Mutex{}
	m.locks[taskID] = l
	return l
}

// EnsureForTask returns (worktreePath, branchName, nil) for the given
// taskID, lazily creating both. Idempotent: if base/<taskID> already exists
// AND is registered as a real git worktree, returns the existing path and
// branch name. If the dir exists but is NOT a registered worktree (stale
// from a half-failed prior run, or .git removed manually), the dir is
// removed and a fresh worktree is created.
//
// Branch naming: implement/<taskID>-<8hex>. The 8-hex suffix prevents
// branch collision when a task is retried after a force cleanup.
func (m *Manager) EnsureForTask(ctx context.Context, taskID string) (string, string, error) {
	lock := m.lockFor(taskID)
	lock.Lock()
	defer lock.Unlock()

	wtPath := filepath.Join(m.base, taskID)
	if existingPath, branch, ok, err := m.probeExisting(ctx, wtPath); err != nil {
		return "", "", err
	} else if ok {
		return existingPath, branch, nil
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

// probeExisting inspects wtPath. Returns (path, branch, true, nil) when the
// dir exists and is a registered git worktree (caller can return as-is).
// Returns (_, _, false, nil) when no dir exists OR the dir was stale and
// has been removed (caller should proceed to fresh creation). Returns a
// non-nil error only on filesystem/git failures the caller cannot recover
// from.
func (m *Manager) probeExisting(ctx context.Context, wtPath string) (string, string, bool, error) {
	fi, err := os.Stat(wtPath)
	if os.IsNotExist(err) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("stat worktree path %q: %w", wtPath, err)
	}
	if !fi.IsDir() {
		return "", "", false, fmt.Errorf("worktree path %q exists but is not a directory", wtPath)
	}

	registered, err := m.isRegisteredWorktree(ctx, wtPath)
	if err != nil {
		return "", "", false, err
	}
	if !registered {
		// Stale dir — remove it so the fresh-creation path can proceed.
		if err := os.RemoveAll(wtPath); err != nil {
			return "", "", false, fmt.Errorf("remove stale dir %q: %w", wtPath, err)
		}
		return "", "", false, nil
	}

	out, err := exec.CommandContext(ctx, "git", "-C", wtPath, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return "", "", false, fmt.Errorf("read existing worktree branch at %q: %w", wtPath, err)
	}
	return wtPath, strings.TrimSpace(string(out)), true, nil
}

// isRegisteredWorktree reports whether wtPath appears in the parent repo's
// `git worktree list --porcelain` output.
func (m *Manager) isRegisteredWorktree(ctx context.Context, wtPath string) (bool, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", m.repoPath, "worktree", "list", "--porcelain").Output()
	if err != nil {
		return false, fmt.Errorf("git worktree list: %w", err)
	}
	want, err := filepath.Abs(wtPath)
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(line, "worktree ") {
			continue
		}
		got := strings.TrimPrefix(line, "worktree ")
		if abs, err := filepath.Abs(got); err == nil && abs == want {
			return true, nil
		}
	}
	return false, nil
}

// Cleanup removes the worktree for a task on failure; preserves it on
// success so gnhf-produced commits remain inspectable. No-op on a missing
// worktree (idempotent). Serialized with EnsureForTask via the per-taskID
// mutex.
func (m *Manager) Cleanup(ctx context.Context, taskID string, success bool) error {
	lock := m.lockFor(taskID)
	lock.Lock()
	defer lock.Unlock()

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

// GarbageCollect removes any subdirectory of base whose newest child mtime
// is older than olderThan. Walking children (instead of trusting the
// top-level dir mtime) avoids killing long-running implementer tasks whose
// gnhf process modifies existing files without bumping the directory's
// own mtime. Best-effort prune of dangling git-worktree metadata follows.
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
		newest, err := newestMTime(path)
		if err != nil {
			continue
		}
		if newest.Before(cutoff) {
			_ = exec.CommandContext(ctx, "git", "-C", m.repoPath, "worktree", "remove", "--force", path).Run()
			_ = os.RemoveAll(path)
		}
	}
	_ = exec.CommandContext(ctx, "git", "-C", m.repoPath, "worktree", "prune").Run()
	return nil
}

// newestMTime walks root recursively and returns the largest ModTime found
// across all entries (including root itself). Errors at intermediate
// entries are skipped — best-effort traversal is sufficient because the
// caller's only decision is "is anything here recent enough to keep the
// whole tree alive."
func newestMTime(root string) (time.Time, error) {
	var newest time.Time
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		return nil
	})
	return newest, walkErr
}
