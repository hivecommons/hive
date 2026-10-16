package hub

import (
	"testing"
	"time"
)

func TestParseUpgradeDrainUntil(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		raw  string
		want time.Time
	}{
		{"empty is not draining", "", time.Time{}},
		{"malformed is not draining", "soon", time.Time{}},
		{"valid deadline is kept", "2026-10-16T12:30:00Z", now.Add(30 * time.Minute)},
		{"far-future deadline is capped", "2030-01-01T00:00:00Z", now.Add(maxUpgradeDrainDeadline)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseUpgradeDrainUntil(tc.raw, now); !got.Equal(tc.want) {
				t.Fatalf("parseUpgradeDrainUntil(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestEvaluateOrphanedUpgradeHonoursDrainDeadline(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		drainLeft time.Duration
		want      bool
	}{
		{"no drain reported is orphaned", 0, true},
		{"drain deadline in the future is left alone", 10 * time.Minute, false},
		{"drain deadline elapsed is orphaned", -time.Minute, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			entry := &RegistryEntry{
				Upgrading:        true,
				UpgradeTarget:    "aaaaaaa",
				GitHash:          "bbbbbbb",
				UpgradeStartedAt: now.Add(-orphanedUpgradeClearAfter - time.Minute),
				LastHeartbeat:    now.Add(-10 * time.Second).Format(time.RFC3339),
			}
			if tc.drainLeft != 0 {
				entry.UpgradeDrainUntil = now.Add(tc.drainLeft)
			}
			if got := evaluateOrphanedUpgrade(entry, now, "").orphaned; got != tc.want {
				t.Fatalf("orphaned = %v, want %v", got, tc.want)
			}
		})
	}
}
