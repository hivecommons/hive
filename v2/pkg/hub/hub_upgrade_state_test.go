package hub

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// setV2Latest seeds the verified latest SHA for v2 and restores it afterward.
func setV2Latest(t *testing.T, sha string) {
	t.Helper()
	latestSHAMu.Lock()
	old := latestSHAByBranch["v2"]
	latestSHAByBranch["v2"] = branchSHAInfo{SHA: sha}
	latestSHAMu.Unlock()
	t.Cleanup(func() {
		latestSHAMu.Lock()
		latestSHAByBranch["v2"] = old
		latestSHAMu.Unlock()
	})
}

// setAutoUpgrade points hubAutoUpgradePath at a temp file containing the given
// value ("true" enables auto-upgrade) and restores the path afterward.
func setAutoUpgrade(t *testing.T, enabled bool) {
	t.Helper()
	old := hubAutoUpgradePath
	p := filepath.Join(t.TempDir(), "hub-auto-upgrade")
	val := "false"
	if enabled {
		val = "true"
	}
	if err := os.WriteFile(p, []byte(val), 0o644); err != nil {
		t.Fatal(err)
	}
	hubAutoUpgradePath = p
	t.Cleanup(func() { hubAutoUpgradePath = old })
}

func TestHubUpgradeState(t *testing.T) {
	// unknown: no latest SHA resolved.
	setV2Latest(t, "")
	s := &HubServer{logger: slog.Default(), hubGitBranch: "v2", hubGitHash: "aaaaaaa"}
	if got := s.hubUpgradeState(); got != "unknown" {
		t.Errorf("no latest -> %q, want unknown", got)
	}

	// current: hub hash matches latest.
	setV2Latest(t, "aaaaaaa")
	if got := s.hubUpgradeState(); got != "current" {
		t.Errorf("matching hash -> %q, want current", got)
	}

	// behind: differs, auto-upgrade OFF, no roll in flight.
	setV2Latest(t, "bbbbbbb")
	setAutoUpgrade(t, false)
	if got := s.hubUpgradeState(); got != "behind" {
		t.Errorf("behind + auto off -> %q, want behind", got)
	}

	// queued: differs, auto-upgrade ON, no roll in flight.
	setAutoUpgrade(t, true)
	if got := s.hubUpgradeState(); got != "queued" {
		t.Errorf("behind + auto on -> %q, want queued", got)
	}

	// upgrading: a rollout was just triggered (within the debounce window).
	s.hubUpgradeMu.Lock()
	s.hubUpgradeTarget = "bbbbbbb"
	s.lastHubUpgradeTrigger = time.Now()
	s.hubUpgradeMu.Unlock()
	if got := s.hubUpgradeState(); got != "upgrading" {
		t.Errorf("roll in flight -> %q, want upgrading", got)
	}

	// After the debounce window elapses (trigger long ago), it falls back to
	// queued (auto still on) rather than latching "upgrading" forever.
	s.hubUpgradeMu.Lock()
	s.lastHubUpgradeTrigger = time.Now().Add(-hubUpgradeDebounce - time.Minute)
	s.hubUpgradeMu.Unlock()
	if got := s.hubUpgradeState(); got != "queued" {
		t.Errorf("stale trigger -> %q, want queued (no longer upgrading)", got)
	}
}

// TestRolloutHubToSHAEmpty verifies rolloutHubToSHA rejects an empty target
// rather than issuing a broken kubectl command.
func TestRolloutHubToSHAEmpty(t *testing.T) {
	s := &HubServer{logger: slog.Default(), hubGitBranch: "v2"}
	if err := s.rolloutHubToSHA(""); err == nil {
		t.Error("empty SHA should error")
	}
}
