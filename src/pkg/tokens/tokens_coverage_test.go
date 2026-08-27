package tokens

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ---------------------------------------------------------------------------
// SetPersistPath
// ---------------------------------------------------------------------------

func TestSetPersistPath(t *testing.T) {
	c := NewCollector(t.TempDir(), discardLogger())
	c.SetPersistPath("/custom/path.json")
	if c.persistPath != "/custom/path.json" {
		t.Errorf("persistPath = %q, want /custom/path.json", c.persistPath)
	}
}

// ---------------------------------------------------------------------------
// SetCopilotSessionsDir
// ---------------------------------------------------------------------------

func TestSetCopilotSessionsDir(t *testing.T) {
	c := NewCollector(t.TempDir(), discardLogger())
	c.SetCopilotSessionsDir("/home/user/.copilot/sessions")
	if c.copilotSessionsDir != "/home/user/.copilot/sessions" {
		t.Errorf("copilotSessionsDir = %q", c.copilotSessionsDir)
	}
}

// ---------------------------------------------------------------------------
// loadSnapshot / saveSnapshot
// ---------------------------------------------------------------------------

func TestLoadSnapshot_ValidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.json")

	agg := &AggregateSummary{
		TotalTokens:  1000,
		SessionCount: 5,
		ByAgent:      map[string]int64{"scanner": 500},
		ByModel:      map[string]int64{"gpt-4": 500},
	}
	data, _ := json.Marshal(agg)
	os.WriteFile(path, data, 0o644)

	c := &Collector{
		persistPath: path,
		logger:      discardLogger(),
	}
	c.loadSnapshot()

	if c.latest == nil {
		t.Fatal("expected latest to be set after loadSnapshot")
	}
	if c.latest.TotalTokens != 1000 {
		t.Errorf("TotalTokens = %d, want 1000", c.latest.TotalTokens)
	}
}

func TestLoadSnapshot_EmptyPath(t *testing.T) {
	c := &Collector{
		persistPath: "",
		logger:      discardLogger(),
	}
	c.loadSnapshot() // should be no-op
	if c.latest != nil {
		t.Error("expected latest to remain nil when path is empty")
	}
}

func TestLoadSnapshot_MissingFile(t *testing.T) {
	c := &Collector{
		persistPath: "/nonexistent/path/snapshot.json",
		logger:      discardLogger(),
	}
	c.loadSnapshot() // should be no-op (file doesn't exist)
	if c.latest != nil {
		t.Error("expected latest to remain nil when file missing")
	}
}

func TestLoadSnapshot_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.json")
	os.WriteFile(path, []byte("not json"), 0o644)

	c := &Collector{
		persistPath: path,
		logger:      discardLogger(),
	}
	c.loadSnapshot()
	if c.latest != nil {
		t.Error("expected latest to remain nil for invalid JSON")
	}
}

func TestLoadSnapshot_NilMapsInitialized(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.json")

	// Write minimal JSON with no maps
	os.WriteFile(path, []byte(`{"total_tokens":100,"session_count":1}`), 0o644)

	c := &Collector{
		persistPath: path,
		logger:      discardLogger(),
	}
	c.loadSnapshot()

	if c.latest == nil {
		t.Fatal("expected latest to be set")
	}
	if c.latest.ByAgent == nil {
		t.Error("ByAgent should be initialized to non-nil map")
	}
	if c.latest.ByModel == nil {
		t.Error("ByModel should be initialized to non-nil map")
	}
	if c.latest.ByAgentDetail == nil {
		t.Error("ByAgentDetail should be initialized to non-nil map")
	}
	if c.latest.ByModelDetail == nil {
		t.Error("ByModelDetail should be initialized to non-nil map")
	}
}

func TestSaveSnapshot_WritesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.json")

	c := &Collector{
		persistPath: path,
		logger:      discardLogger(),
	}

	agg := &AggregateSummary{
		TotalTokens:  2000,
		SessionCount: 3,
		ByAgent:      map[string]int64{},
		ByModel:      map[string]int64{},
	}
	c.saveSnapshot(agg)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read snapshot file: %v", err)
	}

	var loaded AggregateSummary
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("failed to unmarshal snapshot: %v", err)
	}
	if loaded.TotalTokens != 2000 {
		t.Errorf("TotalTokens = %d, want 2000", loaded.TotalTokens)
	}
}

func TestSaveSnapshot_EmptyPath(t *testing.T) {
	c := &Collector{
		persistPath: "",
		logger:      discardLogger(),
	}
	// Should be no-op
	c.saveSnapshot(&AggregateSummary{})
}

func TestSaveSnapshot_NilAgg(t *testing.T) {
	c := &Collector{
		persistPath: "/tmp/test-snapshot.json",
		logger:      discardLogger(),
	}
	// Should be no-op
	c.saveSnapshot(nil)
}

func TestSaveSnapshot_BadDir(t *testing.T) {
	c := &Collector{
		persistPath: "/nonexistent/dir/snapshot.json",
		logger:      discardLogger(),
	}
	// Should log warning but not panic
	c.saveSnapshot(&AggregateSummary{TotalTokens: 100})
}

// ---------------------------------------------------------------------------
// EnhancedAgentDetector
// ---------------------------------------------------------------------------

func TestEnhancedAgentDetector_PathMatch(t *testing.T) {
	detect := EnhancedAgentDetector("/data/agents/scanner/project", nil)
	result := detect("some random first message")
	if result != "scanner" {
		t.Errorf("expected scanner from path, got %q", result)
	}
}

func TestEnhancedAgentDetector_PathMatchCaseInsensitive(t *testing.T) {
	detect := EnhancedAgentDetector("/Data/Agents/Architect/project", nil)
	result := detect("some message")
	if result != "architect" {
		t.Errorf("expected architect from path, got %q", result)
	}
}

func TestEnhancedAgentDetector_FallbackDetector(t *testing.T) {
	fallback := func(msg string) string {
		if strings.Contains(msg, "security") {
			return "sec-check"
		}
		return "unknown"
	}

	detect := EnhancedAgentDetector("/some/other/path", fallback)
	result := detect("run security scan")
	if result != "sec-check" {
		t.Errorf("expected sec-check from fallback, got %q", result)
	}
}

func TestEnhancedAgentDetector_NoMatch(t *testing.T) {
	detect := EnhancedAgentDetector("/some/random/path", nil)
	result := detect("random message")
	if result != "unknown" {
		t.Errorf("expected unknown, got %q", result)
	}
}

func TestEnhancedAgentDetector_AllAgents(t *testing.T) {
	agents := []string{"scanner", "ci-maintainer", "architect", "outreach", "supervisor", "sec-check", "quality", "analyst"}
	for _, agent := range agents {
		detect := EnhancedAgentDetector("/path/to/"+agent+"/dir", nil)
		result := detect("anything")
		if result != agent {
			t.Errorf("agent %q: got %q", agent, result)
		}
	}
}

// ---------------------------------------------------------------------------
// Collector.scan with claude sessions
// ---------------------------------------------------------------------------

func TestCollector_ScanWithClaudeSessions(t *testing.T) {
	sessionsDir := t.TempDir()
	claudeDir := t.TempDir()

	// Create a regular session file
	content := strings.Join([]string{
		`{"role":"user","message":"scanner triage"}`,
		`{"model":"gpt-4","input_tokens":100,"output_tokens":50}`,
	}, "\n") + "\n"
	writeFile(t, sessionsDir, "session1.jsonl", content)

	// Create a Claude session in a project subdirectory
	projDir := filepath.Join(claudeDir, "project-hash")
	os.MkdirAll(projDir, 0o755)
	claudeContent := strings.Join([]string{
		`{"type":"human","timestamp":"2025-06-01T00:00:00Z","message":{"text":"scan for issues"}}`,
		`{"type":"assistant","timestamp":"2025-06-01T00:01:00Z","message":{"model":"claude-sonnet-4-20250514","usage":{"input_tokens":200,"output_tokens":100}}}`,
	}, "\n") + "\n"
	writeFile(t, projDir, "session2.jsonl", claudeContent)

	c := NewCollector(sessionsDir, discardLogger())
	c.SetPersistPath("") // disable persistence
	c.SetClaudeSessionsDir(claudeDir)

	c.scan()

	summary := c.Summary()
	if summary == nil {
		t.Fatal("expected non-nil summary after scan")
	}
	// Should have at least the regular session
	if summary.TotalTokens == 0 {
		t.Error("expected non-zero total tokens after scan")
	}
}

func TestCollector_ScanWithCopilotSessions(t *testing.T) {
	sessionsDir := t.TempDir()

	// Create a regular session file
	content := `{"role":"user","message":"test"}` + "\n" +
		`{"model":"gpt-4","input_tokens":100,"output_tokens":50}` + "\n"
	writeFile(t, sessionsDir, "session1.jsonl", content)

	copilotDir := t.TempDir()

	// Create a copilot session directory with events.jsonl
	sessionSubDir := filepath.Join(copilotDir, "copilot-session-1")
	os.MkdirAll(sessionSubDir, 0o755)
	copilotContent := strings.Join([]string{
		`{"type":"session.start","data":{"sessionId":"abc123","selectedModel":"gpt-4o","context":{"cwd":"/data/agents/scanner"}}}`,
		`{"type":"user.message","timestamp":"2025-06-01T00:00:00Z","data":{"content":"scan for bugs"}}`,
		`{"type":"session.shutdown","data":{"currentModel":"gpt-4o","modelMetrics":{"gpt-4o":{"usage":{"inputTokens":500,"outputTokens":200}}}}}`,
	}, "\n") + "\n"
	writeFile(t, sessionSubDir, "events.jsonl", copilotContent)

	c := NewCollector(sessionsDir, discardLogger())
	c.SetPersistPath("") // disable persistence
	c.SetCopilotSessionsDir(copilotDir)

	c.scan()

	summary := c.Summary()
	if summary == nil {
		t.Fatal("expected non-nil summary after scan")
	}
}

// ---------------------------------------------------------------------------
// Collector.Start (stop channel)
// ---------------------------------------------------------------------------

func TestCollector_Start_StopsOnChannel(t *testing.T) {
	dir := t.TempDir()
	c := NewCollector(dir, discardLogger())
	c.SetPersistPath("") // disable persistence
	c.scanInterval = 50 * time.Millisecond

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Start(stop)
	}()

	time.Sleep(100 * time.Millisecond)
	close(stop)

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Collector.Start did not stop after channel close")
	}
}

// ---------------------------------------------------------------------------
// ScanCopilotSessions
// ---------------------------------------------------------------------------

func TestScanCopilotSessions_EmptyDir(t *testing.T) {
	agg, err := ScanCopilotSessions("", 0)
	if err != nil {
		t.Fatalf("ScanCopilotSessions error: %v", err)
	}
	if agg.SessionCount != 0 {
		t.Errorf("expected 0 sessions for empty dir, got %d", agg.SessionCount)
	}
}

func TestScanCopilotSessions_NonexistentDir(t *testing.T) {
	agg, err := ScanCopilotSessions("/nonexistent/dir", 0)
	if err != nil {
		t.Fatalf("ScanCopilotSessions error: %v", err)
	}
	// Should return empty aggregate, not error
	if agg.SessionCount != 0 {
		t.Errorf("expected 0 sessions, got %d", agg.SessionCount)
	}
}

func TestScanCopilotSessions_WithSessions(t *testing.T) {
	dir := t.TempDir()

	// Create session directory
	sessionDir := filepath.Join(dir, "session-1")
	os.MkdirAll(sessionDir, 0o755)

	content := strings.Join([]string{
		`{"type":"session.start","data":{"sessionId":"test123456789","selectedModel":"gpt-4o","context":{"cwd":"/data/agents/scanner"}}}`,
		`{"type":"user.message","timestamp":"2025-06-01T00:00:00Z","data":{"content":"scan for bugs"}}`,
		`{"type":"assistant.message","timestamp":"2025-06-01T00:01:00Z","data":{}}`,
		`{"type":"tool.execution_complete","data":{"model":"gpt-4o-mini"}}`,
		`{"type":"session.shutdown","data":{"currentModel":"gpt-4o","modelMetrics":{"gpt-4o":{"usage":{"inputTokens":500,"outputTokens":200,"cacheReadTokens":100,"cacheWriteTokens":50}}}}}`,
	}, "\n") + "\n"
	writeFile(t, sessionDir, "events.jsonl", content)

	agg, err := ScanCopilotSessions(dir, 0)
	if err != nil {
		t.Fatalf("ScanCopilotSessions error: %v", err)
	}

	if agg.SessionCount != 1 {
		t.Fatalf("expected 1 session, got %d", agg.SessionCount)
	}
	if agg.TotalInput != 500 {
		t.Errorf("TotalInput = %d, want 500", agg.TotalInput)
	}
	if agg.TotalOutput != 200 {
		t.Errorf("TotalOutput = %d, want 200", agg.TotalOutput)
	}
	if agg.TotalCacheRead != 100 {
		t.Errorf("TotalCacheRead = %d, want 100", agg.TotalCacheRead)
	}
}

// TestScanCopilotSessions_PreCaptureSessionStillCounts is the regression guard
// for the $18k->$394 bug: a Copilot session that ENDED BEFORE live capture
// started was never sniffed by the proxy sink, so its session.shutdown tokens
// MUST still be counted even though live capture is enabled. Only sessions
// active at/after the capture start are deferred to the sink.
func TestScanCopilotSessions_PreCaptureSessionStillCounts(t *testing.T) {
	dir := t.TempDir()
	sessionDir := filepath.Join(dir, "session-old")
	os.MkdirAll(sessionDir, 0o755)

	// Session activity is 2025-06-01 (well in the past).
	content := strings.Join([]string{
		`{"type":"session.start","data":{"sessionId":"old123456789","selectedModel":"gpt-4o","context":{"cwd":"/data/agents/scanner"}}}`,
		`{"type":"user.message","timestamp":"2025-06-01T00:00:00Z","data":{"content":"scan"}}`,
		`{"type":"assistant.message","timestamp":"2025-06-01T00:01:00Z","data":{}}`,
		`{"type":"session.shutdown","data":{"currentModel":"gpt-4o","modelMetrics":{"gpt-4o":{"usage":{"inputTokens":500,"outputTokens":200,"cacheReadTokens":100,"cacheWriteTokens":50}}}}}`,
	}, "\n") + "\n"
	writeFile(t, sessionDir, "events.jsonl", content)

	// Live capture "started" in 2026 — AFTER this session ended. Its tokens
	// must NOT be dropped.
	sinceMs := int64(1767225600000) // 2026-01-01
	agg, err := ScanCopilotSessions(dir, sinceMs)
	if err != nil {
		t.Fatalf("ScanCopilotSessions error: %v", err)
	}
	if agg.TotalInput != 500 || agg.TotalOutput != 200 {
		t.Fatalf("pre-capture session tokens were dropped: input=%d output=%d, want 500/200 (the $18k->$394 regression)", agg.TotalInput, agg.TotalOutput)
	}
}

// TestScanCopilotSessions_LiveCaptureSkipsTokens verifies that when live capture
// is on (the MITM proxy records Copilot usage per response), the session-shutdown
// scanner does NOT accrue tokens again — otherwise every Copilot session would be
// double-counted. It must still report the session, its messages, and the model.
func TestScanCopilotSessions_LiveCaptureSkipsTokens(t *testing.T) {
	dir := t.TempDir()
	sessionDir := filepath.Join(dir, "session-live")
	os.MkdirAll(sessionDir, 0o755)

	content := strings.Join([]string{
		`{"type":"session.start","data":{"sessionId":"live123456789","selectedModel":"gpt-5","context":{"cwd":"/data/agents/scanner"}}}`,
		`{"type":"user.message","timestamp":"2025-06-01T00:00:00Z","data":{"content":"scan for bugs"}}`,
		`{"type":"assistant.message","timestamp":"2025-06-01T00:01:00Z","data":{}}`,
		`{"type":"session.shutdown","data":{"currentModel":"gpt-5","modelMetrics":{"gpt-5":{"usage":{"inputTokens":500,"outputTokens":200,"cacheReadTokens":100,"cacheWriteTokens":50}}}}}`,
	}, "\n") + "\n"
	writeFile(t, sessionDir, "events.jsonl", content)

	// liveCapture=true: tokens must be zero, but the session is still counted
	// (it has messages) and the model is preserved.
	agg, err := ScanCopilotSessions(dir, 1)
	if err != nil {
		t.Fatalf("ScanCopilotSessions error: %v", err)
	}
	if agg.SessionCount != 1 {
		t.Fatalf("expected 1 session, got %d", agg.SessionCount)
	}
	if agg.TotalInput != 0 || agg.TotalOutput != 0 || agg.TotalCacheRead != 0 || agg.TotalCacheCreate != 0 {
		t.Errorf("live capture must skip token accrual; got in=%d out=%d cr=%d cc=%d",
			agg.TotalInput, agg.TotalOutput, agg.TotalCacheRead, agg.TotalCacheCreate)
	}
	if agg.TotalMessages != 2 {
		t.Errorf("TotalMessages = %d, want 2 (messages still counted)", agg.TotalMessages)
	}
	if _, ok := agg.ByModel["gpt-5"]; !ok {
		t.Errorf("model gpt-5 should still be present as a row, got %+v", agg.ByModel)
	}

	// Same file, liveCapture=false: tokens ARE accrued (proves the flag gates it).
	agg2, err := ScanCopilotSessions(dir, 0)
	if err != nil {
		t.Fatalf("ScanCopilotSessions error: %v", err)
	}
	if agg2.TotalInput != 500 || agg2.TotalOutput != 200 {
		t.Errorf("non-live capture should accrue tokens; got in=%d out=%d", agg2.TotalInput, agg2.TotalOutput)
	}
}

func TestScanCopilotSessions_SkipsNonDirectories(t *testing.T) {
	dir := t.TempDir()

	// Create a regular file (not a directory)
	writeFile(t, dir, "not-a-dir.txt", "hello")

	agg, err := ScanCopilotSessions(dir, 0)
	if err != nil {
		t.Fatalf("ScanCopilotSessions error: %v", err)
	}
	if agg.SessionCount != 0 {
		t.Errorf("expected 0 sessions, got %d", agg.SessionCount)
	}
}

func TestScanCopilotSessions_SkipsOldSessions(t *testing.T) {
	dir := t.TempDir()
	sessionDir := filepath.Join(dir, "old-session")
	os.MkdirAll(sessionDir, 0o755)

	content := `{"type":"session.shutdown","data":{"modelMetrics":{"gpt-4":{"usage":{"inputTokens":100,"outputTokens":50}}}}}` + "\n"
	path := filepath.Join(sessionDir, "events.jsonl")
	os.WriteFile(path, []byte(content), 0o644)

	// Set modification time to 60 days ago (beyond maxSessionAgeDays=30)
	oldTime := time.Now().Add(-60 * 24 * time.Hour)
	os.Chtimes(path, oldTime, oldTime)

	agg, err := ScanCopilotSessions(dir, 0)
	if err != nil {
		t.Fatalf("ScanCopilotSessions error: %v", err)
	}
	if agg.SessionCount != 0 {
		t.Errorf("expected 0 sessions (old file), got %d", agg.SessionCount)
	}
}

// ---------------------------------------------------------------------------
// detectAgentFromCwd
// ---------------------------------------------------------------------------

func TestDetectAgentFromCwd_Match(t *testing.T) {
	got := detectAgentFromCwd("/data/agents/scanner/project")
	if got != "scanner" {
		t.Errorf("got %q, want scanner", got)
	}
}

func TestDetectAgentFromCwd_NoMatch(t *testing.T) {
	got := detectAgentFromCwd("/home/user/project")
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestDetectAgentFromCwd_NestedPath(t *testing.T) {
	got := detectAgentFromCwd("/home/data/agents/architect/subdir/project")
	if got != "architect" {
		t.Errorf("got %q, want architect", got)
	}
}

func TestDetectAgentFromCwd_EmptyAgent(t *testing.T) {
	got := detectAgentFromCwd("/data/agents/")
	if got != "" {
		t.Errorf("got %q, want empty for trailing slash only", got)
	}
}

// ---------------------------------------------------------------------------
// parseCopilotSessionFile — message-based agent detection
// ---------------------------------------------------------------------------

func TestParseCopilotSessionFile_AgentFromMessage(t *testing.T) {
	dir := t.TempDir()

	content := strings.Join([]string{
		`{"type":"session.start","data":{"sessionId":"abc","selectedModel":"gpt-4o","context":{"cwd":"/home/user"}}}`,
		`{"type":"user.message","timestamp":"2025-06-01T00:00:00Z","data":{"content":"scanner please triage all open bugs"}}`,
		`{"type":"session.shutdown","data":{"modelMetrics":{"gpt-4o":{"usage":{"inputTokens":100,"outputTokens":50}}}}}`,
	}, "\n") + "\n"
	path := writeFile(t, dir, "events.jsonl", content)

	summary, err := parseCopilotSessionFile(path)
	if err != nil {
		t.Fatalf("parseCopilotSessionFile error: %v", err)
	}
	if summary.Agent != "scanner" {
		t.Errorf("agent = %q, want scanner", summary.Agent)
	}
}

func TestParseCopilotSessionFile_AgentFromCwd(t *testing.T) {
	dir := t.TempDir()

	content := strings.Join([]string{
		`{"type":"session.start","data":{"sessionId":"abc","selectedModel":"gpt-4o","context":{"cwd":"/data/agents/architect"}}}`,
		`{"type":"session.shutdown","data":{"modelMetrics":{"gpt-4o":{"usage":{"inputTokens":100,"outputTokens":50}}}}}`,
	}, "\n") + "\n"
	path := writeFile(t, dir, "events.jsonl", content)

	summary, err := parseCopilotSessionFile(path)
	if err != nil {
		t.Fatalf("parseCopilotSessionFile error: %v", err)
	}
	if summary.Agent != "architect" {
		t.Errorf("agent = %q, want architect", summary.Agent)
	}
}

func TestParseCopilotSessionFile_MaxAgentScan(t *testing.T) {
	dir := t.TempDir()

	// Create a session with many user messages, none matching any agent keyword
	var lines []string
	lines = append(lines, `{"type":"session.start","data":{"sessionId":"abc","selectedModel":"gpt-4o","context":{"cwd":"/home/user"}}}`)
	for i := 0; i < 10; i++ {
		lines = append(lines, `{"type":"user.message","timestamp":"2025-06-01T00:00:00Z","data":{"content":"random message with no agent keywords"}}`)
	}
	lines = append(lines, `{"type":"session.shutdown","data":{"modelMetrics":{"gpt-4o":{"usage":{"inputTokens":100,"outputTokens":50}}}}}`)

	content := strings.Join(lines, "\n") + "\n"
	path := writeFile(t, dir, "events.jsonl", content)

	summary, err := parseCopilotSessionFile(path)
	if err != nil {
		t.Fatalf("parseCopilotSessionFile error: %v", err)
	}
	// Should stop trying after maxAgentScan=5 messages
	if summary.Agent != "unknown" {
		t.Errorf("agent = %q, want unknown", summary.Agent)
	}
}

func TestParseCopilotSessionFile_NoTokens(t *testing.T) {
	dir := t.TempDir()

	// Session with no shutdown metrics
	content := `{"type":"session.start","data":{"sessionId":"abc","selectedModel":"gpt-4o"}}` + "\n"
	path := writeFile(t, dir, "events.jsonl", content)

	summary, err := parseCopilotSessionFile(path)
	if err != nil {
		t.Fatalf("parseCopilotSessionFile error: %v", err)
	}
	if summary.TotalTokens != 0 {
		t.Errorf("TotalTokens = %d, want 0", summary.TotalTokens)
	}
}

// ---------------------------------------------------------------------------
// Collector.scan error paths
// ---------------------------------------------------------------------------

func TestCollector_ScanBadDir(t *testing.T) {
	c := NewCollector("/nonexistent/dir", discardLogger())
	c.SetPersistPath("")
	c.scan()
	// Should not panic; summary may be nil since scan failed
}

func TestCollector_ScanBadClaudeDir(t *testing.T) {
	sessionsDir := t.TempDir()
	c := NewCollector(sessionsDir, discardLogger())
	c.SetPersistPath("")
	c.SetClaudeSessionsDir("/nonexistent/claude/dir")
	c.scan()
	// Should not panic; claude scan failure is logged and ignored
}

func TestCollector_ScanBadCopilotDir(t *testing.T) {
	sessionsDir := t.TempDir()
	c := NewCollector(sessionsDir, discardLogger())
	c.SetPersistPath("")
	c.SetCopilotSessionsDir("/nonexistent/copilot/dir")
	c.scan()
	// Should not panic
}
