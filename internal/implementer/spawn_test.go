package implementer_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caesarioshiddieq/claude-code-lark-channel/internal/implementer"
)

// initGitRepo initialises a bare git repo at dir (git init + initial commit).
// t.Setenv("HOME", t.TempDir()) isolates from gpg signing config.
func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	mustGit(t, dir, "init", "-b", "main")
	mustGit(t, dir, "config", "user.email", "test@test.test")
	mustGit(t, dir, "config", "user.name", "Test")
	mustGit(t, dir, "config", "commit.gpgsign", "false")
	// Need at least one commit so worktrees can branch off HEAD
	readme := filepath.Join(dir, "README")
	if err := os.WriteFile(readme, []byte("test"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "add", ".")
	mustGit(t, dir, "commit", "-m", "init")
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// addWorktree adds a linked worktree at wtPath on a new branch branchName.
func addWorktree(t *testing.T, repoDir, wtPath, branchName string) {
	t.Helper()
	mustGit(t, repoDir, "worktree", "add", "-b", branchName, wtPath)
}

// writeMockGnhf writes a shell script at binDir/gnhf. The mock creates
// .gnhf/runs/<runID>/gnhf.log under $PWD (the worktree cmd.Dir).
// extraSetup is injected before the log write; an empty string is fine.
func writeMockGnhf(t *testing.T, binDir, runID, logContent string, extraSetup string) {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/sh
set -e
RUN_DIR="$PWD/.gnhf/runs/%s"
mkdir -p "$RUN_DIR"
%s
printf '%%s' '%s' > "$RUN_DIR/gnhf.log"
exit 0
`, runID, extraSetup, logContent)
	gnhfPath := filepath.Join(binDir, "gnhf")
	if err := os.WriteFile(gnhfPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// makeCompleteLog builds a run:complete JSONL line matching the gnhf schema.
func makeCompleteLog(status, lastMessage string, iterations, commits int) string {
	return fmt.Sprintf(
		`{"event":"run:complete","status":%q,"iterations":%d,"successCount":1,"failCount":0,"totalInputTokens":100,"totalOutputTokens":50,"commitCount":%d,"worktreePath":"","lastMessage":%q}`,
		status, iterations, commits, lastMessage,
	)
}

// ---- Preflight failures ----

func TestSpawnGnhf_PreflightNonExistentPath(t *testing.T) {
	ctx := context.Background()
	args := implementer.GnhfArgs{
		WorktreePath: "/nonexistent/path/that/does/not/exist",
		Prompt:       "do something",
	}
	_, err := implementer.SpawnGnhf(ctx, args)
	if err == nil {
		t.Fatal("expected error for non-existent WorktreePath")
	}
}

func TestSpawnGnhf_PreflightNotAWorktree(t *testing.T) {
	dir := t.TempDir()
	// plain directory — not a git repo at all
	ctx := context.Background()
	args := implementer.GnhfArgs{
		WorktreePath: dir,
		Prompt:       "do something",
	}
	_, err := implementer.SpawnGnhf(ctx, args)
	if err == nil {
		t.Fatal("expected error for non-worktree directory")
	}
}

func TestSpawnGnhf_PreflightDetachedHEAD(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repoDir := t.TempDir()
	initGitRepo(t, repoDir)

	wtDir := t.TempDir()
	addWorktree(t, repoDir, wtDir, "implement/detach-test")

	// Detach HEAD in the worktree
	mustGit(t, wtDir, "checkout", "--detach")

	ctx := context.Background()
	args := implementer.GnhfArgs{
		WorktreePath: wtDir,
		Prompt:       "do something",
	}
	_, err := implementer.SpawnGnhf(ctx, args)
	if err == nil {
		t.Fatal("expected error for detached HEAD")
	}
}

func TestSpawnGnhf_PreflightBranchMismatch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repoDir := t.TempDir()
	initGitRepo(t, repoDir)

	wtDir := t.TempDir()
	addWorktree(t, repoDir, wtDir, "implement/actual-branch")

	ctx := context.Background()
	args := implementer.GnhfArgs{
		WorktreePath:   wtDir,
		ExpectedBranch: "implement/different-branch",
		Prompt:         "do something",
	}
	_, err := implementer.SpawnGnhf(ctx, args)
	if err == nil {
		t.Fatal("expected error for branch mismatch")
	}
}

// ---- Integration: happy path via shell-script mock ----

func TestSpawnGnhf_HappyPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repoDir := t.TempDir()
	initGitRepo(t, repoDir)

	wtDir := t.TempDir()
	addWorktree(t, repoDir, wtDir, "implement/happy")

	binDir := t.TempDir()
	runID := "run-happy-001"
	logContent := makeCompleteLog("stopped", "stop condition met: all tests pass", 5, 3)
	writeMockGnhf(t, binDir, runID, logContent, "")

	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	ctx := context.Background()
	args := implementer.GnhfArgs{
		WorktreePath:   wtDir,
		ExpectedBranch: "implement/happy",
		Prompt:         "implement the feature",
		MaxIterations:  10,
		StopWhen:       "all tests pass",
		Agent:          "claude",
		Timeout:        30 * time.Second,
		GracePeriod:    5 * time.Second,
	}
	r, err := implementer.SpawnGnhf(ctx, args)
	if err != nil {
		t.Fatalf("SpawnGnhf: unexpected error: %v", err)
	}
	if r.Status != implementer.StatusStopped {
		t.Errorf("Status: want stopped, got %q", r.Status)
	}
	if r.Reason != implementer.ReasonStopWhen {
		t.Errorf("Reason: want stop_when, got %q", r.Reason)
	}
	if r.CommitCount != 3 {
		t.Errorf("CommitCount: want 3, got %d", r.CommitCount)
	}
	if r.RunID == "" {
		t.Error("RunID: want non-empty")
	}
	if r.LogIncomplete {
		t.Error("LogIncomplete: want false")
	}
}

// ---- Default application ----

func TestSpawnGnhf_DefaultsApplied(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repoDir := t.TempDir()
	initGitRepo(t, repoDir)

	wtDir := t.TempDir()
	addWorktree(t, repoDir, wtDir, "implement/defaults")

	binDir := t.TempDir()
	runID := "run-defaults-001"
	// The mock captures its args to a file so we can verify defaults were applied.
	argsFile := filepath.Join(t.TempDir(), "gnhf-args.txt")
	extraSetup := fmt.Sprintf(`printf '%%s\n' "$@" > %q`, argsFile)
	logContent := makeCompleteLog("stopped", "stop condition met: done", 1, 1)
	writeMockGnhf(t, binDir, runID, logContent, extraSetup)
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	ctx := context.Background()
	// Zero-value MaxIterations, Timeout, GracePeriod, Agent → should use defaults
	args := implementer.GnhfArgs{
		WorktreePath: wtDir,
		Prompt:       "do it",
	}
	if _, err := implementer.SpawnGnhf(ctx, args); err != nil {
		t.Fatalf("SpawnGnhf: %v", err)
	}

	argsData, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read args file: %v", err)
	}
	argsStr := string(argsData)
	if !strings.Contains(argsStr, "30") {
		t.Errorf("expected default --max-iterations 30 in args, got: %s", argsStr)
	}
	if !strings.Contains(argsStr, "claude") {
		t.Errorf("expected default --agent claude in args, got: %s", argsStr)
	}
}

// ---- Context cancellation with graceful shutdown ----

func TestSpawnGnhf_ContextCancellation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repoDir := t.TempDir()
	initGitRepo(t, repoDir)

	wtDir := t.TempDir()
	addWorktree(t, repoDir, wtDir, "implement/cancel")

	binDir := t.TempDir()
	runID := "run-cancel-001"
	// Mock traps SIGTERM, writes a run:complete with status=aborted/signal, then exits.
	logContent := makeCompleteLog("aborted", "signal", 2, 0)
	// The script uses a background sleep so SIGTERM to the process group wakes it.
	// On SIGTERM, the trap fires: it writes the log and exits cleanly.
	script := fmt.Sprintf(`#!/bin/sh
RUN_DIR="$PWD/.gnhf/runs/%s"
mkdir -p "$RUN_DIR"
trap 'printf '"'"'%s'"'"' > "$RUN_DIR/gnhf.log"; exit 0' TERM INT
sleep 60 &
SLEEP_PID=$!
wait $SLEEP_PID
`, runID, logContent)
	if err := os.WriteFile(filepath.Join(binDir, "gnhf"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after a short delay so the spawn gets to start
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	args := implementer.GnhfArgs{
		WorktreePath: wtDir,
		Prompt:       "do it",
		Timeout:      30 * time.Second,
		GracePeriod:  2 * time.Second,
	}
	r, err := implementer.SpawnGnhf(ctx, args)
	// Either no error (SIGTERM captured graceful log) or ErrIncompleteLog
	// (SIGKILL fired before flush) — both acceptable. Must not hang.
	if err != nil && !errors.Is(err, implementer.ErrIncompleteLog) {
		t.Fatalf("unexpected error type: %v", err)
	}
	if r.Status != implementer.StatusAborted {
		t.Errorf("Status: want aborted, got %q", r.Status)
	}
}

// ---- Crash mid-flush: partial log → ErrIncompleteLog + synthesized result ----

func TestSpawnGnhf_CrashMidFlush(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repoDir := t.TempDir()
	initGitRepo(t, repoDir)

	wtDir := t.TempDir()
	addWorktree(t, repoDir, wtDir, "implement/crash")

	binDir := t.TempDir()
	runID := "run-crash-001"
	// Write a partial log with no run:complete event
	partialLog := `{"event":"iteration:complete","iteration":1}` + "\n" +
		`{"event":"iteration:complete","iteration":2}`
	writeMockGnhf(t, binDir, runID, partialLog, "")
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	ctx := context.Background()
	args := implementer.GnhfArgs{
		WorktreePath: wtDir,
		Prompt:       "do it",
	}
	r, err := implementer.SpawnGnhf(ctx, args)
	if !errors.Is(err, implementer.ErrIncompleteLog) {
		t.Errorf("err: want ErrIncompleteLog, got %v", err)
	}
	if r.Status != implementer.StatusAborted {
		t.Errorf("Status: want aborted (synthesized), got %q", r.Status)
	}
	if !r.LogIncomplete {
		t.Error("LogIncomplete: want true")
	}
}

// ---- Worktree-aware exclude path ----

func TestSpawnGnhf_ExcludePathLandsInCommonGitDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repoDir := t.TempDir()
	initGitRepo(t, repoDir)

	wtDir := t.TempDir()
	addWorktree(t, repoDir, wtDir, "implement/exclude-test")

	binDir := t.TempDir()
	runID := "run-excl-001"
	logContent := makeCompleteLog("stopped", "stop condition met: done", 1, 1)
	writeMockGnhf(t, binDir, runID, logContent, "")
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	ctx := context.Background()
	args := implementer.GnhfArgs{
		WorktreePath: wtDir,
		Prompt:       "do it",
	}
	if _, err := implementer.SpawnGnhf(ctx, args); err != nil {
		t.Fatalf("SpawnGnhf: %v", err)
	}

	// The exclude entry must be in the COMMON git dir's info/exclude,
	// not in the per-worktree .git file.
	commonGitDir := filepath.Join(repoDir, ".git")
	excludePath := filepath.Join(commonGitDir, "info", "exclude")
	data, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("read exclude file: %v", err)
	}
	if !strings.Contains(string(data), ".gnhf/") {
		t.Errorf("expect .gnhf/ in %s, got:\n%s", excludePath, data)
	}

	// Calling SpawnGnhf again must not duplicate the entry (idempotency).
	// Clear the runs dir so the mock can create the same runID without
	// confusing the set-difference run-id discovery (the test we want here
	// is exclude-file idempotency, not run discovery).
	if err := os.RemoveAll(filepath.Join(wtDir, ".gnhf")); err != nil {
		t.Fatalf("clear .gnhf: %v", err)
	}
	if _, err := implementer.SpawnGnhf(ctx, args); err != nil {
		t.Fatalf("SpawnGnhf (second call): %v", err)
	}
	data2, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("read exclude file (second): %v", err)
	}
	count := strings.Count(string(data2), ".gnhf/")
	if count != 1 {
		t.Errorf("idempotency: .gnhf/ appears %d times in exclude, want 1\n%s", count, data2)
	}
}

// ---- runId discovery via name-set diff ----

func TestSpawnGnhf_RunIDDiscovery_HappyPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repoDir := t.TempDir()
	initGitRepo(t, repoDir)

	wtDir := t.TempDir()
	addWorktree(t, repoDir, wtDir, "implement/runid-happy")

	// Pre-seed two existing run dirs A and B
	for _, name := range []string{"A", "B"} {
		dir := filepath.Join(wtDir, ".gnhf", "runs", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "gnhf.log"), []byte(`{"event":"old"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	binDir := t.TempDir()
	runID := "C"
	logContent := makeCompleteLog("stopped", "stop condition met: done", 3, 2)
	writeMockGnhf(t, binDir, runID, logContent, "")
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	ctx := context.Background()
	args := implementer.GnhfArgs{
		WorktreePath: wtDir,
		Prompt:       "do it",
	}
	r, err := implementer.SpawnGnhf(ctx, args)
	if err != nil {
		t.Fatalf("SpawnGnhf: %v", err)
	}

	// Touch A post-spawn to ensure mtime-based selection would break
	aLog := filepath.Join(wtDir, ".gnhf", "runs", "A", "gnhf.log")
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(aLog, future, future); err != nil {
		t.Fatalf("chtimes A: %v", err)
	}

	if r.RunID != "C" {
		t.Errorf("RunID: want C (set-difference winner), got %q", r.RunID)
	}
}

func TestSpawnGnhf_RunIDDiscovery_MultipleNewOneParseable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repoDir := t.TempDir()
	initGitRepo(t, repoDir)

	wtDir := t.TempDir()
	addWorktree(t, repoDir, wtDir, "implement/runid-multi")

	// Pre-seed one existing dir
	existingDir := filepath.Join(wtDir, ".gnhf", "runs", "existing")
	if err := os.MkdirAll(existingDir, 0o755); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	logX := makeCompleteLog("stopped", "stop condition met: done", 2, 1)
	logY := `{"event":"iteration:complete","iteration":1}`
	// Create X (parseable) and Y (no run:complete). Do NOT create UNUSED dir.
	script := fmt.Sprintf(`#!/bin/sh
set -e
mkdir -p "$PWD/.gnhf/runs/X"
mkdir -p "$PWD/.gnhf/runs/Y"
printf '%%s' '%s' > "$PWD/.gnhf/runs/X/gnhf.log"
printf '%%s' '%s' > "$PWD/.gnhf/runs/Y/gnhf.log"
exit 0
`, logX, logY)
	if err := os.WriteFile(filepath.Join(binDir, "gnhf"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	ctx := context.Background()
	args := implementer.GnhfArgs{
		WorktreePath: wtDir,
		Prompt:       "do it",
	}
	r, err := implementer.SpawnGnhf(ctx, args)
	if err != nil {
		t.Fatalf("SpawnGnhf: unexpected error: %v", err)
	}
	if r.RunID != "X" {
		t.Errorf("RunID: want X (only parseable), got %q", r.RunID)
	}
}

func TestSpawnGnhf_RunIDDiscovery_MultipleNewBothParseable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repoDir := t.TempDir()
	initGitRepo(t, repoDir)

	wtDir := t.TempDir()
	addWorktree(t, repoDir, wtDir, "implement/runid-ambig")

	binDir := t.TempDir()
	logX := makeCompleteLog("stopped", "stop condition met: done", 2, 1)
	logY := makeCompleteLog("aborted", "max iterations reached", 5, 0)
	script := fmt.Sprintf(`#!/bin/sh
set -e
mkdir -p "$PWD/.gnhf/runs/X"
mkdir -p "$PWD/.gnhf/runs/Y"
printf '%%s' '%s' > "$PWD/.gnhf/runs/X/gnhf.log"
printf '%%s' '%s' > "$PWD/.gnhf/runs/Y/gnhf.log"
exit 0
`, logX, logY)
	if err := os.WriteFile(filepath.Join(binDir, "gnhf"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	ctx := context.Background()
	args := implementer.GnhfArgs{
		WorktreePath: wtDir,
		Prompt:       "do it",
	}
	r, err := implementer.SpawnGnhf(ctx, args)
	var ambig *implementer.ErrAmbiguousRunDir
	if !errors.As(err, &ambig) {
		t.Errorf("err: want *ErrAmbiguousRunDir, got %T: %v", err, err)
	} else if len(ambig.Candidates) != 2 {
		t.Errorf("Candidates: want 2, got %d: %v", len(ambig.Candidates), ambig.Candidates)
	}
	if !r.LogIncomplete {
		t.Error("LogIncomplete: want true for ambiguous result")
	}
}

func TestSpawnGnhf_RunIDDiscovery_NoNewDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repoDir := t.TempDir()
	initGitRepo(t, repoDir)

	wtDir := t.TempDir()
	addWorktree(t, repoDir, wtDir, "implement/runid-none")

	binDir := t.TempDir()
	// Mock creates NO .gnhf/runs/ entries
	script := "#!/bin/sh\nexit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "gnhf"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	ctx := context.Background()
	args := implementer.GnhfArgs{
		WorktreePath: wtDir,
		Prompt:       "do it",
	}
	r, err := implementer.SpawnGnhf(ctx, args)
	if !errors.Is(err, implementer.ErrRunDirNotFound) {
		t.Errorf("err: want ErrRunDirNotFound, got %v", err)
	}
	if r.Status != implementer.StatusAborted {
		t.Errorf("Status: want aborted (synthesized), got %q", r.Status)
	}
	if !r.LogIncomplete {
		t.Error("LogIncomplete: want true")
	}
}
