package implementer_test

import (
	"errors"
	"strconv"
	"testing"

	"github.com/caesarioshiddieq/claude-code-lark-channel/internal/implementer"
)

// makeRunComplete builds a minimal valid run:complete JSONL line.
func makeRunComplete(status, lastMessage string, iterations, successCount, failCount, commitCount, inputTokens, outputTokens int) string {
	return `{"event":"run:complete","status":"` + status + `","iterations":` +
		strconv.Itoa(iterations) + `,"successCount":` + strconv.Itoa(successCount) +
		`,"failCount":` + strconv.Itoa(failCount) + `,"totalInputTokens":` +
		strconv.Itoa(inputTokens) + `,"totalOutputTokens":` + strconv.Itoa(outputTokens) +
		`,"commitCount":` + strconv.Itoa(commitCount) + `,"worktreePath":"/tmp/wt","lastMessage":"` +
		lastMessage + `"}`
}

func TestParseGnhfLog_StoppedStopWhen(t *testing.T) {
	jsonl := makeRunComplete("stopped", "stop condition met: all tests pass", 5, 3, 1, 2, 1000, 500)
	r, err := implementer.ParseGnhfLog([]byte(jsonl))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Status != implementer.StatusStopped {
		t.Errorf("Status: want stopped, got %q", r.Status)
	}
	if r.Reason != implementer.ReasonStopWhen {
		t.Errorf("Reason: want stop_when, got %q", r.Reason)
	}
	if r.Iterations != 5 {
		t.Errorf("Iterations: want 5, got %d", r.Iterations)
	}
	if r.SuccessCount != 3 {
		t.Errorf("SuccessCount: want 3, got %d", r.SuccessCount)
	}
	if r.CommitCount != 2 {
		t.Errorf("CommitCount: want 2, got %d", r.CommitCount)
	}
	if r.InputTokens != 1000 {
		t.Errorf("InputTokens: want 1000, got %d", r.InputTokens)
	}
	if r.OutputTokens != 500 {
		t.Errorf("OutputTokens: want 500, got %d", r.OutputTokens)
	}
	if r.NoProgress {
		t.Error("NoProgress: want false (commitCount=2)")
	}
	if r.LogIncomplete {
		t.Error("LogIncomplete: want false")
	}
}

func TestParseGnhfLog_StoppedNoStopWhen(t *testing.T) {
	jsonl := makeRunComplete("stopped", "completed normally", 3, 2, 0, 1, 200, 100)
	r, err := implementer.ParseGnhfLog([]byte(jsonl))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Status != implementer.StatusStopped {
		t.Errorf("Status: want stopped, got %q", r.Status)
	}
	if r.Reason != implementer.ReasonUnknown {
		t.Errorf("Reason: want unknown for stopped+no-stop-when, got %q", r.Reason)
	}
}

func TestParseGnhfLog_AbortedMaxIterations(t *testing.T) {
	jsonl := makeRunComplete("aborted", "max iterations reached", 30, 10, 5, 3, 5000, 2000)
	r, err := implementer.ParseGnhfLog([]byte(jsonl))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Status != implementer.StatusAborted {
		t.Errorf("Status: want aborted, got %q", r.Status)
	}
	if r.Reason != implementer.ReasonMaxIterations {
		t.Errorf("Reason: want max_iterations, got %q", r.Reason)
	}
}

func TestParseGnhfLog_AbortedMaxTokens(t *testing.T) {
	jsonl := makeRunComplete("aborted", "max tokens reached", 15, 5, 3, 2, 9000, 3000)
	r, err := implementer.ParseGnhfLog([]byte(jsonl))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Reason != implementer.ReasonMaxTokens {
		t.Errorf("Reason: want max_tokens, got %q", r.Reason)
	}
}

func TestParseGnhfLog_AbortedMaxFailures(t *testing.T) {
	jsonl := makeRunComplete("aborted", "max consecutive failures", 8, 2, 5, 0, 3000, 1000)
	r, err := implementer.ParseGnhfLog([]byte(jsonl))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Reason != implementer.ReasonMaxFailures {
		t.Errorf("Reason: want max_failures, got %q", r.Reason)
	}
}

func TestParseGnhfLog_AbortedSignal(t *testing.T) {
	jsonl := makeRunComplete("aborted", "signal", 4, 1, 1, 1, 1000, 400)
	r, err := implementer.ParseGnhfLog([]byte(jsonl))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Reason != implementer.ReasonSignal {
		t.Errorf("Reason: want signal, got %q", r.Reason)
	}
}

func TestParseGnhfLog_MissingRunComplete(t *testing.T) {
	jsonl := `{"event":"iteration:complete","iteration":1}
{"event":"iteration:complete","iteration":2}`
	r, err := implementer.ParseGnhfLog([]byte(jsonl))
	if !errors.Is(err, implementer.ErrIncompleteLog) {
		t.Errorf("err: want ErrIncompleteLog, got %v", err)
	}
	if r.Status != implementer.StatusAborted {
		t.Errorf("Status: want aborted (synthesized), got %q", r.Status)
	}
	if r.Reason != implementer.ReasonUnknown {
		t.Errorf("Reason: want unknown (synthesized), got %q", r.Reason)
	}
	if !r.LogIncomplete {
		t.Error("LogIncomplete: want true")
	}
}

func TestParseGnhfLog_TrulyMalformedJSONL(t *testing.T) {
	jsonl := `{not valid json at all
also bad`
	r, err := implementer.ParseGnhfLog([]byte(jsonl))
	if !errors.Is(err, implementer.ErrIncompleteLog) {
		t.Errorf("err: want ErrIncompleteLog for malformed JSONL, got %v", err)
	}
	if !r.LogIncomplete {
		t.Error("LogIncomplete: want true")
	}
	if r.Status != implementer.StatusAborted {
		t.Errorf("Status: want aborted (synthesized), got %q", r.Status)
	}
}

func TestParseGnhfLog_IgnoresNonFinalLookalikes(t *testing.T) {
	part := `{"event":"iteration:complete","status":"stopped","iterations":1}` + "\n"
	final := makeRunComplete("aborted", "max iterations reached", 30, 0, 5, 0, 2000, 800)
	r, err := implementer.ParseGnhfLog([]byte(part + final))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Status != implementer.StatusAborted {
		t.Errorf("Status: want aborted (from run:complete), got %q", r.Status)
	}
	if r.Iterations != 30 {
		t.Errorf("Iterations: want 30 from run:complete, got %d", r.Iterations)
	}
}

func TestParseGnhfLog_UsesLastRunComplete(t *testing.T) {
	first := makeRunComplete("stopped", "first", 5, 2, 0, 1, 100, 50)
	last := makeRunComplete("aborted", "max iterations reached", 30, 10, 20, 0, 9000, 4000)
	r, err := implementer.ParseGnhfLog([]byte(first + "\n" + last))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Status != implementer.StatusAborted {
		t.Errorf("Status: want aborted (last run:complete), got %q", r.Status)
	}
	if r.Iterations != 30 {
		t.Errorf("Iterations: want 30 from last, got %d", r.Iterations)
	}
}

func TestNoProgress_CommitCountZero(t *testing.T) {
	jsonl := makeRunComplete("stopped", "done", 3, 2, 0, 0, 500, 200)
	r, err := implementer.ParseGnhfLog([]byte(jsonl))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !r.NoProgress {
		t.Error("NoProgress: want true when commitCount=0")
	}
}

func TestNoProgress_CommitCountNonZero(t *testing.T) {
	jsonl := makeRunComplete("stopped", "done", 3, 2, 0, 3, 500, 200)
	r, err := implementer.ParseGnhfLog([]byte(jsonl))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.NoProgress {
		t.Error("NoProgress: want false when commitCount=3")
	}
}

func TestNoProgress_SuccessCountNonZeroButCommitZero(t *testing.T) {
	jsonl := makeRunComplete("stopped", "done", 5, 3, 0, 0, 1000, 400)
	r, err := implementer.ParseGnhfLog([]byte(jsonl))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !r.NoProgress {
		t.Error("NoProgress: want true when commitCount=0 regardless of successCount")
	}
}

// TestParseGnhfLog_LongLastMessage verifies that a run:complete line larger
// than the default bufio.Scanner buffer (64KB) is parsed correctly without
// being silently truncated into ErrIncompleteLog (codex round-2 #2). The
// fix sets scanner.Buffer(1MB, 16MB) — this test pushes well above 64KB.
func TestParseGnhfLog_LongLastMessage(t *testing.T) {
	// 100 KB of repeated text — well above the 64KB default Scanner limit
	// but well below the 16MB ceiling we configured. Use a non-special
	// character so the JSON encoding stays simple (no escaping cost).
	const size = 100 * 1024
	long := make([]byte, size)
	for i := range long {
		long[i] = 'x'
	}
	jsonl := makeRunComplete("stopped", string(long), 7, 5, 0, 3, 2000, 1000)

	r, err := implementer.ParseGnhfLog([]byte(jsonl))
	if err != nil {
		t.Fatalf("expected successful parse of 100KB lastMessage line, got err: %v", err)
	}
	if len(r.LastMessage) != size {
		t.Errorf("LastMessage length: got %d, want %d (parser truncated long line?)",
			len(r.LastMessage), size)
	}
	if r.Iterations != 7 || r.CommitCount != 3 {
		t.Errorf("metadata fields lost across long line: iterations=%d commits=%d",
			r.Iterations, r.CommitCount)
	}
}
