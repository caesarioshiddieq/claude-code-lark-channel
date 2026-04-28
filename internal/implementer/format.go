package implementer

import (
	"fmt"
	"strings"
)

// headlineVerb maps a (Status, Reason, NoProgress) triple to the human-readable
// verb phrase used in the Lark thread message headline.
func headlineVerb(result GnhfResult) string {
	switch result.Status {
	case StatusStopped:
		switch result.Reason {
		case ReasonStopWhen:
			if result.NoProgress {
				return "halted — stop-when matched but no commits made"
			}
			return "finished — stop-when condition met"
		default:
			return "stopped — orchestrator returned without explicit reason"
		}
	case StatusAborted:
		switch result.Reason {
		case ReasonMaxIterations:
			return "timed out — max iterations reached"
		case ReasonMaxTokens:
			return "timed out — token ceiling reached"
		case ReasonMaxFailures:
			return "failed — max consecutive iteration failures"
		case ReasonSignal:
			return "interrupted — supervisor cancelled"
		default:
			return "aborted — see notes for context"
		}
	default:
		return "aborted — see notes for context"
	}
}

// FormatImplementerSummary builds the Lark thread message for one
// autonomous-implementer run. All field-lookup logic is in this function —
// caller passes the raw GnhfResult plus the run's environmental knobs
// (branch name, PR URL, ceilings).
//
// When MaxTokens is 0 (unbounded), the "tokens used" line omits the
// "/ N allowance" suffix to avoid the awkward "/ 0 allowance" display.
//
// The formatter does NOT truncate NotesExcerpt — spawn.go already caps it at
// 512 bytes (notesExcerptMax). Double-truncation would be incorrect.
func FormatImplementerSummary(
	result GnhfResult,
	branchName, prURL string,
	maxIterations int,
	maxTokens int64,
) string {
	verb := headlineVerb(result)
	if result.LogIncomplete {
		verb += " ⚠ log incomplete"
	}

	totalTokens := result.InputTokens + result.OutputTokens

	var b strings.Builder
	fmt.Fprintf(&b, "🤖 implementer (gnhf) %s\n", verb)
	fmt.Fprintf(&b, "  • iterations: %d / %d\n", result.Iterations, maxIterations)
	fmt.Fprintf(&b, "  • commits: %d\n", result.CommitCount)
	if maxTokens > 0 {
		fmt.Fprintf(&b, "  • tokens used: %d (in: %d, out: %d) / %d allowance\n",
			totalTokens, result.InputTokens, result.OutputTokens, maxTokens)
	} else {
		fmt.Fprintf(&b, "  • tokens used: %d (in: %d, out: %d)\n",
			totalTokens, result.InputTokens, result.OutputTokens)
	}
	if branchName != "" {
		fmt.Fprintf(&b, "  • branch: %s\n", branchName)
	}
	if prURL != "" {
		fmt.Fprintf(&b, "  • PR: %s\n", prURL)
	}
	if result.NotesExcerpt != "" {
		fmt.Fprintf(&b, "  • notes (excerpt): %q\n", result.NotesExcerpt)
	}

	return b.String()
}
