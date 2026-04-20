package budget

import (
	"context"
	"log"
	"os"
	"strconv"
	"time"
)

// StuckRowChecker abstracts the DB query for the compact watchdog.
type StuckRowChecker interface {
	ListStuckCompactRows(ctx context.Context, olderThanMs int64) ([]string, error)
}

func watchdogTimeout() time.Duration {
	if v := os.Getenv("COMPACT_WATCHDOG_MIN"); v != "" {
		if m, err := strconv.Atoi(v); err == nil && m > 0 {
			return time.Duration(m) * time.Minute
		}
	}
	return 30 * time.Minute
}

// RunWatchdog starts a goroutine that logs an ALERT for rows stuck in phase=compact.
func RunWatchdog(ctx context.Context, checker StuckRowChecker) {
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				threshold := time.Now().Add(-watchdogTimeout()).UnixMilli()
				ids, err := checker.ListStuckCompactRows(ctx, threshold)
				if err != nil {
					log.Printf("watchdog: query error: %v", err)
					continue
				}
				for _, id := range ids {
					log.Printf("ALERT: inbox %s stuck in phase=compact >%v", id, watchdogTimeout())
				}
			}
		}
	}()
}
