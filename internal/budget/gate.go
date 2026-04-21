package budget

import (
	"context"
	"crypto/rand"
	"math/big"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"
)

// execCommand is a package-level var for dependency injection in tests.
var execCommand = exec.CommandContext

var jakarta *time.Location

func init() {
	var err error
	jakarta, err = time.LoadLocation("Asia/Jakarta")
	if err != nil {
		panic("budget: cannot load Asia/Jakarta: " + err.Error())
	}
}

func nightStart() int {
	if v := os.Getenv("NIGHT_START"); v != "" {
		if h, err := strconv.Atoi(v); err == nil && h >= 0 && h <= 23 {
			return h
		}
	}
	return 19
}

func nightEnd() int {
	if v := os.Getenv("NIGHT_END"); v != "" {
		if h, err := strconv.Atoi(v); err == nil && h >= 0 && h <= 23 {
			return h
		}
	}
	return 5
}

func resolvedLoc() *time.Location {
	if tz := os.Getenv("TZ"); tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			return l
		}
	}
	return jakarta
}

// IsNightWindow returns true when now falls in [NIGHT_START, 24)∪[0, NIGHT_END) local time.
// Default: 19:00–04:59 Asia/Jakarta.
func IsNightWindow(now time.Time) bool {
	h := now.In(resolvedLoc()).Hour()
	start, end := nightStart(), nightEnd()
	if start > end {
		return h >= start || h < end
	}
	return h >= start && h < end
}

// JitteredNightStart returns tonight's NIGHT_START + rand(0..jitterMin) minutes,
// clamped to the window and pushed to tomorrow if NIGHT_START has already passed.
func JitteredNightStart(now time.Time, jitterMin int) time.Time {
	loc := resolvedLoc()
	local := now.In(loc)
	start, end := nightStart(), nightEnd()

	base := time.Date(local.Year(), local.Month(), local.Day(), start, 0, 0, 0, loc)
	if !base.After(now) {
		base = base.Add(24 * time.Hour)
	}

	endDay := base.Day()
	if end < start {
		endDay++
	}
	windowEnd := time.Date(base.Year(), base.Month(), endDay, end, 0, 0, 0, loc)
	maxJitter := int(windowEnd.Sub(base).Minutes()) - 1
	if maxJitter < 0 {
		maxJitter = 0
	}
	if jitterMin > maxJitter {
		jitterMin = maxJitter
	}
	return base.Add(time.Duration(cryptoRandN(jitterMin+1)) * time.Minute)
}

// ClockHealthy reports NTP sync via timedatectl → chronyc → fail-open.
// Each tool is given a 3-second timeout.
func ClockHealthy(ctx context.Context) bool {
	ctx3, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if ok, avail := tryTimedatectl(ctx3); avail {
		return ok
	}
	if ok, avail := tryChronyc(ctx3); avail {
		return ok
	}
	return true // no sync tool available — fail open
}

func tryTimedatectl(ctx context.Context) (synced, available bool) {
	out, err := execCommand(ctx, "timedatectl", "status").Output()
	if err != nil {
		return false, false
	}
	return strings.Contains(string(out), "synchronized: yes"), true
}

func tryChronyc(ctx context.Context) (synced, available bool) {
	out, err := execCommand(ctx, "chronyc", "tracking").Output()
	if err != nil {
		return false, false
	}
	return strings.Contains(string(out), "System time"), true
}

// CanSpawn returns (true,"") or (false,reason). Human spawns are never gated.
func CanSpawn(ctx context.Context, source string, now time.Time) (bool, string) {
	if source != "autonomous" {
		return true, ""
	}
	if !IsNightWindow(now) {
		return false, "autonomous spawns blocked during day window (05:00–19:00 WIB)"
	}
	if !ClockHealthy(ctx) {
		return false, "clock health check failed: NTP unsynchronized"
	}
	return true, ""
}

func cryptoRandN(n int) int {
	if n <= 0 {
		return 0
	}
	b, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0
	}
	return int(b.Int64())
}
