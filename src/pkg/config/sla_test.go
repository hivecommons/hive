package config

import "testing"

func TestSLAConfig_EffectiveTargetsDefaults(t *testing.T) {
	var nilCfg *SLAConfig
	got := nilCfg.EffectiveTargets()
	if got.StartToPRH != DefaultSLAStartToPRHours || got.PRToMergeH != DefaultSLAPRToMergeHours || got.OpenToMergeH != DefaultSLAOpenToMergeHours {
		t.Fatalf("nil defaults = %+v", got)
	}
	cfg := &SLAConfig{Targets: SLATargets{PRToMergeH: 8}}
	got = cfg.EffectiveTargets()
	if got.StartToPRH != 4 || got.PRToMergeH != 8 || got.OpenToMergeH != 72 {
		t.Fatalf("partial = %+v", got)
	}
}

func TestValidateSLATargets(t *testing.T) {
	if err := ValidateSLATargets(SLATargets{}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSLATargets(SLATargets{StartToPRH: -1}); err == nil {
		t.Fatal("negative accepted")
	}
	if err := ValidateSLATargets(SLATargets{OpenToMergeH: MaxSLATargetHours + 1}); err == nil {
		t.Fatal("absurd accepted")
	}
}
