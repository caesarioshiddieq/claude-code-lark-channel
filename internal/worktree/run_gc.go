package worktree

import (
	"context"
	"log"
	"os"
	"strconv"
	"time"
)

const (
	envGCTTL     = "IMPLEMENTER_WORKTREE_TTL_HOURS"
	defaultGCTTL = 7 * 24 * time.Hour // 7 days
	gcInterval   = 24 * time.Hour     // run once per day after the first delay
)

// gcTTL returns the configured worktree GC TTL. Reads
// IMPLEMENTER_WORKTREE_TTL_HOURS as an integer hour count; falls back to 7d.
func gcTTL() time.Duration {
	if v := os.Getenv(envGCTTL); v != "" {
		if h, err := strconv.Atoi(v); err == nil && h > 0 {
			return time.Duration(h) * time.Hour
		}
		log.Printf("worktree GC: invalid %s=%q; using default %s", envGCTTL, v, defaultGCTTL)
	}
	return defaultGCTTL
}

// RunGC starts a background goroutine that prunes per-task worktrees whose
// newest descendant mtime is older than the configured TTL. Mirrors the
// goroutine pattern used by gc.RunUsageGC: a kickoff delay gives the boot
// path quiet time, then it runs once per day for the supervisor's lifetime.
//
// The first run fires after a short delay (5 minutes) so a freshly-booted
// supervisor doesn't immediately scan large worktree trees while it's also
// reconciling stale deferrals; subsequent runs fire every 24h.
func RunGC(ctx context.Context, m *Manager) {
	go func() {
		// First run: short delay so boot stays cheap and we don't fight
		// concurrent gnhf spawns that may still be writing to a worktree.
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Minute):
		}
		runOnce(ctx, m)
		ticker := time.NewTicker(gcInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				runOnce(ctx, m)
			}
		}
	}()
}

func runOnce(ctx context.Context, m *Manager) {
	ttl := gcTTL()
	if err := m.GarbageCollect(ctx, ttl); err != nil {
		log.Printf("worktree GC: %v", err)
		return
	}
	log.Printf("worktree GC: pruned worktrees idle longer than %s", ttl)
}
