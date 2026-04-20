package budget_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/caesarioshiddieq/claude-code-lark-channel/internal/budget"
)

type fakeQueuer struct {
	stale       []budget.DeferredRow
	readyNow    []string
	rescheduled map[string]int64
	bumped      []string
	dlq         []string
	err         error
}

func (f *fakeQueuer) ListStaleDeferrals(_ context.Context) ([]budget.DeferredRow, error) {
	return f.stale, f.err
}
func (f *fakeQueuer) MarkReadyNow(_ context.Context, id string) error {
	f.readyNow = append(f.readyNow, id)
	return nil
}
func (f *fakeQueuer) RescheduleDeferred(_ context.Context, id string, ts int64) error {
	if f.rescheduled == nil {
		f.rescheduled = make(map[string]int64)
	}
	f.rescheduled[id] = ts
	return nil
}
func (f *fakeQueuer) BumpDeferCount(_ context.Context, id string) error {
	f.bumped = append(f.bumped, id)
	return nil
}
func (f *fakeQueuer) MoveToDLQ(_ context.Context, id, _, _ string) error {
	f.dlq = append(f.dlq, id)
	return nil
}

func TestReconcile_NightWindow_DispatchImmediately(t *testing.T) {
	os.Unsetenv("NIGHT_START")
	os.Unsetenv("NIGHT_END")
	os.Unsetenv("TZ")
	fq := &fakeQueuer{stale: []budget.DeferredRow{{CommentID: "C1", TaskID: "T1"}}}
	night := jakartaTime(2026, 4, 20, 21, 0, 0)
	if err := budget.ReconcileStaleDeferrals(context.Background(), fq, night); err != nil {
		t.Fatal(err)
	}
	if len(fq.readyNow) != 1 || fq.readyNow[0] != "C1" {
		t.Errorf("expected C1 marked ready, got %v", fq.readyNow)
	}
	if len(fq.rescheduled) != 0 {
		t.Error("should not reschedule during night window")
	}
}

func TestReconcile_DayWindow_Reschedule(t *testing.T) {
	os.Unsetenv("NIGHT_START")
	os.Unsetenv("NIGHT_END")
	os.Unsetenv("TZ")
	fq := &fakeQueuer{stale: []budget.DeferredRow{{CommentID: "C2", TaskID: "T2", DeferCount: 1}}}
	day := jakartaTime(2026, 4, 20, 10, 0, 0)
	if err := budget.ReconcileStaleDeferrals(context.Background(), fq, day); err != nil {
		t.Fatal(err)
	}
	if _, ok := fq.rescheduled["C2"]; !ok {
		t.Error("expected C2 rescheduled")
	}
	if len(fq.bumped) != 1 || fq.bumped[0] != "C2" {
		t.Errorf("expected C2 bumped, got %v", fq.bumped)
	}
}

func TestReconcile_DLQ_AtMaxDeferrals(t *testing.T) {
	os.Unsetenv("NIGHT_START")
	os.Unsetenv("NIGHT_END")
	os.Unsetenv("TZ")
	os.Setenv("DEFER_COUNT_DLQ", "3")
	defer os.Unsetenv("DEFER_COUNT_DLQ")
	fq := &fakeQueuer{stale: []budget.DeferredRow{{CommentID: "C3", TaskID: "T3", DeferCount: 3}}}
	if err := budget.ReconcileStaleDeferrals(context.Background(), fq, jakartaTime(2026, 4, 20, 10, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if len(fq.dlq) != 1 || fq.dlq[0] != "C3" {
		t.Errorf("expected C3 in DLQ, got %v", fq.dlq)
	}
	if len(fq.rescheduled) != 0 {
		t.Error("DLQ row should not be rescheduled")
	}
}

func TestReconcile_DBError_ReturnsError(t *testing.T) {
	fq := &fakeQueuer{err: errors.New("db down")}
	if err := budget.ReconcileStaleDeferrals(context.Background(), fq, time.Now()); err == nil {
		t.Error("expected error from DB failure")
	}
}
