package tokens

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestScanBobSessions_ExplicitUsage(t *testing.T) {
	dir := t.TempDir()
	chatDir := filepath.Join(dir, "tmp", "abc-123", "chats")
	if err := os.MkdirAll(chatDir, 0o755); err != nil {
		t.Fatal(err)
	}

	sess := bobChatSession{
		ID:    "sess-1",
		Model: "granite-3.1-8b-instruct",
		Messages: []bobChatMessage{
			{Role: "user", Content: "fix the bug"},
			{Role: "assistant", Content: "done", Usage: bobMsgUsage{InputTokens: 100, OutputTokens: 50}},
		},
	}
	data, _ := json.Marshal(sess)
	if err := os.WriteFile(filepath.Join(chatDir, "session.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	agg, err := ScanBobSessions(dir)
	if err != nil {
		t.Fatal(err)
	}
	if agg.TotalTokens != 150 {
		t.Errorf("TotalTokens = %d, want 150", agg.TotalTokens)
	}
	if agg.SessionCount != 1 {
		t.Errorf("SessionCount = %d, want 1", agg.SessionCount)
	}
	if agg.ByAgent["bob"] != 150 {
		t.Errorf("ByAgent[bob] = %d, want 150", agg.ByAgent["bob"])
	}
	if agg.ByModel["granite-3.1-8b-instruct"] != 150 {
		t.Errorf("ByModel = %d, want 150", agg.ByModel["granite-3.1-8b-instruct"])
	}
}

func TestScanBobSessions_EstimationFallback(t *testing.T) {
	dir := t.TempDir()
	chatDir := filepath.Join(dir, "tmp", "def-456", "chats")
	if err := os.MkdirAll(chatDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// No usage fields — should estimate from content length.
	sess := bobChatSession{
		ID:    "sess-2",
		Model: "granite-3.1-8b-instruct",
		Messages: []bobChatMessage{
			{Role: "user", Content: "1234567890123456"},          // 16 chars -> 4 tokens
			{Role: "assistant", Content: "12345678901234567890"}, // 20 chars -> 5 tokens
		},
	}
	data, _ := json.Marshal(sess)
	if err := os.WriteFile(filepath.Join(chatDir, "chat.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	agg, err := ScanBobSessions(dir)
	if err != nil {
		t.Fatal(err)
	}
	// 16/4 = 4 input, 20/4 = 5 output, total = 9
	if agg.TotalTokens != 9 {
		t.Errorf("TotalTokens = %d, want 9", agg.TotalTokens)
	}
}

func TestScanBobSessions_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	agg, err := ScanBobSessions(dir)
	if err != nil {
		t.Fatal(err)
	}
	if agg.TotalTokens != 0 {
		t.Errorf("TotalTokens = %d, want 0", agg.TotalTokens)
	}
	if agg.SessionCount != 0 {
		t.Errorf("SessionCount = %d, want 0", agg.SessionCount)
	}
}

func TestScanBobSessions_EmptyString(t *testing.T) {
	agg, err := ScanBobSessions("")
	if err != nil {
		t.Fatal(err)
	}
	if agg.TotalTokens != 0 {
		t.Errorf("TotalTokens = %d, want 0", agg.TotalTokens)
	}
}

func TestScanBobSessions_PromptTokensFallback(t *testing.T) {
	dir := t.TempDir()
	chatDir := filepath.Join(dir, "tmp", "ghi-789", "chats")
	if err := os.MkdirAll(chatDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Uses prompt_tokens/completion_tokens instead of input/output.
	sess := bobChatSession{
		ID:    "sess-3",
		Model: "granite-code",
		Messages: []bobChatMessage{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "hi", Usage: bobMsgUsage{PromptTokens: 200, CompletionTokens: 80}},
		},
	}
	data, _ := json.Marshal(sess)
	if err := os.WriteFile(filepath.Join(chatDir, "s.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	agg, err := ScanBobSessions(dir)
	if err != nil {
		t.Fatal(err)
	}
	if agg.TotalTokens != 280 {
		t.Errorf("TotalTokens = %d, want 280", agg.TotalTokens)
	}
}

func TestScanBobSessions_ArrayFormat(t *testing.T) {
	dir := t.TempDir()
	chatDir := filepath.Join(dir, "tmp", "jkl-012", "chats")
	if err := os.MkdirAll(chatDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Alternative format: bare array of messages.
	msgs := []bobChatMessage{
		{Role: "user", Content: "test"},
		{Role: "assistant", Content: "ok", Usage: bobMsgUsage{InputTokens: 10, OutputTokens: 5}},
	}
	data, _ := json.Marshal(msgs)
	if err := os.WriteFile(filepath.Join(chatDir, "alt.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	agg, err := ScanBobSessions(dir)
	if err != nil {
		t.Fatal(err)
	}
	if agg.TotalTokens != 15 {
		t.Errorf("TotalTokens = %d, want 15", agg.TotalTokens)
	}
}

func TestExtractBobSessionID(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/data/home/.bob/tmp/abc-123/chats/session.json", "abc-123"},
		{"/data/home/.bob/tmp/xyz/chats/foo.json", "xyz"},
	}
	for _, tc := range tests {
		got := extractBobSessionID(tc.path)
		if got != tc.want {
			t.Errorf("extractBobSessionID(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}
