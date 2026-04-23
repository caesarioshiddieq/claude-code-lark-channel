package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
//
// This is redundant with the in-process busy-task map in worker.Pool —
// but flock is kept as defense in depth: (a) protects against a second
// supervisor binary accidentally running on the same VM, (b) protects
// against a human operator running `claude -p` directly against the
// session dir. Do not remove without first proving both cases cannot
// happen in production.
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

type claudeUsage struct {
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	CacheReadTokens     int `json:"cache_read_tokens"`
	CacheCreationTokens int `json:"cache_creation_tokens"`
}

type claudeOutput struct {
	IsError bool        `json:"is_error"`
	Result  string      `json:"result"`
	Usage   claudeUsage `json:"usage"`
}

// SpawnResult holds the reply and token telemetry from a claude -p invocation.
type SpawnResult struct {
	Reply               string
	InputTokens         int
	OutputTokens        int
	CacheReadTokens     int
	CacheCreationTokens int
	IsRateLimit         bool
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

var rateLimitRe = regexp.MustCompile(`(?i)rate.?limit|429|retry.?after`)

// ParseClaudeOutputWithUsage parses claude JSON output and optional stderr.
func ParseClaudeOutputWithUsage(raw []byte, stderr []byte) (SpawnResult, error) {
	var out claudeOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return SpawnResult{}, fmt.Errorf("claude output parse: %w", err)
	}
	r := SpawnResult{
		Reply:               out.Result,
		InputTokens:         out.Usage.InputTokens,
		OutputTokens:        out.Usage.OutputTokens,
		CacheReadTokens:     out.Usage.CacheReadTokens,
		CacheCreationTokens: out.Usage.CacheCreationTokens,
	}
	if len(stderr) > 0 && rateLimitRe.Match(stderr) {
		r.IsRateLimit = true
	}
	if out.IsError {
		if rateLimitRe.MatchString(out.Result) {
			r.IsRateLimit = true
		}
		return r, fmt.Errorf("claude error: %s", out.Result)
	}
	return r, nil
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

// SpawnClaudeWithUsage runs claude -p and returns reply + token usage + rate-limit flag.
func SpawnClaudeWithUsage(ctx context.Context, sessionUUID string, isNew bool, prompt string) (SpawnResult, error) {
	args := []string{"-p", "--output-format", "json"}
	if isNew {
		args = append(args, "--session-id", sessionUUID)
	} else {
		args = append(args, "--resume", sessionUUID)
	}
	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Stdin = strings.NewReader(prompt)
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	out, err := cmd.Output()
	if err != nil {
		return SpawnResult{IsRateLimit: rateLimitRe.Match(stderrBuf.Bytes())},
			fmt.Errorf("claude spawn: %w", err)
	}
	return ParseClaudeOutputWithUsage(out, stderrBuf.Bytes())
}
