package budget_test

import (
	"os"
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
	ok, reason := budget.CanSpawn("human", jakartaTime(2026, 4, 20, 10, 0, 0))
	if !ok {
		t.Errorf("human spawn should be allowed, got: %s", reason)
	}
}

func TestCanSpawn_Autonomous_BlockedDuringDay(t *testing.T) {
	os.Unsetenv("NIGHT_START")
	os.Unsetenv("NIGHT_END")
	os.Unsetenv("TZ")
	ok, reason := budget.CanSpawn("autonomous", jakartaTime(2026, 4, 20, 10, 0, 0))
	if ok {
		t.Error("autonomous spawn should be blocked during day window")
	}
	if reason == "" {
		t.Error("expected non-empty reason")
	}
}
