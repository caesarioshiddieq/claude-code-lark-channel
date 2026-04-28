package implementer

import (
	"context"
	"errors"
	"fmt"
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

// SpawnGnhf runs gnhf as a subprocess inside args.WorktreePath, waits for it
// to complete (respecting ctx and args.Timeout), discovers the run directory
// via name-set diff, and returns the parsed GnhfResult.
func SpawnGnhf(ctx context.Context, args GnhfArgs) (GnhfResult, error) {
	// stub — RED: always returns not-implemented error so tests fail on behavior
	return GnhfResult{
		Status:        StatusAborted,
		Reason:        ReasonUnknown,
		LogIncomplete: true,
	}, fmt.Errorf("SpawnGnhf: not implemented")
}
