package budget_test

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/caesarioshiddieq/claude-code-lark-channel/internal/budget"
)

func jakartaTime(year int, month time.Month, day, hour, min, sec int) time.Time {
	loc, _ := time.LoadLocation("Asia/Jakarta")
	return time.Date(year, month, day, hour, min, sec, 0, loc)
}

func TestIsNightWindow(t *testing.T) {
	os.Unsetenv("NIGHT_START")
	os.Unsetenv("NIGHT_END")
	os.Unsetenv("TZ")

	tests := []struct {
		name string
		t    time.Time
		want bool
	}{
		{"18:59:59 = day", jakartaTime(2026, 4, 20, 18, 59, 59), false},
		{"19:00:00 = night", jakartaTime(2026, 4, 20, 19, 0, 0), true},
		{"23:00:00 = night", jakartaTime(2026, 4, 20, 23, 0, 0), true},
		{"00:00:00 = night", jakartaTime(2026, 4, 21, 0, 0, 0), true},
		{"04:59:59 = night", jakartaTime(2026, 4, 21, 4, 59, 59), true},
		{"05:00:00 = day", jakartaTime(2026, 4, 21, 5, 0, 0), false},
		{"12:00:00 = day", jakartaTime(2026, 4, 20, 12, 0, 0), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := budget.IsNightWindow(tt.t); got != tt.want {
				t.Errorf("IsNightWindow(%v) = %v, want %v", tt.t, got, tt.want)
			}
		})
	}
}

func TestIsNightWindow_UTCInput(t *testing.T) {
	os.Unsetenv("NIGHT_START")
	os.Unsetenv("NIGHT_END")
	os.Unsetenv("TZ")
	// 12:00 UTC = 19:00 WIB
	utc := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)
	if !budget.IsNightWindow(utc) {
		t.Error("12:00 UTC (= 19:00 WIB) should be night window")
	}
}

func TestJitteredNightStart_InRange(t *testing.T) {
	os.Unsetenv("NIGHT_START")
	os.Unsetenv("NIGHT_END")
	os.Unsetenv("TZ")
	now := jakartaTime(2026, 4, 20, 10, 0, 0)
	loc, _ := time.LoadLocation("Asia/Jakarta")
	for i := 0; i < 100; i++ {
		result := budget.JitteredNightStart(now, 30)
		h := result.In(loc).Hour()
		m := result.In(loc).Minute()
		total := h*60 + m
		if total < 19*60 || total > 19*60+30 {
			t.Errorf("iteration %d: got %02d:%02d, want 19:00–19:30", i, h, m)
		}
	}
}

func TestJitteredNightStart_AfterNightStart_PushesTomorrow(t *testing.T) {
	os.Unsetenv("NIGHT_START")
	os.Unsetenv("NIGHT_END")
	os.Unsetenv("TZ")
	now := jakartaTime(2026, 4, 20, 21, 0, 0) // already in night window
	loc, _ := time.LoadLocation("Asia/Jakarta")
	result := budget.JitteredNightStart(now, 30)
	if result.In(loc).Day() != 21 {
		t.Errorf("expected tomorrow (21), got day %d", result.In(loc).Day())
	}
}

func TestCanSpawn_Human_AlwaysAllowed(t *testing.T) {
	os.Unsetenv("NIGHT_START")
	os.Unsetenv("NIGHT_END")
	os.Unsetenv("TZ")
	ok, reason := budget.CanSpawn(context.Background(), "human", jakartaTime(2026, 4, 20, 10, 0, 0))
	if !ok {
		t.Errorf("human spawn should be allowed, got: %s", reason)
	}
}

func TestCanSpawn_Autonomous_BlockedDuringDay(t *testing.T) {
	os.Unsetenv("NIGHT_START")
	os.Unsetenv("NIGHT_END")
	os.Unsetenv("TZ")
	ok, reason := budget.CanSpawn(context.Background(), "autonomous", jakartaTime(2026, 4, 20, 10, 0, 0))
	if ok {
		t.Error("autonomous spawn should be blocked during day window")
	}
	if reason == "" {
		t.Error("expected non-empty reason")
	}
}

func TestClockHealthy_TimedatectlSynced(t *testing.T) {
	restore := budget.SetExecCommand(func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if name == "timedatectl" {
			return exec.Command("echo", "NTP synchronized: yes")
		}
		return exec.Command("false")
	})
	defer restore()
	if !budget.ClockHealthy(context.Background()) {
		t.Error("expected ClockHealthy=true when timedatectl reports synced")
	}
}

func TestClockHealthy_Fallback_Chronyc(t *testing.T) {
	restore := budget.SetExecCommand(func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if name == "timedatectl" {
			return exec.Command("false") // unavailable
		}
		if name == "chronyc" {
			return exec.Command("echo", "System time : 0.000001234 seconds fast of NTP")
		}
		return exec.Command("false")
	})
	defer restore()
	if !budget.ClockHealthy(context.Background()) {
		t.Error("expected ClockHealthy=true when chronyc reports synced")
	}
}

func TestClockHealthy_FailOpen(t *testing.T) {
	restore := budget.SetExecCommand(func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.Command("false") // all tools unavailable
	})
	defer restore()
	if !budget.ClockHealthy(context.Background()) {
		t.Error("expected ClockHealthy=true (fail-open) when no tools available")
	}
}

func TestIsNightWindow_EnvOverride(t *testing.T) {
	t.Setenv("NIGHT_START", "23")
	t.Setenv("NIGHT_END", "3")
	loc, _ := time.LoadLocation("Asia/Jakarta")
	// 23:30 = night with custom window
	if !budget.IsNightWindow(time.Date(2026, 4, 20, 23, 30, 0, 0, loc)) {
		t.Error("23:30 should be night with NIGHT_START=23")
	}
	// 12:00 = day with custom window
	if budget.IsNightWindow(time.Date(2026, 4, 20, 12, 0, 0, 0, loc)) {
		t.Error("12:00 should be day with NIGHT_END=3")
	}
}

func TestIsNightWindow_SameDay(t *testing.T) {
	t.Setenv("NIGHT_START", "8")
	t.Setenv("NIGHT_END", "20")
	loc, _ := time.LoadLocation("Asia/Jakarta")
	// 10:00 = inside same-day window
	if !budget.IsNightWindow(time.Date(2026, 4, 20, 10, 0, 0, 0, loc)) {
		t.Error("10:00 should be night with NIGHT_START=8 NIGHT_END=20 (same-day window)")
	}
	// 21:00 = outside same-day window
	if budget.IsNightWindow(time.Date(2026, 4, 20, 21, 0, 0, 0, loc)) {
		t.Error("21:00 should be day with same-day window ending at 20")
	}
}
