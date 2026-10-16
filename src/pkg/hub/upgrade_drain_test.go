package hub

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestParseUpgradeDrainUntil(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		raw       string
		startedAt time.Time
		want      time.Time
	}{
		{"empty is not draining", "", time.Time{}, time.Time{}},
		{"malformed is not draining", "soon", time.Time{}, time.Time{}},
		{"valid deadline is kept", "2026-10-16T12:30:00Z", time.Time{}, now.Add(30 * time.Minute)},
		{"far-future deadline is capped from now without a start clock", "2030-01-01T00:00:00Z", time.Time{}, now.Add(maxUpgradeDrainDeadline)},
		// The cap is anchored to the upgrade's START so a spoke cannot renew
		// it on every heartbeat and mute stuck detection indefinitely.
		{"far-future deadline is capped from the start clock", "2030-01-01T00:00:00Z", now.Add(-2 * time.Hour), now.Add(maxUpgradeDrainDeadline - 2*time.Hour)},
		{"deadline past the start-anchored cap is clamped even if near", "2026-10-16T13:00:00Z", now.Add(-maxUpgradeDrainDeadline - time.Hour), now.Add(-time.Hour)},
		{"future start clock falls back to now", "2030-01-01T00:00:00Z", now.Add(time.Hour), now.Add(maxUpgradeDrainDeadline)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseUpgradeDrainUntil(tc.raw, tc.startedAt, now); !got.Equal(tc.want) {
				t.Fatalf("parseUpgradeDrainUntil(%q, %v) = %v, want %v", tc.raw, tc.startedAt, got, tc.want)
			}
		})
	}
}

// TestDrainCapIsBoundedPerUpgrade is the property the start-anchored cap
// exists for: however many heartbeats keep reporting a far-future deadline,
// the honoured deadline never moves past start+maxUpgradeDrainDeadline, so the
// mute window on stuck/orphan detection is finite for every upgrade.
func TestDrainCapIsBoundedPerUpgrade(t *testing.T) {
	start := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	limit := start.Add(maxUpgradeDrainDeadline)
	for beat := time.Duration(0); beat < 24*time.Hour; beat += 30 * time.Minute {
		now := start.Add(beat)
		got := parseUpgradeDrainUntil("2030-01-01T00:00:00Z", start, now)
		if got.After(limit) {
			t.Fatalf("beat at +%v: honoured deadline %v exceeds start-anchored cap %v", beat, got, limit)
		}
		if beat > maxUpgradeDrainDeadline && upgradeDrainActive(got, now) {
			t.Fatalf("beat at +%v: drain still active past the per-upgrade cap", beat)
		}
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

// TestTriggerAutoUpgradesHonoursDrainDeadline pins the recovery loop half of
// #12442. The issue's cited evidence — "re-arming heartbeat fallback for stale
// upgrade ... stale_minutes=11" — comes from triggerAutoUpgrades, which fires
// at staleUpgradeTimeout, long before the orphan sweep can. While the spoke's
// reported drain deadline is still ahead the hive must NOT be treated as
// stale: no target advance, no start-clock reset. The not-stale branch still
// re-confirms the original target so the spoke's drain latch stays open. Once
// the deadline passes, recovery runs as before (the positive control).
func TestTriggerAutoUpgradesHonoursDrainDeadline(t *testing.T) {
	tests := []struct {
		name       string
		drainLeft  time.Duration
		wantTarget string
	}{
		{"draining: keeps target", 10 * time.Minute, "fc32ae4"},
		{"drain elapsed: advances target", -time.Minute, "newer99"},
		{"no drain reported: advances target", 0, "newer99"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cleanup := helperSetupTempDirs(t)
			defer cleanup()
			installScriptedKubectl(t)

			remote := ClusterConfig{ID: "remote", InCluster: false, KubeconfigPath: "/tmp/kc", Context: "ctx"}
			saveSaaSHive(&SaaSHive{ID: "drain-1", Owner: "alice", AutoUpgrade: true, Status: "running", ClusterID: "remote"})
			resetSHACaches(t)
			latestSHAMu.Lock()
			latestSHAByBranch["v2"] = branchSHAInfo{SHA: "newer99"}
			latestSHAMu.Unlock()

			started := time.Now().Add(-30 * time.Minute)
			entry := RegistryEntry{
				ID: "drain-1", GitBranch: "v2", GitHash: "old1234", Upgrading: true,
				UpgradeTarget: "fc32ae4", UpgradeStartedAt: started,
				LastHeartbeat: time.Now().UTC().Add(-30 * time.Second).Format(time.RFC3339),
			}
			if tc.drainLeft != 0 {
				entry.UpgradeDrainUntil = time.Now().Add(tc.drainLeft)
			}
			s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret, heartbeatUpgrade: make(map[string]string), clusters: map[string]ClusterConfig{"remote": remote}}
			s.registry.Hives = []RegistryEntry{entry}

			s.triggerAutoUpgrades()

			s.mu.RLock()
			defer s.mu.RUnlock()
			got := s.registry.Hives[0]
			if got.UpgradeTarget != tc.wantTarget {
				t.Errorf("UpgradeTarget = %q, want %q", got.UpgradeTarget, tc.wantTarget)
			}
			if armed := s.heartbeatUpgrade["drain-1"]; armed != tc.wantTarget {
				t.Errorf("heartbeatUpgrade = %q, want %q", armed, tc.wantTarget)
			}
			if !got.Upgrading {
				t.Error("the durable Upgrading latch must survive")
			}
			if tc.drainLeft > 0 && !got.UpgradeStartedAt.Equal(started) {
				t.Errorf("a draining hive's start clock must not be touched, got %v want %v", got.UpgradeStartedAt, started)
			}
		})
	}
}

// TestDashboardUpgradeCounterHonoursDrain is the structural guard for the
// client-side half: the red "stuck" escalation and the facet's stuck count
// are derived in the browser from upgradeStartedAt, independently of the
// server alert, so they must consult the same drain deadline the hub does or
// the row turns red while the alert panel stays quiet.
func TestDashboardUpgradeCounterHonoursDrain(t *testing.T) {
	for name, snippet := range map[string]string{
		"helper reads the heartbeat field": "var until = h.upgradeDrainUntil;",
		"red escalation gated":             "var stuck = ms >= UPGRADE_ELAPSED_RED_MS && !draining;",
		"colour gated":                     "if (ms >= UPGRADE_ELAPSED_RED_MS && !draining) return 'var(--red)';",
		"facet stuck count gated":          "ms >= UPGRADE_FACET_STUCK_MS && !hiveUpgradeDraining(hives[i], now)) stuck++;",
	} {
		if !strings.Contains(dashboardHTML, snippet) {
			t.Errorf("%s: missing %q", name, snippet)
		}
	}
}
