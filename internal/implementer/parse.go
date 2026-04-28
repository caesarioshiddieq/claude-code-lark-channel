// Package implementer provides the gnhf spawn wrapper and log parser for the
// autonomous implementer subsystem. parse.go is a pure I/O-free module;
// spawn.go contains all OS interaction (exec, filesystem, signals).
package implementer

import "errors"

// Status represents the final state of a gnhf run as reported in run:complete.
type Status string

const (
	StatusStopped Status = "stopped"
	StatusAborted Status = "aborted"
)

// Reason is derived from the lastMessage field of the run:complete event.
type Reason string

const (
	ReasonStopWhen      Reason = "stop_when"
	ReasonMaxIterations Reason = "max_iterations"
	ReasonMaxTokens     Reason = "max_tokens"
	ReasonMaxFailures   Reason = "max_failures"
	ReasonSignal        Reason = "signal"
	ReasonUnknown       Reason = "unknown"
)

// ErrIncompleteLog is returned when no run:complete event is found in the log,
// indicating gnhf or its agent crashed before flushing the final event.
// Callers receive a synthesized GnhfResult alongside this error so they can
// persist implementer_runs and decide retry policy without leaving a NULL row.
var ErrIncompleteLog = errors.New("gnhf log incomplete: missing run:complete event")

// GnhfResult holds the parsed outcome of a gnhf run.
type GnhfResult struct {
	Status        Status
	Reason        Reason
	Iterations    int
	SuccessCount  int
	FailCount     int
	CommitCount   int
	InputTokens   int
	OutputTokens  int
	WorktreePath  string // gnhf-reported (may be empty when --worktree absent)
	RunID         string // .gnhf/runs/<runID>/ directory name; set by SpawnGnhf
	NotesExcerpt  string // first ~512 bytes of <runDir>/notes.md, if present
	LastMessage   string // free-text from orchestrator (used to derive Reason)
	NoProgress    bool   // derived: CommitCount == 0
	LogIncomplete bool   // set when run:complete event was never written
}

// ParseGnhfLog parses a gnhf JSONL log file and returns the run outcome.
// It scans all lines, keeps the LAST run:complete event, and derives Reason
// from LastMessage substrings.
//
// Crash-resilience: if no run:complete line is found, ParseGnhfLog returns
// ErrIncompleteLog alongside a synthesized GnhfResult with Status=Aborted,
// Reason=Unknown, LogIncomplete=true. This allows the caller to persist the
// row and make retry decisions without blocking on a NULL outcome.
func ParseGnhfLog(jsonl []byte) (GnhfResult, error) {
	// stub — always returns incomplete to make RED tests fail on content
	return GnhfResult{
		Status:        StatusAborted,
		Reason:        ReasonUnknown,
		LogIncomplete: true,
	}, ErrIncompleteLog
}
