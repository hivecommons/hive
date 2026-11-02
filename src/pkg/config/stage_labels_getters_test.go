package config

import (
	"testing"
	"time"
)

func TestStageLabelsConfigGetterDefaults(t *testing.T) {
	var s StageLabelsConfig
	if !s.IsEnabled() || !s.Stalled.IsRequireQuiet() || !s.Adoption.IsEnabled() ||
		!s.Adoption.InPlaceEnabled() || !s.Adoption.StrictAttributionEnabled() || !s.Adoption.AutoMergeEnabled() || !s.ContributorNudge.IsEnabled() {
		t.Fatal("nil tri-state toggles default on")
	}
	if s.Adoption.EffectiveAfterOffline() != DefaultAdoptAfterOffline ||
		s.Adoption.EffectiveAfterUnregistered() != DefaultAdoptAfterUnregistered ||
		s.Adoption.EffectiveRebaseAttemptTimeout() != 15*time.Minute ||
		s.EffectiveContributorGraceOnline() != DefaultContributorGraceOnline ||
		s.EffectiveContributorGraceOffline() != DefaultContributorGraceOffline ||
		s.EffectivePresenceWindow() != DefaultPresenceWindow ||
		s.ContributorNudge.EffectiveRenudgeInterval() != 0 {
		t.Fatal("unset durations take their defaults")
	}
	if s.DwellOverrides() != nil {
		t.Fatal("no dwell overrides without a map")
	}

	off := false
	s = StageLabelsConfig{
		Enabled:                 &off,
		PresenceWindow:          "1m",
		ContributorGraceOnline:  "bogus",
		ContributorGraceOffline: "-5m",
		Dwell:                   map[string]string{" Review ": " 3h ", "bad": "x", "neg": "-1h"},
		ContributorNudge:        ContributorNudgeConfig{Enabled: &off, RenudgeInterval: "30m"},
		Adoption: StageAdoptionConfig{
			Enabled: &off, InPlace: &off, StrictAttribution: &off, AutoMerge: &off,
			AfterOffline: "2m", AfterUnregistered: "0s", RebaseAttemptTimeout: "20m",
		},
		Stalled: StageStalledConfig{RequireQuiet: &off},
	}
	if s.IsEnabled() || s.Stalled.IsRequireQuiet() || s.Adoption.IsEnabled() ||
		s.Adoption.InPlaceEnabled() || s.Adoption.StrictAttributionEnabled() || s.Adoption.AutoMergeEnabled() || s.ContributorNudge.IsEnabled() {
		t.Fatal("explicit false turns each toggle off")
	}
	if s.EffectivePresenceWindow() != time.Minute || s.Adoption.EffectiveAfterOffline() != 2*time.Minute ||
		s.Adoption.EffectiveRebaseAttemptTimeout() != 20*time.Minute ||
		s.ContributorNudge.EffectiveRenudgeInterval() != 30*time.Minute {
		t.Fatal("valid durations parse")
	}
	if s.EffectiveContributorGraceOnline() != DefaultContributorGraceOnline ||
		s.EffectiveContributorGraceOffline() != DefaultContributorGraceOffline ||
		s.Adoption.EffectiveAfterUnregistered() != DefaultAdoptAfterUnregistered {
		t.Fatal("unparsable and non-positive durations fall back to the default")
	}
	dw := s.DwellOverrides()
	if len(dw) != 1 || dw["review"] != 3*time.Hour {
		t.Fatalf("dwell overrides = %v", dw)
	}
}

func TestClaimsConfigEffectiveDefaults(t *testing.T) {
	var c ClaimsConfig
	th, w, co := c.EffectiveContributorFlap()
	if th != DefaultContributorFlapThreshold || w != DefaultContributorFlapWindow || co != DefaultContributorFlapCooloff {
		t.Fatalf("flap defaults = %d %v %v", th, w, co)
	}
	if c.EffectiveContributorStarvation() != DefaultContributorStarvation || c.EffectiveReclaimCooldown() != DefaultReclaimCooldown {
		t.Fatal("starvation/reclaim defaults")
	}
	c = ClaimsConfig{ContributorFlapThreshold: -1, ContributorStarvationS: -1, ReclaimCooldownS: -1}
	th, _, _ = c.EffectiveContributorFlap()
	if th != 0 || c.EffectiveContributorStarvation() != 0 || c.EffectiveReclaimCooldown() != 0 {
		t.Fatal("negative disables")
	}
	c = ClaimsConfig{ContributorFlapThreshold: 5, ContributorFlapWindowS: 60, ContributorFlapCooloffS: 120, ContributorStarvationS: 30, ReclaimCooldownS: 45}
	th, w, co = c.EffectiveContributorFlap()
	if th != 5 || w != time.Minute || co != 2*time.Minute || c.EffectiveContributorStarvation() != 30*time.Second || c.EffectiveReclaimCooldown() != 45*time.Second {
		t.Fatal("explicit values")
	}
}
