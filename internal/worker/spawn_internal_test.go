package worker

import (
	"testing"
)

func TestParseClaudeOutputWithUsage_Normal(t *testing.T) {
	raw := []byte(`{"is_error":false,"result":"hello","usage":{"input_tokens":10,"output_tokens":5,"cache_read_tokens":2}}`)
	got, err := ParseClaudeOutputWithUsage(raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Reply != "hello" {
		t.Errorf("reply=%q", got.Reply)
	}
	if got.InputTokens != 10 {
		t.Errorf("input=%d", got.InputTokens)
	}
	if got.OutputTokens != 5 {
		t.Errorf("output=%d", got.OutputTokens)
	}
	if got.CacheReadTokens != 2 {
		t.Errorf("cache_read=%d", got.CacheReadTokens)
	}
	if got.IsRateLimit {
		t.Error("should not be rate limit")
	}
}

func TestParseClaudeOutputWithUsage_RateLimitInResult(t *testing.T) {
	raw := []byte(`{"is_error":true,"result":"rate limit exceeded, retry after 60s"}`)
	got, err := ParseClaudeOutputWithUsage(raw, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !got.IsRateLimit {
		t.Error("IsRateLimit should be true")
	}
}

func TestParseClaudeOutputWithUsage_RateLimitInStderr(t *testing.T) {
	raw := []byte(`{"is_error":false,"result":"reply"}`)
	got, err := ParseClaudeOutputWithUsage(raw, []byte("429 Too Many Requests"))
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsRateLimit {
		t.Error("IsRateLimit should be true from stderr")
	}
}

func TestParseClaudeOutputWithUsage_GenericError(t *testing.T) {
	raw := []byte(`{"is_error":true,"result":"internal error"}`)
	_, err := ParseClaudeOutputWithUsage(raw, nil)
	if err == nil {
		t.Fatal("expected error")
	}
}
