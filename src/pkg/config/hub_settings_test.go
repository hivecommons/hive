package config

import (
	"strings"
	"testing"
)

func TestValidateHubSettings(t *testing.T) {
	if err := ValidateHubSettings(HubConfig{}); err != nil {
		t.Fatalf("zero hub config: %v", err)
	}
	ok := HubConfig{
		ContributeWallRetentionDays:   MaxContributeWallRetentionDays,
		ContributeAlreadyDoneHoldDays: 365,
		ContributeAlreadyDoneLabel:    strings.Repeat("x", MaxContributeAlreadyDoneLabelLen),
		SnapshotIntervalMin:           MinSnapshotIntervalMin,
		UpgradeDrainTimeoutS:          -1,
		ContributeAnnouncement:        ContributeAnnouncement{ExpiresAt: "2099-01-01T00:00:00Z"},
	}
	if err := ValidateHubSettings(ok); err != nil {
		t.Fatalf("in-range hub config: %v", err)
	}
	for name, h := range map[string]HubConfig{
		"wall retention negative":  {ContributeWallRetentionDays: -1},
		"wall retention too large": {ContributeWallRetentionDays: MaxContributeWallRetentionDays + 1},
		"hold days negative":       {ContributeAlreadyDoneHoldDays: -1},
		"hold days too large":      {ContributeAlreadyDoneHoldDays: 366},
		"label too long":           {ContributeAlreadyDoneLabel: strings.Repeat("x", MaxContributeAlreadyDoneLabelLen+1)},
		"snapshot interval small":  {SnapshotIntervalMin: MinSnapshotIntervalMin - 1},
		"snapshot interval large":  {SnapshotIntervalMin: MaxSnapshotIntervalMin + 1},
		"drain timeout too large":  {UpgradeDrainTimeoutS: MaxUpgradeDrainTimeoutS + 1},
		"expires_at not RFC3339":   {ContributeAnnouncement: ContributeAnnouncement{ExpiresAt: "tomorrow"}},
	} {
		if err := ValidateHubSettings(h); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestHubDefaultsMatchResolvers(t *testing.T) {
	var h HubConfig
	if got := h.ContributeAlreadyDoneLabelOrDefault(); got != DefaultContributeAlreadyDoneLabel {
		t.Errorf("already-done label default = %q", got)
	}
	if got := h.ContributeAlreadyDoneHoldDaysOrDefault(); got != DefaultContributeAlreadyDoneHoldDays {
		t.Errorf("already-done hold default = %d", got)
	}
	if got := h.EffectiveUpgradeDrainTimeout().Seconds(); got != DefaultUpgradeDrainTimeoutS {
		t.Errorf("upgrade drain default = %v", got)
	}
}
