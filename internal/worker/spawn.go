package worker

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

func lockDir() string {
	if d := os.Getenv("LOCK_DIR"); d != "" {
		return d
	}
	return "/var/lib/claude-vm/sessions"
}

// LockTask acquires an exclusive flock on the per-task lock file.
// Caller must call UnlockTask when done.
func LockTask(taskID string) (*os.File, error) {
	lockPath := filepath.Join(lockDir(), taskID, "lock")
	// Guard against path traversal: ensure lockPath stays within lockDir()
	base := filepath.Clean(lockDir()) + string(os.PathSeparator)
	if !strings.HasPrefix(filepath.Clean(lockPath), base) {
		return nil, fmt.Errorf("invalid taskID %q: path escapes lock directory", taskID)
	}
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir lock dir: %w", err)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("flock: %w", err)
	}
	return f, nil
}

// UnlockTask releases the flock and closes the lock file.
func UnlockTask(f *os.File) {
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck
	f.Close()
}

// ContentHash returns sha256(taskID + NUL + reply) as hex.
// Used as outbox primary key to detect already-posted replies (Gate G1).
func ContentHash(taskID, reply string) string {
	h := sha256.Sum256([]byte(taskID + "\x00" + reply))
	return fmt.Sprintf("%x", h)
}

type claudeOutput struct {
	IsError bool   `json:"is_error"`
	Result  string `json:"result"`
}

// ParseClaudeOutput extracts the assistant reply from `claude -p --output-format json` stdout.
func ParseClaudeOutput(raw []byte) (string, error) {
	var out claudeOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("claude output parse: %w", err)
	}
	if out.IsError {
		return "", fmt.Errorf("claude error: %s", out.Result)
	}
	return out.Result, nil
}

// SpawnClaude runs `claude -p` and returns the assistant reply.
// isNew=true uses --session-id (first turn); false uses --resume.
func SpawnClaude(ctx context.Context, sessionUUID string, isNew bool, prompt string) (string, error) {
	args := []string{"-p", "--output-format", "json"}
	if isNew {
		args = append(args, "--session-id", sessionUUID)
	} else {
		args = append(args, "--resume", sessionUUID)
	}
	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Stdin = strings.NewReader(prompt)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("claude spawn: %w", err)
	}
	return ParseClaudeOutput(out)
}
