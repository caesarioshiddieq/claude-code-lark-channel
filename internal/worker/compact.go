package worker

import "context"

// RunCompactPhase runs /compact on an existing session.
// On error, caller must call db.ResetInboxPhase to allow retry.
func RunCompactPhase(ctx context.Context, sessionUUID string) (SpawnResult, error) {
	return SpawnClaudeWithUsage(ctx, sessionUUID, false, "/compact")
}

// RunAnswerPhase replays originalContent on the session after a successful compact.
// Always uses originalContent (the snapshotted inbox.original_content), never inbox.content.
func RunAnswerPhase(ctx context.Context, sessionUUID string, originalContent string) (SpawnResult, error) {
	return SpawnClaudeWithUsage(ctx, sessionUUID, false, originalContent)
}
