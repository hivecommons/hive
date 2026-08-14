package governor

import (
	"log/slog"
	"testing"

	"github.com/kubestellar/hive/v2/pkg/config"
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
			"surge":  {Threshold: 50},
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

// --- #3498: auto-scale default thresholds by configured repo count --------

// TestScaledThreshold pins the pure scaling function directly: max(base,
// base*repoCount), with repoCount<=1 a deliberate no-op.
func TestScaledThreshold(t *testing.T) {
	tests := []struct {
		name      string
		base      int
		repoCount int
		want      int
	}{
		{"zero repo count is a no-op", 20, 0, 20},
		{"one repo is a no-op", 20, 1, 20},
		{"three repos scales linearly", 20, 3, 60},
		{"the #3498 live example: 39 repos", 20, 39, 780},
		{"busy base at 39 repos", 10, 39, 390},
		{"quiet base at 39 repos", 2, 39, 78},
		{"zero base never scales above zero", 0, 39, 0},
		{"negative repo count floors at base, never goes negative", 20, -5, 20},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := scaledThreshold(tt.base, tt.repoCount); got != tt.want {
				t.Errorf("scaledThreshold(%d, %d) = %d, want %d", tt.base, tt.repoCount, got, tt.want)
			}
		})
	}
}

// TestThresholdForAutoScalesByDefault is the end-to-end proof through
// thresholdFor: a hive that sets nothing but a repo count gets the scaled
// default, with no config beyond what SetRepoCount (wired at construction and
// on every reload) already provides.
func TestThresholdForAutoScalesByDefault(t *testing.T) {
	g := New(config.GovernorConfig{}, map[string]config.AgentConfig{}, slog.Default())
	g.SetRepoCount(39)

	tests := []struct {
		mode     string
		expected int
	}{
		{"surge", 780},
		{"busy", 390},
		{"quiet", 78},
	}
	for _, tt := range tests {
		if got := g.thresholdFor(tt.mode); got != tt.expected {
			t.Errorf("thresholdFor(%q) with repoCount=39 = %d, want %d", tt.mode, got, tt.expected)
		}
	}
}

// TestThresholdForExplicitConfigWinsOverAutoScale is the "no behavior change
// for hives that hand-tune" guarantee #3498 requires: an explicit threshold
// must never be overridden by the repo-count scaling, no matter the repo
// count.
func TestThresholdForExplicitConfigWinsOverAutoScale(t *testing.T) {
	cfg := config.GovernorConfig{
		Modes: map[string]config.ModeConfig{
			"surge": {Threshold: 50},
		},
	}
	g := New(cfg, map[string]config.AgentConfig{}, slog.Default())
	g.SetRepoCount(39)

	if got := g.thresholdFor("surge"); got != 50 {
		t.Errorf("explicit surge threshold with repoCount=39 = %d, want 50 (explicit config must win)", got)
	}
	// The UNCONFIGURED neighbour in the same call still scales — proves the
	// explicit win is per-mode, not an all-or-nothing switch on the Governor.
	if got := g.thresholdFor("busy"); got != 390 {
		t.Errorf("unconfigured busy threshold with repoCount=39 = %d, want 390 (auto-scale still applies)", got)
	}
}

// TestThresholdForAutoScaleOptOut covers governor.auto_scale_thresholds:
// false — the escape hatch for hives that want the flat historical defaults
// regardless of repo count.
func TestThresholdForAutoScaleOptOut(t *testing.T) {
	optOut := false
	cfg := config.GovernorConfig{AutoScaleThresholds: &optOut}
	g := New(cfg, map[string]config.AgentConfig{}, slog.Default())
	g.SetRepoCount(39)

	tests := []struct {
		mode     string
		expected int
	}{
		{"surge", 20},
		{"busy", 10},
		{"quiet", 2},
	}
	for _, tt := range tests {
		if got := g.thresholdFor(tt.mode); got != tt.expected {
			t.Errorf("thresholdFor(%q) opted out with repoCount=39 = %d, want flat default %d", tt.mode, got, tt.expected)
		}
	}
}

// TestThresholdForAutoScaleDefaultIsOn asserts AutoScaleThresholds's zero
// value (nil *bool, i.e. a hive that never mentions the setting) means ON —
// matching AttributionTrailer's established default-true convention in this
// same config struct.
func TestThresholdForAutoScaleDefaultIsOn(t *testing.T) {
	cfg := config.GovernorConfig{} // AutoScaleThresholds left nil
	if !cfg.AutoScaleThresholdsEnabled() {
		t.Error("AutoScaleThresholdsEnabled() = false for a nil AutoScaleThresholds field, want true (default ON)")
	}
}

// TestComputeModeUsesAutoScaledThresholds proves the scaling actually reaches
// mode selection, not just thresholdFor in isolation — the #3498 queue=~210,
// 39-repo scenario that sat in SURGE against the flat default now lands in
// QUIET (surge=780, busy=390, quiet=78; 210 clears quiet but not busy).
func TestComputeModeUsesAutoScaledThresholds(t *testing.T) {
	g := New(config.GovernorConfig{}, map[string]config.AgentConfig{}, slog.Default())

	if got := g.computeMode(210); got != ModeSurge {
		t.Fatalf("fixture: computeMode(210) with no repo count set = %s, want %s (flat default surge=20)", got, ModeSurge)
	}

	g.SetRepoCount(39)
	if got := g.computeMode(210); got != ModeQuiet {
		t.Errorf("computeMode(210) with repoCount=39 = %s, want %s — auto-scaling did not reach mode selection", got, ModeQuiet)
	}
}

// TestEffectiveThresholds covers the dashboard-facing accessor: it must
// report the SAME resolved values thresholdFor uses for mode selection, not
// the raw config.
func TestEffectiveThresholds(t *testing.T) {
	cfg := config.GovernorConfig{
		Modes: map[string]config.ModeConfig{
			"surge": {Threshold: 50},
		},
	}
	g := New(cfg, map[string]config.AgentConfig{}, slog.Default())
	g.SetRepoCount(39)

	got := g.EffectiveThresholds()
	want := map[string]int{"surge": 50, "busy": 390, "quiet": 78}
	for mode, wantVal := range want {
		if got[mode] != wantVal {
			t.Errorf("EffectiveThresholds()[%q] = %d, want %d", mode, got[mode], wantVal)
		}
	}
	if len(got) != len(want) {
		t.Errorf("EffectiveThresholds() returned %d entries, want %d: %v", len(got), len(want), got)
	}
}

// TestSetRepoCountIsConcurrencySafe exercises SetRepoCount and thresholdFor
// (via Evaluate, which holds the same mutex) concurrently — SetRepoCount is
// called from a config-reload goroutine independent of the eval loop, so it
// must not race with it.
func TestSetRepoCountIsConcurrencySafe(t *testing.T) {
	g := New(config.GovernorConfig{}, map[string]config.AgentConfig{}, slog.Default())

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			g.SetRepoCount(i)
		}
	}()
	for i := 0; i < 100; i++ {
		g.Evaluate(i, 0, 0, 0)
	}
	<-done
}
