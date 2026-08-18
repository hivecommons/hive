package tokens

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// detectAgentFromCwd
// ---------------------------------------------------------------------------

func TestDetectAgentFromCwd(t *testing.T) {
	tests := []struct {
		name string
		cwd  string
		want string
	}{
		{"standard agent path", "/data/agents/quality", "quality"},
		{"nested agent path", "/data/agents/sec-check/subdir", "sec-check"},
		{"trailing slash", "/data/agents/supervisor/", "supervisor"},
		{"no agent prefix", "/home/user/projects", ""},
		{"empty string", "", ""},
		{"prefix only no agent", "/data/agents/", ""},
		{"embedded prefix", "/var/run/data/agents/scanner/work", "scanner"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectAgentFromCwd(tt.cwd)
			if got != tt.want {
				t.Errorf("detectAgentFromCwd(%q) = %q, want %q", tt.cwd, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// parseCopilotSessionFile
// ---------------------------------------------------------------------------

func TestParseCopilotSessionFile_FullLifecycle(t *testing.T) {
	dir := t.TempDir()
	events := []copilotEvent{
		{
			Type: "session.start",
			Data: mustMarshal(t, copilotSessionStart{
				SessionID:     "abcdef123456789",
				SelectedModel: "gpt-4o",
				Context:       copilotContext{Cwd: "/data/agents/quality"},
			}),
		},
		{
			Type:      "user.message",
			Timestamp: "2026-08-10T12:00:00.000Z",
			Data:      mustMarshal(t, copilotUserMessage{Content: "analyze coverage gaps"}),
		},
		{
			Type:      "assistant.message",
			Timestamp: "2026-08-10T12:00:05.000Z",
			Data:      json.RawMessage(`{}`),
		},
		{
			Type: "tool.execution_complete",
			Data: mustMarshal(t, copilotToolComplete{Model: "claude-sonnet-4-20250514"}),
		},
		{
			Type: "session.shutdown",
			Data: mustMarshal(t, copilotShutdown{
				CurrentModel: "claude-sonnet-4-20250514",
				ModelMetrics: map[string]copilotModelMetric{
					"claude-sonnet-4-20250514": {
						Usage: copilotUsage{
							InputTokens:      1000,
							OutputTokens:     500,
							CacheReadTokens:  200,
							CacheWriteTokens: 100,
						},
					},
				},
			}),
		},
	}

	path := writeEventsFile(t, dir, events)

	summary, err := parseCopilotSessionFile(path)
	if err != nil {
		t.Fatalf("parseCopilotSessionFile: %v", err)
	}

	if summary.SessionID != "abcdef123456" {
		t.Errorf("SessionID = %q, want truncated to 12 chars", summary.SessionID)
	}
	if summary.Agent != "quality" {
		t.Errorf("Agent = %q, want 'quality'", summary.Agent)
	}
	if summary.Model != "claude-sonnet-4-20250514" {
		t.Errorf("Model = %q, want 'claude-sonnet-4-20250514'", summary.Model)
	}
	if summary.Messages != 2 {
		t.Errorf("Messages = %d, want 2", summary.Messages)
	}
	if summary.InputTokens != 1000 {
		t.Errorf("InputTokens = %d, want 1000", summary.InputTokens)
	}
	if summary.OutputTokens != 500 {
		t.Errorf("OutputTokens = %d, want 500", summary.OutputTokens)
	}
	if summary.CacheRead != 200 {
		t.Errorf("CacheRead = %d, want 200", summary.CacheRead)
	}
	if summary.CacheCreate != 100 {
		t.Errorf("CacheCreate = %d, want 100", summary.CacheCreate)
	}
	if summary.TotalTokens != 1800 {
		t.Errorf("TotalTokens = %d, want 1800", summary.TotalTokens)
	}
	if summary.LastActive == 0 {
		t.Error("LastActive should be set from message timestamps")
	}
}

func TestParseCopilotSessionFile_MalformedLinesSkipped(t *testing.T) {
	dir := t.TempDir()
	content := "not json at all\n" +
		`{"type":"session.start","data":{"sessionId":"sess123","selectedModel":"gpt-4","context":{"cwd":"/data/agents/scanner"}}}` + "\n" +
		"{broken json\n" +
		`{"type":"user.message","timestamp":"2026-01-01T00:00:00Z","data":{"content":"hello"}}` + "\n"

	path := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	summary, err := parseCopilotSessionFile(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if summary.Agent != "scanner" {
		t.Errorf("Agent = %q, want 'scanner'", summary.Agent)
	}
	if summary.Messages != 1 {
		t.Errorf("Messages = %d, want 1 (malformed lines skipped)", summary.Messages)
	}
}

func TestParseCopilotSessionFile_AgentDetectionFromMessages(t *testing.T) {
	// When CWD doesn't reveal agent, fall back to message content detection.
	dir := t.TempDir()
	events := []copilotEvent{
		{
			Type: "session.start",
			Data: mustMarshal(t, copilotSessionStart{
				SessionID:     "xyz999",
				SelectedModel: "gpt-4",
				Context:       copilotContext{Cwd: "/home/user"},
			}),
		},
		{
			Type:      "user.message",
			Timestamp: "2026-01-01T00:00:00Z",
			Data:      mustMarshal(t, copilotUserMessage{Content: "generic message no agent hint"}),
		},
	}

	path := writeEventsFile(t, dir, events)
	summary, err := parseCopilotSessionFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Agent stays "unknown" if CWD and messages don't match
	if summary.Agent == "" {
		t.Error("Agent should not be empty (defaults to 'unknown')")
	}
}

func TestParseCopilotSessionFile_MultipleModelMetrics(t *testing.T) {
	dir := t.TempDir()
	events := []copilotEvent{
		{
			Type: "session.start",
			Data: mustMarshal(t, copilotSessionStart{
				SessionID:     "multi-model",
				SelectedModel: "gpt-4",
				Context:       copilotContext{Cwd: "/data/agents/ci"},
			}),
		},
		{
			Type: "session.shutdown",
			Data: mustMarshal(t, copilotShutdown{
				CurrentModel: "claude-sonnet-4-20250514",
				ModelMetrics: map[string]copilotModelMetric{
					"gpt-4": {Usage: copilotUsage{InputTokens: 100, OutputTokens: 50}},
					"claude-sonnet-4-20250514": {Usage: copilotUsage{InputTokens: 200, OutputTokens: 100}},
				},
			}),
		},
	}

	path := writeEventsFile(t, dir, events)
	summary, err := parseCopilotSessionFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Tokens from all models are summed
	if summary.InputTokens != 300 {
		t.Errorf("InputTokens = %d, want 300 (sum of both models)", summary.InputTokens)
	}
	if summary.OutputTokens != 150 {
		t.Errorf("OutputTokens = %d, want 150 (sum of both models)", summary.OutputTokens)
	}
}

// ---------------------------------------------------------------------------
// ScanCopilotSessions
// ---------------------------------------------------------------------------

func TestScanCopilotSessions_EmptyDirReturnsZero(t *testing.T) {
	dir := t.TempDir()
	agg, err := ScanCopilotSessions(dir, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if agg.SessionCount != 0 {
		t.Errorf("SessionCount = %d, want 0", agg.SessionCount)
	}
}

func TestScanCopilotSessions_EmptyStringPath(t *testing.T) {
	agg, err := ScanCopilotSessions("", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if agg.SessionCount != 0 {
		t.Errorf("SessionCount = %d, want 0", agg.SessionCount)
	}
}

func TestScanCopilotSessions_MissingDirReturnsZero(t *testing.T) {
	agg, err := ScanCopilotSessions("/nonexistent/path/xyz", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if agg.SessionCount != 0 {
		t.Errorf("SessionCount = %d, want 0 for nonexistent dir", agg.SessionCount)
	}
}

func TestScanCopilotSessions_AggregatesMultipleSessions(t *testing.T) {
	dir := t.TempDir()

	// Session 1: quality agent
	sess1Dir := filepath.Join(dir, "session-001")
	os.MkdirAll(sess1Dir, 0o755)
	writeEventsFileAt(t, sess1Dir, []copilotEvent{
		{Type: "session.start", Data: mustMarshal(t, copilotSessionStart{
			SessionID: "s1", SelectedModel: "gpt-4", Context: copilotContext{Cwd: "/data/agents/quality"},
		})},
		{Type: "user.message", Timestamp: "2026-08-10T10:00:00Z", Data: mustMarshal(t, copilotUserMessage{Content: "hi"})},
		{Type: "session.shutdown", Data: mustMarshal(t, copilotShutdown{
			ModelMetrics: map[string]copilotModelMetric{
				"gpt-4": {Usage: copilotUsage{InputTokens: 500, OutputTokens: 200}},
			},
		})},
	})

	// Session 2: scanner agent
	sess2Dir := filepath.Join(dir, "session-002")
	os.MkdirAll(sess2Dir, 0o755)
	writeEventsFileAt(t, sess2Dir, []copilotEvent{
		{Type: "session.start", Data: mustMarshal(t, copilotSessionStart{
			SessionID: "s2", SelectedModel: "claude-sonnet-4-20250514", Context: copilotContext{Cwd: "/data/agents/scanner"},
		})},
		{Type: "user.message", Timestamp: "2026-08-10T11:00:00Z", Data: mustMarshal(t, copilotUserMessage{Content: "scan"})},
		{Type: "session.shutdown", Data: mustMarshal(t, copilotShutdown{
			ModelMetrics: map[string]copilotModelMetric{
				"claude-sonnet-4-20250514": {Usage: copilotUsage{InputTokens: 300, OutputTokens: 100}},
			},
		})},
	})

	agg, err := ScanCopilotSessions(dir, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if agg.SessionCount != 2 {
		t.Errorf("SessionCount = %d, want 2", agg.SessionCount)
	}
	if agg.TotalInput != 800 {
		t.Errorf("TotalInput = %d, want 800", agg.TotalInput)
	}
	if agg.TotalOutput != 300 {
		t.Errorf("TotalOutput = %d, want 300", agg.TotalOutput)
	}
	if agg.TotalMessages != 2 {
		t.Errorf("TotalMessages = %d, want 2", agg.TotalMessages)
	}
	if agg.ByAgent["quality"] != 700 {
		t.Errorf("ByAgent[quality] = %d, want 700", agg.ByAgent["quality"])
	}
	if agg.ByAgent["scanner"] != 400 {
		t.Errorf("ByAgent[scanner] = %d, want 400", agg.ByAgent["scanner"])
	}
}

func TestScanCopilotSessions_DoubleCountAvoidance(t *testing.T) {
	dir := t.TempDir()

	// Simulate liveCaptureSinceMs = a time BEFORE the session's last activity
	// So the session was active after live capture started → tokens should be zeroed.
	liveCaptureSince := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC).UnixMilli()

	sessDir := filepath.Join(dir, "session-live")
	os.MkdirAll(sessDir, 0o755)
	writeEventsFileAt(t, sessDir, []copilotEvent{
		{Type: "session.start", Data: mustMarshal(t, copilotSessionStart{
			SessionID: "live1", SelectedModel: "gpt-4", Context: copilotContext{Cwd: "/data/agents/quality"},
		})},
		{Type: "user.message", Timestamp: "2026-08-10T10:00:00Z", Data: mustMarshal(t, copilotUserMessage{Content: "work"})},
		{Type: "session.shutdown", Data: mustMarshal(t, copilotShutdown{
			ModelMetrics: map[string]copilotModelMetric{
				"gpt-4": {Usage: copilotUsage{InputTokens: 1000, OutputTokens: 500}},
			},
		})},
	})

	agg, err := ScanCopilotSessions(dir, liveCaptureSince)
	if err != nil {
		t.Fatal(err)
	}

	// Tokens zeroed (proxy captured them live) but messages kept
	if agg.TotalTokens != 0 {
		t.Errorf("TotalTokens = %d, want 0 (double-count avoidance)", agg.TotalTokens)
	}
	if agg.TotalMessages != 1 {
		t.Errorf("TotalMessages = %d, want 1 (messages always counted)", agg.TotalMessages)
	}
}

func TestScanCopilotSessions_OldSessionKeepsTokens(t *testing.T) {
	dir := t.TempDir()

	// liveCaptureSince is AFTER the session ended → tokens should be kept
	liveCaptureSince := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC).UnixMilli()

	sessDir := filepath.Join(dir, "session-old")
	os.MkdirAll(sessDir, 0o755)
	writeEventsFileAt(t, sessDir, []copilotEvent{
		{Type: "session.start", Data: mustMarshal(t, copilotSessionStart{
			SessionID: "old1", SelectedModel: "gpt-4", Context: copilotContext{Cwd: "/data/agents/quality"},
		})},
		{Type: "user.message", Timestamp: "2026-08-10T10:00:00Z", Data: mustMarshal(t, copilotUserMessage{Content: "work"})},
		{Type: "session.shutdown", Data: mustMarshal(t, copilotShutdown{
			ModelMetrics: map[string]copilotModelMetric{
				"gpt-4": {Usage: copilotUsage{InputTokens: 1000, OutputTokens: 500}},
			},
		})},
	})

	agg, err := ScanCopilotSessions(dir, liveCaptureSince)
	if err != nil {
		t.Fatal(err)
	}

	// Tokens kept (session ended before live capture started)
	if agg.TotalTokens != 1500 {
		t.Errorf("TotalTokens = %d, want 1500 (session predates live capture)", agg.TotalTokens)
	}
}

func TestScanCopilotSessions_SkipsNonDirEntries(t *testing.T) {
	dir := t.TempDir()
	// Create a file (not a directory) in the sessions dir
	os.WriteFile(filepath.Join(dir, "not-a-session.txt"), []byte("hello"), 0o644)

	agg, err := ScanCopilotSessions(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if agg.SessionCount != 0 {
		t.Errorf("SessionCount = %d, want 0 (non-dirs skipped)", agg.SessionCount)
	}
}

func TestScanCopilotSessions_ByAgentDetail(t *testing.T) {
	dir := t.TempDir()

	sessDir := filepath.Join(dir, "session-detail")
	os.MkdirAll(sessDir, 0o755)
	writeEventsFileAt(t, sessDir, []copilotEvent{
		{Type: "session.start", Data: mustMarshal(t, copilotSessionStart{
			SessionID: "d1", SelectedModel: "gpt-4", Context: copilotContext{Cwd: "/data/agents/quality"},
		})},
		{Type: "user.message", Timestamp: "2026-08-10T10:00:00Z", Data: mustMarshal(t, copilotUserMessage{Content: "check"})},
		{Type: "session.shutdown", Data: mustMarshal(t, copilotShutdown{
			ModelMetrics: map[string]copilotModelMetric{
				"gpt-4": {Usage: copilotUsage{InputTokens: 100, OutputTokens: 50, CacheReadTokens: 25, CacheWriteTokens: 10}},
			},
		})},
	})

	agg, err := ScanCopilotSessions(dir, 0)
	if err != nil {
		t.Fatal(err)
	}

	detail := agg.ByAgentDetail["quality"]
	if detail == nil {
		t.Fatal("ByAgentDetail[quality] is nil")
	}
	if detail.Input != 100 {
		t.Errorf("detail.Input = %d, want 100", detail.Input)
	}
	if detail.Output != 50 {
		t.Errorf("detail.Output = %d, want 50", detail.Output)
	}
	if detail.CacheRead != 25 {
		t.Errorf("detail.CacheRead = %d, want 25", detail.CacheRead)
	}
	if detail.CacheCreate != 10 {
		t.Errorf("detail.CacheCreate = %d, want 10", detail.CacheCreate)
	}
	if detail.Messages != 1 {
		t.Errorf("detail.Messages = %d, want 1", detail.Messages)
	}
	if detail.Sessions != 1 {
		t.Errorf("detail.Sessions = %d, want 1", detail.Sessions)
	}
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func mustMarshal(t *testing.T, v interface{}) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("mustMarshal: %v", err)
	}
	return json.RawMessage(b)
}

func writeEventsFile(t *testing.T, dir string, events []copilotEvent) string {
	t.Helper()
	path := filepath.Join(dir, "events.jsonl")
	return writeEventsToPath(t, path, events)
}

func writeEventsFileAt(t *testing.T, dir string, events []copilotEvent) {
	t.Helper()
	path := filepath.Join(dir, "events.jsonl")
	writeEventsToPath(t, path, events)
}

func writeEventsToPath(t *testing.T, path string, events []copilotEvent) string {
	t.Helper()
	var content []byte
	for _, evt := range events {
		line, err := json.Marshal(evt)
		if err != nil {
			t.Fatalf("marshal event: %v", err)
		}
		content = append(content, line...)
		content = append(content, '\n')
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("writeEventsFile: %v", err)
	}
	return path
}
