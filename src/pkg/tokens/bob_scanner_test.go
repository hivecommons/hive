package tokens

import (
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestScanBobSessions_RealSchema(t *testing.T) {
	dir := t.TempDir()
	chatDir := filepath.Join(dir, "tmp", "abc-123", "chats")
	if err := os.MkdirAll(chatDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Read a LITERAL JSON fixture captured from a real bobshell recording.
	// Do NOT marshal a bobChatSession here: that round-trips through the very
	// struct tags under test, so a wrong tag would serialize and deserialize
	// symmetrically and the test would pass against an invented schema. That
	// tautology is exactly how the first version of this scanner shipped
	// green while returning 0 tokens on real data.
	data, err := os.ReadFile(filepath.Join("testdata", "bob_session_real.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chatDir, "session.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	agg, err := ScanBobSessions(dir)
	if err != nil {
		t.Fatal(err)
	}

	// input = 8200 + 4000 = 12200, output = 285 + 150 = 435, total = 12635
	wantTotal := int64(12635)
	if agg.TotalTokens != wantTotal {
		t.Errorf("TotalTokens = %d, want %d", agg.TotalTokens, wantTotal)
	}
	if agg.SessionCount != 1 {
		t.Errorf("SessionCount = %d, want 1", agg.SessionCount)
	}
	if agg.ByAgent["bob"] != wantTotal {
		t.Errorf("ByAgent[bob] = %d, want %d", agg.ByAgent["bob"], wantTotal)
	}
	if agg.ByModel["premium"] != wantTotal {
		t.Errorf("ByModel[premium] = %d, want %d", agg.ByModel["premium"], wantTotal)
	}
	// Cached should be recorded for visibility but NOT added to totals.
	if agg.ByAgentDetail["bob"].CacheRead != 8166+3800 {
		t.Errorf("CacheRead = %d, want %d", agg.ByAgentDetail["bob"].CacheRead, 8166+3800)
	}
	// Session ID from the JSON field, not the path.
	if len(agg.Sessions) != 1 || agg.Sessions[0].SessionID != "f4e80411-test-session" {
		t.Errorf("SessionID = %q, want f4e80411-test-session", agg.Sessions[0].SessionID)
	}
}

func TestScanBobSessions_EstimationFallback(t *testing.T) {
	dir := t.TempDir()
	chatDir := filepath.Join(dir, "tmp", "def-456", "chats")
	if err := os.MkdirAll(chatDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// No tokens field — should estimate from content length.
	sess := bobChatSession{
		SessionID: "est-session",
		Messages: []bobChatMessage{
			{Type: "user", Content: "1234567890123456"},          // 16 chars -> 4 tokens
			{Type: "bob-shell", Content: "12345678901234567890"}, // 20 chars -> 5 tokens
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

func TestScanBobSessions_LegacyUsageFallback(t *testing.T) {
	dir := t.TempDir()
	chatDir := filepath.Join(dir, "tmp", "ghi-789", "chats")
	if err := os.MkdirAll(chatDir, 0o755); err != nil {
		t.Fatal(err)
	}

	sess := bobChatSession{
		SessionID: "legacy-sess",
		Messages: []bobChatMessage{
			{Type: "user", Content: "hello"},
			{Type: "bob-shell", Content: "hi", Model: "standard",
				Usage: &bobLegacyUsage{InputTokens: 200, OutputTokens: 80}},
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

func TestScanBobSessions_PerMessageModel(t *testing.T) {
	dir := t.TempDir()
	chatDir := filepath.Join(dir, "tmp", "model-test", "chats")
	if err := os.MkdirAll(chatDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Two messages with different models; last model wins for the session.
	sess := bobChatSession{
		SessionID: "model-sess",
		Messages: []bobChatMessage{
			{Type: "user", Content: "hi"},
			{Type: "bob-shell", Content: "a", Model: "standard",
				Tokens: &bobTokens{Input: 100, Output: 50}},
			{Type: "bob-shell", Content: "b", Model: "premium",
				Tokens: &bobTokens{Input: 200, Output: 100}},
		},
	}
	data, _ := json.Marshal(sess)
	if err := os.WriteFile(filepath.Join(chatDir, "m.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	agg, err := ScanBobSessions(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Last model seen = "premium"
	if agg.ByModel["premium"] != 450 {
		t.Errorf("ByModel[premium] = %d, want 450", agg.ByModel["premium"])
	}
}

func TestScanBobSessions_AttributesTrustedFolderAgent(t *testing.T) {
	dir := t.TempDir()
	agentPath := "/data/agents/sec-check"
	projectHash := sha256.Sum256([]byte(agentPath))
	hash := hexLower(projectHash[:])

	trusted, _ := json.Marshal(map[string]string{agentPath: "TRUST_FOLDER"})
	if err := os.WriteFile(filepath.Join(dir, "trustedFolders.json"), trusted, 0o644); err != nil {
		t.Fatal(err)
	}
	chatDir := filepath.Join(dir, "tmp", hash, "chats")
	if err := os.MkdirAll(chatDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sess := bobChatSession{
		SessionID:   "agent-session",
		LastUpdated: "2026-08-21T16:33:10.357Z",
		Messages: []bobChatMessage{
			{Type: "user", Content: "scan"},
			{Type: "bob-shell", Content: "done", Model: "premium",
				Tokens: &bobTokens{Input: 1000, Output: 50}},
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
	if agg.ByAgent["sec-check"] != 1050 {
		t.Fatalf("ByAgent[sec-check] = %d, want 1050 (all agents must not collapse under bob)", agg.ByAgent["sec-check"])
	}
	if _, ok := agg.ByAgent["bob"]; ok {
		t.Fatalf("unexpected fallback bob bucket: %+v", agg.ByAgent)
	}
	if len(agg.Sessions) != 1 || agg.Sessions[0].Agent != "sec-check" {
		t.Fatalf("session agent = %+v, want sec-check", agg.Sessions)
	}
	wantLastActive := mustParseMillis(t, "2026-08-21T16:33:10.357Z")
	if agg.Sessions[0].LastActive != wantLastActive {
		t.Fatalf("LastActive = %d, want %d", agg.Sessions[0].LastActive, wantLastActive)
	}
}

func mustParseMillis(t *testing.T, raw string) int64 {
	t.Helper()
	ts, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		t.Fatal(err)
	}
	return ts.UnixMilli()
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
