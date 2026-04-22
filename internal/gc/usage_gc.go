package gc

import (
	"context"
	"log"
	"os"
	"strconv"
	"time"
	_ "time/tzdata"
)

// GCer abstracts the DB delete for usage GC.
type GCer interface {
	DeleteOldTurnUsage(ctx context.Context, olderThanMs int64) (int64, error)
}

var jakarta *time.Location

func init() {
	var err error
	jakarta, err = time.LoadLocation("Asia/Jakarta")
	if err != nil {
		panic("gc: cannot load Asia/Jakarta: " + err.Error())
	}
}

func retentionDays() int {
	if v := os.Getenv("USAGE_RETENTION_DAYS"); v != "" {
		if d, err := strconv.Atoi(v); err == nil && d > 0 {
			return d
		}
	}
	return 30
}

// nextGCTime returns the next 03:00 Asia/Jakarta after now.
func nextGCTime(now time.Time) time.Time {
	local := now.In(jakarta)
	t := time.Date(local.Year(), local.Month(), local.Day(), 3, 0, 0, 0, jakarta)
	if !t.After(now) {
		t = t.AddDate(0, 0, 1)
	}
	return t
}

// RunUsageGC starts a goroutine that deletes turn_usage rows older than USAGE_RETENTION_DAYS.
// Runs daily at 03:00 Asia/Jakarta.
func RunUsageGC(ctx context.Context, g GCer) {
	go func() {
		for {
			next := nextGCTime(time.Now())
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
