package tokens

import (
	"log/slog"
	"path/filepath"
	"testing"
)

func TestSetDetectKeywords(t *testing.T) {
	defer SetDetectKeywords(nil)

	kw := map[string][]string{
		"scanner": {"scan", "triage"},
		"helper":  {"help", "assist"},
	}
	SetDetectKeywords(kw)

	// Verify keyword-driven detection uses the configured map.
	if got := DefaultAgentDetector("please triage this"); got != "scanner" {
		t.Errorf("DefaultAgentDetector = %q, want scanner", got)
	}
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
