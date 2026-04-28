package implementer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// GnhfArgs configures a SpawnGnhf invocation.
type GnhfArgs struct {
	Prompt         string        // delivered via stdin
	WorktreePath   string        // cmd.Dir; cwd for the gnhf process
	ExpectedBranch string        // preflight: HEAD branch must match this if non-empty
	MaxTokens      int64         // passed as --max-tokens (0 = omit flag)
	MaxIterations  int           // default 30
	StopWhen       string        // default "all tests pass and the implementation matches the request"
	Agent          string        // default "claude"
	Timeout        time.Duration // default 4h
	GracePeriod    time.Duration // default 30s — SIGTERM→grace→SIGKILL window
}

// ErrAmbiguousRunDir is returned when multiple new run directories appear after
// spawn and more than one contains a parseable run:complete event. Callers
// receive a synthesized LogIncomplete=true result alongside this error.
type ErrAmbiguousRunDir struct {
	Candidates []string
}

func (e *ErrAmbiguousRunDir) Error() string {
	return fmt.Sprintf("ambiguous gnhf run dir: %d candidates: %v", len(e.Candidates), e.Candidates)
}

// ErrRunDirNotFound is returned when gnhf exits but no new run directory
// appeared under <WorktreePath>/.gnhf/runs/. Callers receive a synthesized
// (Aborted, Unknown, LogIncomplete=true) result alongside this error.
var ErrRunDirNotFound = errors.New("gnhf run directory not found after spawn")

const (
	defaultMaxIterations = 30
	defaultStopWhen      = "all tests pass and the implementation matches the request"
	defaultAgent         = "claude"
	defaultTimeout       = 4 * time.Hour
	defaultGracePeriod   = 30 * time.Second
	notesExcerptMax      = 512
)

// applyDefaults fills zero-value fields with their documented defaults.
func applyDefaults(args *GnhfArgs) {
	if args.MaxIterations == 0 {
		args.MaxIterations = defaultMaxIterations
	}
	if args.StopWhen == "" {
		args.StopWhen = defaultStopWhen
	}
	if args.Agent == "" {
		args.Agent = defaultAgent
	}
	if args.Timeout == 0 {
		args.Timeout = defaultTimeout
	}
	if args.GracePeriod == 0 {
		args.GracePeriod = defaultGracePeriod
	}
}

// preflight validates WorktreePath: must exist, be a git worktree, have a
// non-detached HEAD, and (if ExpectedBranch is set) be on that branch.
func preflight(ctx context.Context, args GnhfArgs) error {
	if _, err := os.Stat(args.WorktreePath); err != nil {
		return fmt.Errorf("preflight: WorktreePath %q: %w", args.WorktreePath, err)
	}

	// Verify it's a git repo (has .git entry or is a worktree).
	out, err := exec.CommandContext(ctx, "git", "-C", args.WorktreePath,
		"rev-parse", "--git-dir").Output()
	if err != nil {
		return fmt.Errorf("preflight: %q is not a git worktree: %w", args.WorktreePath, err)
	}
	_ = out

	// Check HEAD is not detached.
	headOut, err := exec.CommandContext(ctx, "git", "-C", args.WorktreePath,
		"symbolic-ref", "--short", "HEAD").Output()
	if err != nil {
		return fmt.Errorf("preflight: detached HEAD in %q", args.WorktreePath)
	}
	branch := strings.TrimSpace(string(headOut))

	// Check branch matches if caller provided ExpectedBranch.
	if args.ExpectedBranch != "" && branch != args.ExpectedBranch {
		return fmt.Errorf("preflight: branch mismatch: want %q, HEAD is %q",
			args.ExpectedBranch, branch)
	}
	return nil
}

// commonGitDir resolves the common git directory for the worktree at wtPath.
// For a linked worktree, git rev-parse --git-common-dir returns the parent
// repo's .git directory. For the main worktree itself it returns ".git".
func commonGitDir(ctx context.Context, wtPath string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", wtPath,
		"rev-parse", "--absolute-git-dir").Output()
	if err != nil {
		return "", fmt.Errorf("rev-parse --absolute-git-dir: %w", err)
	}
	gitDir := strings.TrimSpace(string(out))

	// For a linked worktree .git is a file; the common dir is the parent.
	// git rev-parse --git-common-dir gives us the shared objects dir directly.
	out2, err := exec.CommandContext(ctx, "git", "-C", wtPath,
		"rev-parse", "--git-common-dir").Output()
	if err != nil {
		// Fallback: use the absolute git dir
		return gitDir, nil
	}
	common := strings.TrimSpace(string(out2))
	if filepath.IsAbs(common) {
		return common, nil
	}
	// Relative path is relative to the worktree
	abs, err := filepath.Abs(filepath.Join(wtPath, common))
	if err != nil {
		return gitDir, nil
	}
	return abs, nil
}

// ensureGnhfExcluded appends ".gnhf/" to <commonGitDir>/info/exclude
// idempotently (no duplicate if already present).
func ensureGnhfExcluded(ctx context.Context, wtPath string) error {
	cgd, err := commonGitDir(ctx, wtPath)
	if err != nil {
		return fmt.Errorf("resolve common git dir: %w", err)
	}
	excludePath := filepath.Join(cgd, "info", "exclude")

	// Read existing content; missing file is fine — we'll create it.
	existing, err := os.ReadFile(excludePath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read exclude file %q: %w", excludePath, err)
	}

	if strings.Contains(string(existing), ".gnhf/") {
		return nil // already present — idempotent
	}

	if err := os.MkdirAll(filepath.Dir(excludePath), 0o755); err != nil {
		return fmt.Errorf("mkdir exclude dir: %w", err)
	}

	f, err := os.OpenFile(excludePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open exclude file: %w", err)
	}
	defer f.Close()

	line := "\n.gnhf/\n"
	if len(existing) > 0 && existing[len(existing)-1] == '\n' {
		line = ".gnhf/\n"
	}
	_, err = f.WriteString(line)
	return err
}

// snapshotRunDirs returns the set of directory names currently present under
// <wtPath>/.gnhf/runs/. Missing directory → empty set (not an error).
func snapshotRunDirs(wtPath string) map[string]struct{} {
	runsDir := filepath.Join(wtPath, ".gnhf", "runs")
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		return map[string]struct{}{}
	}
	set := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			set[e.Name()] = struct{}{}
		}
	}
	return set
}

// newDirNames returns names in post that are not in pre.
func newDirNames(pre, post map[string]struct{}) []string {
	var names []string
	for name := range post {
		if _, ok := pre[name]; !ok {
			names = append(names, name)
		}
	}
	return names
}

// hasParseableRunComplete reports whether the gnhf.log in runDir contains a
// parseable run:complete event. Used for tie-breaking ambiguous new dirs.
func hasParseableRunComplete(runDir string) bool {
	data, err := os.ReadFile(filepath.Join(runDir, "gnhf.log"))
	if err != nil {
		return false
	}
	r, err := ParseGnhfLog(data)
	return err == nil && !r.LogIncomplete
}

// readNotesExcerpt reads the first notesExcerptMax bytes of notes.md in runDir.
// Returns "" when the file does not exist.
func readNotesExcerpt(runDir string) string {
	data, err := os.ReadFile(filepath.Join(runDir, "notes.md"))
	if err != nil {
		return ""
	}
	if len(data) > notesExcerptMax {
		data = data[:notesExcerptMax]
	}
	return string(data)
}

// resolveRunDir picks the unique new run directory from newDirs under runsBase.
// Returns (runID, nil) on success. Returns ("", ErrRunDirNotFound) when
// newDirs is empty. Returns ("", *ErrAmbiguousRunDir) when ambiguous.
func resolveRunDir(runsBase string, newDirs []string) (string, error) {
	switch len(newDirs) {
	case 0:
		return "", ErrRunDirNotFound
	case 1:
		return newDirs[0], nil
	default:
		// Multiple new dirs: find those with a parseable run:complete.
		var parseable []string
		for _, name := range newDirs {
			if hasParseableRunComplete(filepath.Join(runsBase, name)) {
				parseable = append(parseable, name)
			}
		}
		if len(parseable) == 1 {
			return parseable[0], nil
		}
		// Zero or >1 parseable: ambiguous.
		candidates := parseable
		if len(candidates) == 0 {
			candidates = newDirs
		}
		return "", &ErrAmbiguousRunDir{Candidates: candidates}
	}
}

// SpawnGnhf runs gnhf as a subprocess inside args.WorktreePath, waits for it
// to complete (respecting ctx and args.Timeout), discovers the run directory
// via name-set diff, and returns the parsed GnhfResult.
func SpawnGnhf(ctx context.Context, args GnhfArgs) (GnhfResult, error) {
	applyDefaults(&args)

	// Step 1: Preflight
	if err := preflight(ctx, args); err != nil {
		return GnhfResult{}, err
	}

	// Step 2: Ensure .gnhf/ is in the common git exclude file
	if err := ensureGnhfExcluded(ctx, args.WorktreePath); err != nil {
		// Non-fatal: log only; don't abort the spawn
		_ = err
	}

	// Step 3: Snapshot pre-spawn run dirs
	runsBase := filepath.Join(args.WorktreePath, ".gnhf", "runs")
	preSnap := snapshotRunDirs(args.WorktreePath)

	// Step 4: Build gnhf command
	cmdArgs := []string{
		"--agent", args.Agent,
		"--max-iterations", fmt.Sprintf("%d", args.MaxIterations),
		"--stop-when", args.StopWhen,
	}
	if args.MaxTokens > 0 {
		cmdArgs = append(cmdArgs, "--max-tokens", fmt.Sprintf("%d", args.MaxTokens))
	}

	// Use plain exec.Command (NOT CommandContext) — we manage kill ourselves
	// via the SIGTERM→grace→SIGKILL flow below, so we can let the process
	// flush its run:complete event during the grace period. CommandContext
	// would race us with its own SIGKILL on ctx.Done().
	cmd := exec.Command("gnhf", cmdArgs...)
	cmd.Dir = args.WorktreePath
	cmd.Stdin = strings.NewReader(args.Prompt)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Step 5: Start with graceful shutdown on cancellation/timeout
	if err := cmd.Start(); err != nil {
		return GnhfResult{}, fmt.Errorf("gnhf start: %w", err)
	}

	// Set up a timeout context layered on top of the caller's ctx
	runCtx, runCancel := context.WithTimeout(ctx, args.Timeout)
	defer runCancel()

	// waitCh receives the process exit error (nil = clean exit)
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	select {
	case <-waitCh:
		// Process exited on its own — proceed to runDir discovery
	case <-runCtx.Done():
		// Cancellation or timeout: graceful SIGTERM → grace period → SIGKILL
		if cmd.Process != nil {
			// Send SIGTERM to the whole process group
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		}
		graceTimer := time.NewTimer(args.GracePeriod)
		select {
		case <-waitCh:
			graceTimer.Stop()
		case <-graceTimer.C:
			if cmd.Process != nil {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
			<-waitCh
		}
	}

	// Step 6: Re-snapshot and compute new dirs
	postSnap := snapshotRunDirs(args.WorktreePath)
	newDirs := newDirNames(preSnap, postSnap)

	runID, resolveErr := resolveRunDir(runsBase, newDirs)
	if resolveErr != nil {
		synth := GnhfResult{
			Status:        StatusAborted,
			Reason:        ReasonUnknown,
			LogIncomplete: true,
		}
		return synth, resolveErr
	}

	// Step 7: Parse log and populate result
	runDir := filepath.Join(runsBase, runID)
	logData, err := os.ReadFile(filepath.Join(runDir, "gnhf.log"))
	if err != nil {
		// Missing or unreadable log file → treat as incomplete. This covers the
		// SIGKILL-before-flush case (graceful cancel exceeded GracePeriod).
		synth := GnhfResult{
			Status:        StatusAborted,
			Reason:        ReasonUnknown,
			RunID:         runID,
			LastMessage:   "missing gnhf.log file",
			LogIncomplete: true,
		}
		return synth, ErrIncompleteLog
	}

	result, parseErr := ParseGnhfLog(logData)
	result.RunID = runID
	result.NotesExcerpt = readNotesExcerpt(runDir)
	return result, parseErr
}
