// Package implementer provides the gnhf spawn wrapper and log parser for the
// autonomous implementer subsystem. parse.go is a pure I/O-free module;
// spawn.go contains all OS interaction (exec, filesystem, signals).
package implementer

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

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

// gnhfRunCompleteEvent is the JSON shape of a run:complete line in gnhf.log.
type gnhfRunCompleteEvent struct {
	Event             string `json:"event"`
	Status            string `json:"status"`
	Iterations        int    `json:"iterations"`
	SuccessCount      int    `json:"successCount"`
	FailCount         int    `json:"failCount"`
	TotalInputTokens  int    `json:"totalInputTokens"`
	TotalOutputTokens int    `json:"totalOutputTokens"`
	CommitCount       int    `json:"commitCount"`
	WorktreePath      string `json:"worktreePath"`
	LastMessage       string `json:"lastMessage"`
}

// incompleteResult is the synthesized result returned when no run:complete
// event is found — allows callers to persist implementer_runs without blocking.
func incompleteResult() GnhfResult {
	return GnhfResult{
		Status:        StatusAborted,
		Reason:        ReasonUnknown,
		LastMessage:   "missing run:complete event",
		LogIncomplete: true,
	}
}

// deriveReason maps lastMessage substrings to a Reason constant.
// For status=stopped, the stop-when condition match is signalled by the
// "stop condition" substring that gnhf injects when its --stop-when predicate
// is satisfied. All other stopped cases fall through to ReasonUnknown.
func deriveReason(status Status, lastMessage string) Reason {
	msg := strings.ToLower(lastMessage)
	switch {
	case strings.Contains(msg, "max iterations reached"):
		return ReasonMaxIterations
	case strings.Contains(msg, "max tokens reached"):
		return ReasonMaxTokens
	case strings.Contains(msg, "max consecutive failures"):
		return ReasonMaxFailures
	case strings.Contains(msg, "signal"):
		return ReasonSignal
	case status == StatusStopped && strings.Contains(msg, "stop condition"):
		return ReasonStopWhen
	default:
		return ReasonUnknown
	}
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
	var last *gnhfRunCompleteEvent

	scanner := bufio.NewScanner(bytes.NewReader(jsonl))
	// Default Scanner buffer is 64KB. A run:complete line with a long
	// lastMessage (e.g. multi-line stack trace, large stop-condition output)
	// could exceed it and silently truncate the stream — making this function
	// incorrectly return ErrIncompleteLog (codex round-2 #2).
	// 1MB initial / 16MB max accommodates realistic worst cases.
	scanner.Buffer(make([]byte, 1<<20), 1<<24)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		// Fast-path: only attempt full unmarshal for lines containing
		// "run:complete" to avoid paying JSON decode cost on every iteration
		// event. Uses bytes.Contains on raw line for efficiency.
		if !bytes.Contains(line, []byte(`"run:complete"`)) {
			continue
		}
		var ev gnhfRunCompleteEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue // malformed line — skip; keep scanning
		}
		if ev.Event != "run:complete" {
			continue // event field didn't match (e.g. "iteration:complete" containing the substring)
		}
		evCopy := ev
		last = &evCopy
	}

	// Distinct from missing run:complete: scanner.Err() reports buffer overflow
	// or read-side failures. Treat as incomplete log so the dispatcher persists
	// a synthesized result, but with a more specific LastMessage so operators
	// can distinguish "log truncated by gnhf" from "log unparseable by us".
	if err := scanner.Err(); err != nil {
		return GnhfResult{
			Status:        StatusAborted,
			Reason:        ReasonUnknown,
			LastMessage:   "log scanner error: " + err.Error(),
			LogIncomplete: true,
		}, ErrIncompleteLog
	}

	if last == nil {
		return incompleteResult(), ErrIncompleteLog
	}

	status := Status(last.Status)
	reason := deriveReason(status, last.LastMessage)

	r := GnhfResult{
		Status:       status,
		Reason:       reason,
		Iterations:   last.Iterations,
		SuccessCount: last.SuccessCount,
		FailCount:    last.FailCount,
		CommitCount:  last.CommitCount,
		InputTokens:  last.TotalInputTokens,
		OutputTokens: last.TotalOutputTokens,
		WorktreePath: last.WorktreePath,
		LastMessage:  last.LastMessage,
		NoProgress:   last.CommitCount == 0,
	}
	return r, nil
}
