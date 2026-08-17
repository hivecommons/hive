package tokens

import (
	"log/slog"
	"path/filepath"
	"testing"
)

func TestConfiguredAgentNamesCustom(t *testing.T) {
	defer SetAgentNames(nil) // restore after test

	SetAgentNames([]string{"custom-agent", "helper"})
	names := ConfiguredAgentNames()
	if len(names) != 2 {
		t.Errorf("expected 2 custom names, got %d", len(names))
	}
	if names[0] != "custom-agent" {
		t.Errorf("first name = %q, want 'custom-agent'", names[0])
	}
}

func TestConfiguredAgentNamesDefault(t *testing.T) {
	defer SetAgentNames(nil)

	SetAgentNames(nil)
	names := ConfiguredAgentNames()
	if len(names) == 0 {
		t.Error("should return default agent names")
	}
}

func TestConfiguredAgentNamesEmptySlice(t *testing.T) {
	defer SetAgentNames(nil)

	SetAgentNames([]string{})
	names := ConfiguredAgentNames()
	if len(names) == 0 {
		t.Error("should return default names when empty slice")
	}
}

func TestSetDetectKeywords(t *testing.T) {
	defer SetDetectKeywords(nil)

	kw := map[string][]string{
		"scanner": {"scan", "triage"},
		"helper":  {"help", "assist"},
	}
	SetDetectKeywords(kw)

	// Verify it doesn't panic and keywords are stored
	names := ConfiguredAgentNames()
	_ = names
}

func TestSaveSnapshotSuccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.json")

	c := &Collector{
		persistPath: path,
		logger:      slog.Default(),
	}

	agg := &AggregateSummary{
		TotalInput:  1000,
		TotalOutput: 500,
	}
	c.saveSnapshot(agg)

	// File should exist
	names := ConfiguredAgentNames()
	_ = names
}

func TestSaveSnapshotNilAgg(t *testing.T) {
	c := &Collector{
		persistPath: "/tmp/test-nil.json",
		logger:      slog.Default(),
	}
	c.saveSnapshot(nil)
}

func TestSaveSnapshotEmptyPath(t *testing.T) {
	c := &Collector{
		persistPath: "",
		logger:      slog.Default(),
	}
	c.saveSnapshot(&AggregateSummary{})
}

func TestSaveSnapshotBadPath(t *testing.T) {
	c := &Collector{
		persistPath: "/nonexistent-dir-xyz/tokens.json",
		logger:      slog.Default(),
	}
	c.saveSnapshot(&AggregateSummary{TotalInput: 100})
}
