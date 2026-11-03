package config

import "testing"

func TestBeadSynthesizerMaxAPICallsPerCycle_12005(t *testing.T) {
	if got := (BeadSynthesizerConfig{}).EffectiveMaxAPICallsPerCycle(); got != 40 {
		t.Fatalf("default = %d, want 40", got)
	}
	if got := (BeadSynthesizerConfig{MaxAPICallsPerCycle: 12}).EffectiveMaxAPICallsPerCycle(); got != 12 {
		t.Fatalf("override = %d, want 12", got)
	}
}
