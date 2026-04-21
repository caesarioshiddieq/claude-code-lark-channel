package budget

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"
)

// DeferredRow represents an overdue deferred inbox row.
type DeferredRow struct {
	CommentID  string
	TaskID     string
	DeferCount int
}

// Queuer is the DB interface required by ReconcileStaleDeferrals.
type Queuer interface {
	ListStaleDeferrals(ctx context.Context) ([]DeferredRow, error)
	MarkReadyNow(ctx context.Context, commentID string) error
	RescheduleDeferred(ctx context.Context, commentID string, scheduledFor int64) error
	BumpDeferCount(ctx context.Context, commentID string) error
	MoveToDLQ(ctx context.Context, commentID, taskID, reason string) error
}

func deferCountDLQ() int {
	if v := os.Getenv("DEFER_COUNT_DLQ"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 3
}

func nightJitterMinutes() int {
	if v := os.Getenv("NIGHT_JITTER_MINUTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return 30
}

// ReconcileStaleDeferrals processes overdue deferred inbox rows at supervisor boot.
// In night window: dispatch immediately. In day window: push to tonight + jitter.
// At or beyond DEFER_COUNT_DLQ: move to dead-letter queue.
func ReconcileStaleDeferrals(ctx context.Context, q Queuer, now time.Time) error {
	rows, err := q.ListStaleDeferrals(ctx)
	if err != nil {
		return err
	}
	dlqThreshold := deferCountDLQ()
	jitter := nightJitterMinutes()
	var failCount int

	for _, row := range rows {
		// S7: bail early if context is cancelled.
		if err := ctx.Err(); err != nil {
			return err
		}

		if row.DeferCount >= dlqThreshold {
			if err := q.MoveToDLQ(ctx, row.CommentID, row.TaskID, "max deferrals exceeded"); err != nil {
				log.Printf("reconciler: DLQ move failed for %s, falling back to reschedule: %v", row.CommentID, err)
				failCount++
				// fall through to reschedule path below
			} else {
				continue
			}
		}

		if IsNightWindow(now) {
			if err := q.MarkReadyNow(ctx, row.CommentID); err != nil {
				log.Printf("reconciler: mark-ready failed for %s: %v", row.CommentID, err)
				failCount++
			}
		} else {
			nextNight := JitteredNightStart(now, jitter)
			if err := q.RescheduleDeferred(ctx, row.CommentID, nextNight.UnixMilli()); err != nil {
				log.Printf("reconciler: reschedule failed for %s: %v", row.CommentID, err)
				failCount++
			} else if err := q.BumpDeferCount(ctx, row.CommentID); err != nil {
				log.Printf("reconciler: bump-defer failed for %s: %v", row.CommentID, err)
				failCount++
			}
		}
	}

	if failCount > 0 {
		return fmt.Errorf("reconciler: %d/%d rows had errors", failCount, len(rows))
	}
	return nil
}
