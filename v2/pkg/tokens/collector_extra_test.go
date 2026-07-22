package tokens

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestNewCollector(t *testing.T) {
	c := NewCollector("/tmp/test-sessions", testLogger())
	if c == nil {
		t.Fatal("NewCollector returned nil")
	}
	if c.sessionsDir != "/tmp/test-sessions" {
		t.Errorf("sessionsDir = %q", c.sessionsDir)
	}
}

func TestCollector_Summary_Initially(t *testing.T) {
	c := NewCollectorWithPersistPath(filepath.Join(t.TempDir(), "missing-sessions"), filepath.Join(t.TempDir(), "missing-summary.json"), testLogger())
	summary := c.Summary()
	if summary != nil {
		t.Errorf("expected nil summary initially, got %v", summary)
	}
}

func TestCollector_IssueCosts(t *testing.T) {
	c := NewCollector("/tmp/test-sessions", testLogger())
	costs := c.IssueCosts()
	if costs == nil {
		t.Fatal("expected non-nil costs map")
	}
	if len(costs) != 0 {
		t.Errorf("expected empty costs map, got %d entries", len(costs))
	}
}

func TestCollectFromDir_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	agg, err := CollectFromDir(dir, DefaultAgentDetector)
	if err != nil {
		t.Fatalf("CollectFromDir: %v", err)
	}
	if agg == nil {
		t.Fatal("expected non-nil aggregate")
	}
	if agg.TotalTokens != 0 {
		t.Errorf("total tokens = %d", agg.TotalTokens)
	}
	if agg.SessionCount != 0 {
		t.Errorf("session count = %d", agg.SessionCount)
	}
}

func TestCollectFromDir_WithSessions(t *testing.T) {
	dir := t.TempDir()

	// Create a valid JSONL session file
	sessionData := `{"type":"usage","model":"sonnet","input_tokens":100,"output_tokens":50}
{"type":"message","role":"user","message":"scan for issues in the repo"}
{"type":"message","role":"assistant","message":"scanning..."}
{"type":"usage","model":"sonnet","input_tokens":200,"output_tokens":75}
`
	os.WriteFile(filepath.Join(dir, "session1.jsonl"), []byte(sessionData), 0644)

	agg, err := CollectFromDir(dir, DefaultAgentDetector)
	if err != nil {
		t.Fatalf("CollectFromDir: %v", err)
	}
	if agg.SessionCount != 1 {
		t.Errorf("session count = %d, want 1", agg.SessionCount)
	}
	if agg.TotalTokens == 0 {
		t.Error("expected non-zero total tokens")
	}
	if agg.Sessions[0].Model != "sonnet" {
		t.Errorf("model = %q", agg.Sessions[0].Model)
	}
	if agg.Sessions[0].Messages != 2 {
		t.Errorf("messages = %d, want 2", agg.Sessions[0].Messages)
	}
}

func TestCollectFromDir_SkipsZeroTokenSessions(t *testing.T) {
	dir := t.TempDir()

	// Session with no token data
	sessionData := `{"type":"message","role":"user","message":"hello"}
{"type":"message","role":"assistant","message":"hi"}
`
	os.WriteFile(filepath.Join(dir, "empty-session.jsonl"), []byte(sessionData), 0644)

	agg, err := CollectFromDir(dir, DefaultAgentDetector)
	if err != nil {
		t.Fatalf("CollectFromDir: %v", err)
	}
	if agg.SessionCount != 0 {
		t.Errorf("session count = %d, want 0 (zero-token sessions skipped)", agg.SessionCount)
	}
}

func TestCollectFromDir_InvalidJSONL(t *testing.T) {
	dir := t.TempDir()

	// Session with mixed valid/invalid lines
	sessionData := `{"type":"usage","model":"sonnet","input_tokens":100,"output_tokens":50}
not-json-line
{"type":"usage","model":"sonnet","input_tokens":100,"output_tokens":50}
`
	os.WriteFile(filepath.Join(dir, "mixed.jsonl"), []byte(sessionData), 0644)

	agg, err := CollectFromDir(dir, DefaultAgentDetector)
	if err != nil {
		t.Fatalf("CollectFromDir: %v", err)
	}
	if agg.SessionCount != 1 {
		t.Errorf("session count = %d", agg.SessionCount)
	}
}

func TestCollectFromDir_CacheTokens(t *testing.T) {
	dir := t.TempDir()

	sessionData := `{"type":"usage","model":"sonnet","input_tokens":100,"output_tokens":50,"cache_creation":25,"cache_read":30}
`
	os.WriteFile(filepath.Join(dir, "cache-session.jsonl"), []byte(sessionData), 0644)

	agg, err := CollectFromDir(dir, DefaultAgentDetector)
	if err != nil {
		t.Fatalf("CollectFromDir: %v", err)
	}
	if agg.SessionCount != 1 {
		t.Fatalf("session count = %d", agg.SessionCount)
	}
	// InputTokens = input_tokens only = 100
	if agg.Sessions[0].InputTokens != 100 {
		t.Errorf("input tokens = %d, want 100", agg.Sessions[0].InputTokens)
	}
	// CacheCreate = 25, CacheRead = 30
	if agg.Sessions[0].CacheCreate != 25 {
		t.Errorf("cache_create = %d, want 25", agg.Sessions[0].CacheCreate)
	}
	if agg.Sessions[0].CacheRead != 30 {
		t.Errorf("cache_read = %d, want 30", agg.Sessions[0].CacheRead)
	}
	// TotalTokens = 100 + 50 + 25 + 30 = 205
	if agg.Sessions[0].TotalTokens != 205 {
		t.Errorf("total tokens = %d, want 205", agg.Sessions[0].TotalTokens)
	}
}

func TestDefaultAgentDetector(t *testing.T) {
	tests := []struct {
		msg  string
		want string
	}{
		{"Please triage this issue", "scanner"},
		{"CI review needed", "ci-maintainer"},
		{"Architecture refactor needed", "architect"},
		{"Community outreach task", "outreach"},
		{"Supervisor sweep check", "supervisor"},
		{"Security vulnerability scan", "sec-check"},
		{"Random unmatched message", "unknown"},
		{"", "unknown"},
	}
	for _, tt := range tests {
		got := DefaultAgentDetector(tt.msg)
		if got != tt.want {
			t.Errorf("DefaultAgentDetector(%q) = %q, want %q", tt.msg, got, tt.want)
		}
	}
}

func TestCollector_Scan(t *testing.T) {
	dir := t.TempDir()
	sessionData := `{"type":"usage","model":"sonnet","input_tokens":100,"output_tokens":50}
{"type":"message","role":"user","message":"triage issue"}
`
	os.WriteFile(filepath.Join(dir, "session1.jsonl"), []byte(sessionData), 0644)

	c := NewCollector(dir, testLogger())
	c.scan()

	summary := c.Summary()
	if summary == nil {
		t.Fatal("expected non-nil summary after scan")
	}
	if summary.TotalTokens == 0 {
		t.Error("expected non-zero total tokens")
	}
	if summary.ByAgent["scanner"] == 0 {
		t.Error("expected scanner agent tokens")
	}
}

func TestCollector_Start(t *testing.T) {
	dir := t.TempDir()
	sessionData := `{"type":"usage","model":"sonnet","input_tokens":100,"output_tokens":50}
{"type":"message","role":"user","message":"triage issue"}
`
	os.WriteFile(filepath.Join(dir, "session1.jsonl"), []byte(sessionData), 0644)

	c := NewCollector(dir, testLogger())
	stop := make(chan struct{})
	go c.Start(stop)

	// Give it time to do an initial scan
	time.Sleep(100 * time.Millisecond)
	close(stop)

	summary := c.Summary()
	if summary == nil {
		t.Fatal("expected non-nil summary after Start")
	}
}

func TestCollector_IssueCosts_WithData(t *testing.T) {
	c := NewCollector("/tmp/nonexistent", testLogger())
	c.mu.Lock()
	c.issueCosts["repo1#1"] = 500
	c.issueCosts["repo1#2"] = 1000
	c.mu.Unlock()

	costs := c.IssueCosts()
	if costs["repo1#1"] != 500 {
		t.Errorf("issue cost = %d", costs["repo1#1"])
	}
	if costs["repo1#2"] != 1000 {
		t.Errorf("issue cost = %d", costs["repo1#2"])
	}
	// Verify it's a copy
	costs["repo1#1"] = 999
	original := c.IssueCosts()
	if original["repo1#1"] != 500 {
		t.Error("IssueCosts should return a copy")
	}
}

func TestCollectFromDir_NilDetector(t *testing.T) {
	dir := t.TempDir()
	sessionData := `{"type":"usage","model":"sonnet","input_tokens":100,"output_tokens":50}
{"type":"message","role":"user","message":"hello"}
`
	os.WriteFile(filepath.Join(dir, "session1.jsonl"), []byte(sessionData), 0644)

	agg, err := CollectFromDir(dir, nil)
	if err != nil {
		t.Fatalf("CollectFromDir: %v", err)
	}
	if agg.SessionCount != 1 {
		t.Errorf("session count = %d", agg.SessionCount)
	}
	if agg.Sessions[0].Agent != "unknown" {
		t.Errorf("agent = %q, want unknown", agg.Sessions[0].Agent)
	}
}
