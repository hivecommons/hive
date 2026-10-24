package agent

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// TestLogCopilotModelFallbackWarnsOnce covers #13659: the same substitution
// tuple warns once, then drops to Debug; a changed tuple warns again.
func TestLogCopilotModelFallbackWarnsOnce(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	m := &Manager{logger: logger}
	a := &AgentProcess{Name: "telemetry"}

	warns := func() int { return strings.Count(buf.String(), "level=WARN") }

	m.logCopilotModelFallback(a, "claude-sonnet-4.6", "claude-sonnet-5")
	if warns() != 1 {
		t.Fatalf("first detection: want 1 warn, got %d", warns())
	}
	a.copilotModelFallbackRequested = "" // relaunch clears the poll gate
	m.logCopilotModelFallback(a, "claude-sonnet-4.6", "claude-sonnet-5")
	if warns() != 1 {
		t.Fatalf("repeat tuple must not warn again, got %d warns", warns())
	}
	m.logCopilotModelFallback(a, "claude-sonnet-4.6", "claude-sonnet-4.5")
	if warns() != 2 {
		t.Fatalf("changed tuple should warn, got %d warns", warns())
	}
}
