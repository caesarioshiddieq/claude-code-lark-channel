package implementer_test

import (
	"strings"
	"testing"

	"github.com/caesarioshiddieq/claude-code-lark-channel/internal/implementer"
)

// TestFormatImplementerSummary covers the 8 (Status, Reason, NoProgress)
// outcome rows from the Task 6 spec table, plus edge cases.
func TestFormatImplementerSummary(t *testing.T) {
	t.Parallel()

	baseResult := implementer.GnhfResult{
		Iterations:   5,
		CommitCount:  2,
		InputTokens:  1000,
		OutputTokens: 500,
	}

	type tc struct {
		name          string
		result        implementer.GnhfResult
		branchName    string
		prURL         string
		maxIterations int
		maxTokens     int64
		wantHeadline  string
		wantContains  []string
		wantAbsent    []string
	}

	cases := []tc{
		// Row 1: stopped / stop_when / NoProgress=false
		{
			name: "stopped_stopwhen_progress",
			result: func() implementer.GnhfResult {
				r := baseResult
				r.Status = implementer.StatusStopped
				r.Reason = implementer.ReasonStopWhen
				r.NoProgress = false
				return r
			}(),
			branchName:    "implement/task-abc",
			prURL:         "https://github.com/example/repo/pull/1",
			maxIterations: 30,
			maxTokens:     50000,
			wantHeadline:  "finished — stop-when condition met",
			wantContains: []string{
				"iterations: 5 / 30",
				"commits: 2",
				"tokens used: 1500 (in: 1000, out: 500) / 50000 allowance",
				"branch: implement/task-abc",
				"PR: https://github.com/example/repo/pull/1",
			},
		},
		// Row 2: stopped / stop_when / NoProgress=true
		{
			name: "stopped_stopwhen_noprogress",
			result: func() implementer.GnhfResult {
				r := baseResult
				r.Status = implementer.StatusStopped
				r.Reason = implementer.ReasonStopWhen
				r.NoProgress = true
				r.CommitCount = 0
				return r
			}(),
			branchName:    "implement/task-abc",
			maxIterations: 30,
			wantHeadline:  "halted — stop-when matched but no commits made",
		},
		// Row 3: stopped / unknown / *
		{
			name: "stopped_unknown",
			result: func() implementer.GnhfResult {
				r := baseResult
				r.Status = implementer.StatusStopped
				r.Reason = implementer.ReasonUnknown
				return r
			}(),
			branchName:    "implement/task-abc",
			maxIterations: 30,
			wantHeadline:  "stopped — orchestrator returned without explicit reason",
		},
		// Row 4: aborted / max_iterations / *
		{
			name: "aborted_maxiterations",
			result: func() implementer.GnhfResult {
				r := baseResult
				r.Status = implementer.StatusAborted
				r.Reason = implementer.ReasonMaxIterations
				return r
			}(),
			branchName:    "implement/task-abc",
			maxIterations: 30,
			wantHeadline:  "timed out — max iterations reached",
		},
		// Row 5: aborted / max_tokens / *
		{
			name: "aborted_maxtokens",
			result: func() implementer.GnhfResult {
				r := baseResult
				r.Status = implementer.StatusAborted
				r.Reason = implementer.ReasonMaxTokens
				return r
			}(),
			branchName:    "implement/task-abc",
			maxIterations: 30,
			wantHeadline:  "timed out — token ceiling reached",
		},
		// Row 6: aborted / max_failures / *
		{
			name: "aborted_maxfailures",
			result: func() implementer.GnhfResult {
				r := baseResult
				r.Status = implementer.StatusAborted
				r.Reason = implementer.ReasonMaxFailures
				return r
			}(),
			branchName:    "implement/task-abc",
			maxIterations: 30,
			wantHeadline:  "failed — max consecutive iteration failures",
		},
		// Row 7: aborted / signal / *
		{
			name: "aborted_signal",
			result: func() implementer.GnhfResult {
				r := baseResult
				r.Status = implementer.StatusAborted
				r.Reason = implementer.ReasonSignal
				return r
			}(),
			branchName:    "implement/task-abc",
			maxIterations: 30,
			wantHeadline:  "interrupted — supervisor cancelled",
		},
		// Row 8: aborted / unknown / *
		{
			name: "aborted_unknown",
			result: func() implementer.GnhfResult {
				r := baseResult
				r.Status = implementer.StatusAborted
				r.Reason = implementer.ReasonUnknown
				return r
			}(),
			branchName:    "implement/task-abc",
			maxIterations: 30,
			wantHeadline:  "aborted — see notes for context",
		},
		// Edge case: empty NotesExcerpt → no notes line
		{
			name: "no_notes_line_when_empty",
			result: func() implementer.GnhfResult {
				r := baseResult
				r.Status = implementer.StatusStopped
				r.Reason = implementer.ReasonStopWhen
				r.NotesExcerpt = ""
				return r
			}(),
			branchName:    "implement/task-abc",
			maxIterations: 30,
			wantHeadline:  "finished — stop-when condition met",
			wantAbsent:    []string{"notes (excerpt):"},
		},
		// Edge case: non-empty NotesExcerpt → notes line present
		{
			name: "notes_line_when_present",
			result: func() implementer.GnhfResult {
				r := baseResult
				r.Status = implementer.StatusStopped
				r.Reason = implementer.ReasonStopWhen
				r.NotesExcerpt = "fixed the auth bug"
				return r
			}(),
			branchName:    "implement/task-abc",
			maxIterations: 30,
			wantContains:  []string{`notes (excerpt): "fixed the auth bug"`},
		},
		// Edge case: BranchName="" → no branch line
		{
			name: "no_branch_line_when_empty",
			result: func() implementer.GnhfResult {
				r := baseResult
				r.Status = implementer.StatusStopped
				r.Reason = implementer.ReasonStopWhen
				return r
			}(),
			branchName:    "",
			maxIterations: 30,
			wantHeadline:  "finished — stop-when condition met",
			wantAbsent:    []string{"branch:"},
		},
		// Edge case: PRURL="" → no PR line
		{
			name: "no_pr_line_when_empty",
			result: func() implementer.GnhfResult {
				r := baseResult
				r.Status = implementer.StatusStopped
				r.Reason = implementer.ReasonStopWhen
				return r
			}(),
			branchName:    "implement/task-abc",
			prURL:         "",
			maxIterations: 30,
			wantHeadline:  "finished — stop-when condition met",
			wantAbsent:    []string{"PR:"},
		},
		// Edge case: MaxTokens=0 → no "/ N allowance" suffix on tokens line
		{
			name: "maxtokens_zero_no_allowance",
			result: func() implementer.GnhfResult {
				r := baseResult
				r.Status = implementer.StatusStopped
				r.Reason = implementer.ReasonStopWhen
				r.InputTokens = 100
				r.OutputTokens = 50
				return r
			}(),
			branchName:    "implement/task-abc",
			maxIterations: 30,
			maxTokens:     0,
			wantContains:  []string{"tokens used: 150 (in: 100, out: 50)"},
			wantAbsent:    []string{"allowance"},
		},
		// Edge case: zero Iterations + zero tokens → still prints the lines
		{
			name: "zero_iterations_and_tokens",
			result: func() implementer.GnhfResult {
				r := implementer.GnhfResult{}
				r.Status = implementer.StatusAborted
				r.Reason = implementer.ReasonUnknown
				return r
			}(),
			branchName:    "implement/task-abc",
			maxIterations: 30,
			wantContains:  []string{"iterations: 0 / 30", "tokens used: 0 (in: 0, out: 0)"},
		},
		// Edge case: very long NotesExcerpt (512 bytes) — formatter does NOT re-truncate
		{
			name: "long_notes_excerpt_no_double_truncate",
			result: func() implementer.GnhfResult {
				r := baseResult
				r.Status = implementer.StatusStopped
				r.Reason = implementer.ReasonStopWhen
				r.NotesExcerpt = strings.Repeat("x", 512)
				return r
			}(),
			branchName:    "implement/task-abc",
			maxIterations: 30,
			// Notes should appear as-is (already at the 512-byte limit from spawn.go)
			wantContains: []string{`notes (excerpt): "` + strings.Repeat("x", 512) + `"`},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := implementer.FormatImplementerSummary(tc.result, tc.branchName, tc.prURL, tc.maxIterations, tc.maxTokens)

			if tc.wantHeadline != "" && !strings.Contains(got, tc.wantHeadline) {
				t.Errorf("headline not found in output:\nwant substring: %q\ngot:\n%s", tc.wantHeadline, got)
			}
			for _, want := range tc.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("expected substring %q not found in output:\n%s", want, got)
				}
			}
			for _, absent := range tc.wantAbsent {
				if strings.Contains(got, absent) {
					t.Errorf("unexpected substring %q found in output:\n%s", absent, got)
				}
			}
		})
	}
}

// TestFormatImplementerSummary_LogIncomplete_AllRows verifies that the
// " ⚠ log incomplete" suffix appears regardless of which base row matched.
// Single 8-row table covers all (Status, Reason, NoProgress) combinations
// from the Task 6 spec — names match the spec's row ordering for readability.
func TestFormatImplementerSummary_LogIncomplete_AllRows(t *testing.T) {
	t.Parallel()

	type rowCase struct {
		name       string
		status     implementer.Status
		reason     implementer.Reason
		noProgress bool
		wantBase   string
	}

	rows := []rowCase{
		{"row1_stopped_stopwhen_progress", implementer.StatusStopped, implementer.ReasonStopWhen, false, "finished — stop-when condition met"},
		{"row2_stopped_stopwhen_noprogress", implementer.StatusStopped, implementer.ReasonStopWhen, true, "halted — stop-when matched but no commits made"},
		{"row3_stopped_unknown", implementer.StatusStopped, implementer.ReasonUnknown, false, "stopped — orchestrator returned without explicit reason"},
		{"row4_aborted_maxiterations", implementer.StatusAborted, implementer.ReasonMaxIterations, false, "timed out — max iterations reached"},
		{"row5_aborted_maxtokens", implementer.StatusAborted, implementer.ReasonMaxTokens, false, "timed out — token ceiling reached"},
		{"row6_aborted_maxfailures", implementer.StatusAborted, implementer.ReasonMaxFailures, false, "failed — max consecutive iteration failures"},
		{"row7_aborted_signal", implementer.StatusAborted, implementer.ReasonSignal, false, "interrupted — supervisor cancelled"},
		{"row8_aborted_unknown", implementer.StatusAborted, implementer.ReasonUnknown, false, "aborted — see notes for context"},
	}

	for _, row := range rows {
		row := row
		t.Run(row.name+"_logincomplete", func(t *testing.T) {
			t.Parallel()
			r := implementer.GnhfResult{
				Status:        row.status,
				Reason:        row.reason,
				NoProgress:    row.noProgress,
				LogIncomplete: true,
				Iterations:    3,
				CommitCount:   1,
				InputTokens:   100,
				OutputTokens:  50,
			}
			got := implementer.FormatImplementerSummary(r, "implement/test", "", 30, 0)

			wantSuffix := row.wantBase + " ⚠ log incomplete"
			if !strings.Contains(got, wantSuffix) {
				t.Errorf("expected headline+suffix %q not found in output:\n%s", wantSuffix, got)
			}
		})
	}
}
