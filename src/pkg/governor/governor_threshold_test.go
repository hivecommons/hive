package governor

import (
	"log/slog"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestThresholdForDefaults(t *testing.T) {
	g := New(config.GovernorConfig{}, map[string]config.AgentConfig{}, slog.Default())

	tests := []struct {
		mode     string
		expected int
	}{
		{"surge", 20},
		{"busy", 10},
		{"quiet", 2},
		{"unknown", 0},
		{"", 0},
		{"custom-mode", 0},
	}

	for _, tt := range tests {
		got := g.thresholdFor(tt.mode)
		if got != tt.expected {
			t.Errorf("thresholdFor(%q) = %d, want %d", tt.mode, got, tt.expected)
		}
	}
}

func TestThresholdForConfigured(t *testing.T) {
	cfg := config.GovernorConfig{
		Modes: map[string]config.ModeConfig{
			"surge": {Threshold: 50},
			"custom": {Threshold: 7},
		},
	}
	g := New(cfg, map[string]config.AgentConfig{}, slog.Default())

	if got := g.thresholdFor("surge"); got != 50 {
		t.Errorf("configured surge threshold = %d, want 50", got)
	}
	if got := g.thresholdFor("custom"); got != 7 {
		t.Errorf("configured custom threshold = %d, want 7", got)
	}
	// Unconfigured mode falls through to defaults
	if got := g.thresholdFor("busy"); got != 10 {
		t.Errorf("unconfigured busy = %d, want 10", got)
	}
}
