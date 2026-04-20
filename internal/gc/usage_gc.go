package gc

import (
	"context"
	"log"
	"os"
	"strconv"
	"time"
)

// GCer abstracts the DB delete for usage GC.
type GCer interface {
	DeleteOldTurnUsage(ctx context.Context, olderThanMs int64) (int64, error)
}

func retentionDays() int {
	if v := os.Getenv("USAGE_RETENTION_DAYS"); v != "" {
		if d, err := strconv.Atoi(v); err == nil && d > 0 {
			return d
		}
	}
	return 30
}

func nextGCTime() time.Time {
	now := time.Now()
	t := time.Date(now.Year(), now.Month(), now.Day(), 3, 0, 0, 0, now.Location())
	if !t.After(now) {
		t = t.AddDate(0, 0, 1)
	}
	return t
}

// RunUsageGC starts a goroutine that deletes turn_usage rows older than USAGE_RETENTION_DAYS.
// Runs daily at 03:00 local time.
func RunUsageGC(ctx context.Context, g GCer) {
	go func() {
		for {
			next := nextGCTime()
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Until(next)):
			}
			days := retentionDays()
			cutoff := time.Now().AddDate(0, 0, -days).UnixMilli()
			n, err := g.DeleteOldTurnUsage(ctx, cutoff)
			if err != nil {
				log.Printf("usage GC error: %v", err)
			} else {
				log.Printf("usage GC: deleted %d rows older than %d days", n, days)
			}
		}
	}()
}
