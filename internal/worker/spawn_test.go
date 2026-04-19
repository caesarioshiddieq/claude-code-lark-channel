package worker_test

import (
	"os"
	"testing"

	"github.com/caesarioshiddieq/claude-code-lark-channel/internal/worker"
)

func TestContentHash_Stable(t *testing.T) {
	h1 := worker.ContentHash("task-1", "hello world")
	h2 := worker.ContentHash("task-1", "hello world")
	if h1 != h2 {
		t.Fatalf("hash not stable: %s vs %s", h1, h2)
	}
	if worker.ContentHash("task-2", "hello world") == h1 {
		t.Fatal("different task_id must produce different hash")
	}
	if worker.ContentHash("task-1", "other") == h1 {
		t.Fatal("different content must produce different hash")
	}
}

func TestLockTask_AcquireRelease(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LOCK_DIR", dir)

	f, err := worker.LockTask("task-abc")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir + "/task-abc/lock"); err != nil {
		t.Fatalf("lock file not created: %v", err)
	}
	worker.UnlockTask(f)
}

func TestParseClaudeOutput_ExtractsResult(t *testing.T) {
	raw := []byte(`{"type":"result","subtype":"success","is_error":false,"result":"hello from claude","session_id":"s1","cost_usd":0.01}`)
	result, err := worker.ParseClaudeOutput(raw)
	if err != nil {
		t.Fatal(err)
	}
	if result != "hello from claude" {
		t.Fatalf("want 'hello from claude', got %q", result)
	}
}

func TestParseClaudeOutput_ErrorCase(t *testing.T) {
	raw := []byte(`{"type":"result","subtype":"error","is_error":true,"result":"something went wrong"}`)
	_, err := worker.ParseClaudeOutput(raw)
	if err == nil {
		t.Fatal("expected error for is_error=true")
	}
}
