package gc_test

import (
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/caesarioshiddieq/claude-code-lark-channel/internal/gc"
)

func jakartaTime(year int, month time.Month, day, hour, min, sec int) time.Time {
	loc, _ := time.LoadLocation("Asia/Jakarta")
	return time.Date(year, month, day, hour, min, sec, 0, loc)
}

func TestNextGCTime_BeforeGC(t *testing.T) {
	// 01:00 WIB — next GC is 03:00 WIB same day
	now := jakartaTime(2026, 4, 22, 1, 0, 0)
	got := gc.NextGCTime(now)
	want := jakartaTime(2026, 4, 22, 3, 0, 0)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestNextGCTime_AfterGC(t *testing.T) {
	// 04:00 WIB — already past 03:00, next GC is 03:00 WIB next day
	now := jakartaTime(2026, 4, 22, 4, 0, 0)
	got := gc.NextGCTime(now)
	want := jakartaTime(2026, 4, 23, 3, 0, 0)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestNextGCTime_AtExactGC(t *testing.T) {
	// exactly 03:00:00 WIB — t.After(now) is false, pushes to next day
	now := jakartaTime(2026, 4, 22, 3, 0, 0)
	got := gc.NextGCTime(now)
	want := jakartaTime(2026, 4, 23, 3, 0, 0)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestNextGCTime_UTCInput(t *testing.T) {
	// VM TZ is UTC: 20:00 UTC = 03:00 WIB next day (UTC+7).
	// At exactly GC time → pushes to the day after.
	now := time.Date(2026, 4, 22, 20, 0, 0, 0, time.UTC) // = 03:00 WIB Apr 23
	got := gc.NextGCTime(now)
	loc, _ := time.LoadLocation("Asia/Jakarta")
	wib := got.In(loc)
	if wib.Hour() != 3 || wib.Minute() != 0 || wib.Second() != 0 {
		t.Errorf("got %v WIB, want 03:00:00 WIB", wib)
	}
	if wib.Day() != 24 {
		t.Errorf("got day %d, want 24 (Apr 23 03:00 WIB = exactly now, so next is Apr 24)", wib.Day())
	}
}

func TestNextGCTime_ResultIsAlwaysJakarta(t *testing.T) {
	// Result timezone must be Asia/Jakarta regardless of input TZ
	now := time.Date(2026, 4, 22, 10, 0, 0, 0, time.UTC)
	got := gc.NextGCTime(now)
	loc, _ := time.LoadLocation("Asia/Jakarta")
	if got.Location().String() != loc.String() {
		t.Errorf("result location = %q, want %q", got.Location(), loc)
	}
}
