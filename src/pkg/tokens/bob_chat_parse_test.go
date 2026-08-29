package tokens

import (
	"os"
	"path/filepath"
	"testing"
)

func writeBobChatFixture(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParseBobChatFile_ObjectFormat(t *testing.T) {
	path := writeBobChatFixture(t, `{
		"sessionId": "abc-123",
		"projectHash": "deadbeef",
		"messages": [
			{"type": "user", "content": "hi"},
			{"type": "bob-shell", "content": "hello", "tokens": {"input": 10, "output": 5, "cached": 2}}
		]
	}`)

	sess, err := parseBobChatFile(path)
	if err != nil {
		t.Fatalf("parseBobChatFile: %v", err)
	}
	if sess.SessionID != "abc-123" || sess.ProjectHash != "deadbeef" {
		t.Fatalf("session header mismatch: %+v", sess)
	}
	if len(sess.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(sess.Messages))
	}
	in, out, cached := sess.Messages[1].effectiveTokensReal()
	if in != 10 || out != 5 || cached != 2 {
		t.Fatalf("tokens = (%d,%d,%d), want (10,5,2)", in, out, cached)
	}
}

func TestParseBobChatFile_ArrayFormat(t *testing.T) {
	path := writeBobChatFixture(t, `[
		{"role": "user", "content": "hi"},
		{"role": "assistant", "content": "hello",
		 "usage": {"input_tokens": 7, "output_tokens": 3}}
	]`)

	sess, err := parseBobChatFile(path)
	if err != nil {
		t.Fatalf("parseBobChatFile (array format): %v", err)
	}
	if sess.SessionID != "" {
		t.Fatalf("array format should leave the header empty, got %+v", sess)
	}
	if len(sess.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(sess.Messages))
	}
	if got := sess.Messages[0].effectiveType(); got != "user" {
		t.Fatalf("effectiveType role fallback = %q, want user", got)
	}
	in, out, _ := sess.Messages[1].effectiveTokensReal()
	if in != 7 || out != 3 {
		t.Fatalf("legacy usage tokens = (%d,%d), want (7,3)", in, out)
	}
}

func TestParseBobChatFile_InvalidJSONReturnsOriginalError(t *testing.T) {
	path := writeBobChatFixture(t, `{not json at all`)
	if _, err := parseBobChatFile(path); err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestParseBobChatFile_ScalarJSONRejected(t *testing.T) {
	// Valid JSON, but neither an object nor an array of messages.
	path := writeBobChatFixture(t, `42`)
	if _, err := parseBobChatFile(path); err == nil {
		t.Fatal("expected error for scalar JSON")
	}
}

func TestParseBobChatFile_MissingFile(t *testing.T) {
	if _, err := parseBobChatFile(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestEffectiveType_TypeWinsOverRole(t *testing.T) {
	m := &bobChatMessage{Type: "bob-shell", Role: "assistant"}
	if got := m.effectiveType(); got != "bob-shell" {
		t.Fatalf("effectiveType = %q, want bob-shell", got)
	}
}

func TestEffectiveTokensReal_PromptCompletionFallback(t *testing.T) {
	m := &bobChatMessage{Usage: &bobLegacyUsage{PromptTokens: 11, CompletionTokens: 4}}
	in, out, cached := m.effectiveTokensReal()
	if in != 11 || out != 4 || cached != 0 {
		t.Fatalf("tokens = (%d,%d,%d), want (11,4,0)", in, out, cached)
	}
}

func TestEffectiveTokensReal_NoUsage(t *testing.T) {
	m := &bobChatMessage{}
	in, out, cached := m.effectiveTokensReal()
	if in != 0 || out != 0 || cached != 0 {
		t.Fatalf("tokens = (%d,%d,%d), want zeros", in, out, cached)
	}
}
