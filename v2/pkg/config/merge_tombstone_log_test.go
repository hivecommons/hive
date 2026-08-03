package config

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// TestMergeAgentOverridesLogsTombstoneSkip pins the observability added for
// #2439: when MergeAgentOverrides skips a tombstoned agent that still had a
// per-agent overlay file, it emits a greppable INFO line naming the agent. The
// log is the whole point — on a live hive it is what proves the stale-file guard
// fired without exec'ing in to diff config files. Behavior is unchanged; this
// asserts only the line.
func TestMergeAgentOverridesLogsTombstoneSkip(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	cfg := &Config{
		HiveID:        "hive-test",
		Agents:        map[string]AgentConfig{},
		RemovedAgents: []string{"guide"},
	}
	cfg.MergeAgentOverrides(map[string]AgentConfig{
		"guide": {Model: "claude-sonnet-4-6"},
	})

	out := buf.String()
	if !strings.Contains(out, "merge: skipped tombstoned agent") {
		t.Errorf("expected the tombstone-skip audit line, got: %q", out)
	}
	if !strings.Contains(out, "agent=guide") {
		t.Errorf("audit line must name the agent, got: %q", out)
	}
	if !strings.Contains(out, "had_overlay_file=true") {
		t.Errorf("audit line must flag the lingering overlay file, got: %q", out)
	}
}
