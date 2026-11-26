package config

import (
	"path/filepath"
	"testing"
)

func TestConfigAssistDefaultsOff(t *testing.T) {
	var c ChatConfig
	if c.ConfigAssistEnabled() {
		t.Fatal("config assist must be off when unset")
	}
	f := false
	c.ConfigAssist.Enabled = &f
	if c.ConfigAssistEnabled() {
		t.Fatal("explicit false must be off")
	}
	// grounded_answers on does not turn it on.
	if !c.GroundedAnswersEnabled() || c.ConfigAssistEnabled() {
		t.Fatal("grounded answers default must not enable config assist")
	}
	tr := true
	c.ConfigAssist.Enabled = &tr
	if !c.ConfigAssistEnabled() {
		t.Fatal("explicit true must be on")
	}
}

// #20092: chat.config_assist.model overrides the proposal model and falls
// back to grounded_answers.model when unset.
func TestConfigAssistModelFallsBackToGroundedAnswers(t *testing.T) {
	var c ChatConfig
	if got := c.ConfigAssistModel(); got != "" {
		t.Fatalf("unset model = %q, want empty", got)
	}
	c.GroundedAnswers.Model = " grounded-model "
	if got := c.ConfigAssistModel(); got != "grounded-model" {
		t.Fatalf("fallback model = %q, want grounded-model", got)
	}
	c.ConfigAssist.Model = "assist-model"
	if got := c.ConfigAssistModel(); got != "assist-model" {
		t.Fatalf("override model = %q, want assist-model", got)
	}
	c.ConfigAssist.Model = "   "
	if got := c.ConfigAssistModel(); got != "grounded-model" {
		t.Fatalf("blank override = %q, want grounded-model", got)
	}
}

// #19979: the toggle written to the dashboard overlay survives reload and the
// next persist.
func TestConfigAssistFlagSurvivesReloadAndPersist(t *testing.T) {
	overlay := overlayCarrySeed + `
chat:
  config_assist:
    enabled: true
`
	f := setupOverlayCarry(t, overlayCarrySeed, overlay, true)
	cfg, err := LoadWithDashboardOverlay(f.seed)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !cfg.Chat.ConfigAssistEnabled() {
		t.Fatal("reload dropped chat.config_assist.enabled")
	}
	if err := cfg.ReconcilePausedAndSave(nil); err != nil {
		t.Fatalf("persist: %v", err)
	}
	for _, layer := range []string{f.overlay, f.seed, f.runtime} {
		if !parseLayer(t, layer).Chat.ConfigAssistEnabled() {
			t.Errorf("persist dropped the flag from %s", filepath.Base(layer))
		}
	}
	again, err := LoadWithDashboardOverlay(f.seed)
	if err != nil {
		t.Fatalf("second reload: %v", err)
	}
	if !again.Chat.ConfigAssistEnabled() {
		t.Fatal("second reload dropped the flag")
	}
}
