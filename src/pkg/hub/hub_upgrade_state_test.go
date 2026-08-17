package hub

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setV2Latest seeds the verified latest SHA for v2 and restores it afterward.
// Both the spoke and hub caches are seeded: the spoke cache drives hive upgrade
// targets, the hub cache drives the hub's own. They are separate because the two
// images are separate builds, but tests that don't care about that distinction
// want them in agreement — see setV2HubLatest for the divergent case.
func setV2Latest(t *testing.T, sha string) {
	t.Helper()
	latestSHAMu.Lock()
	old := latestSHAByBranch["v2"]
	oldHub := latestHubSHAByBranch["v2"]
	latestSHAByBranch["v2"] = branchSHAInfo{SHA: sha}
	latestHubSHAByBranch["v2"] = branchSHAInfo{SHA: sha}
	latestSHAMu.Unlock()
	t.Cleanup(func() {
		latestSHAMu.Lock()
		latestSHAByBranch["v2"] = old
		latestHubSHAByBranch["v2"] = oldHub
		latestSHAMu.Unlock()
	})
}

// setV2HubLatest seeds ONLY the hub-image cache for v2, leaving the spoke cache
// untouched, so a test can reproduce the case this split exists for: the hub
// image published for a SHA whose spoke image did not.
func setV2HubLatest(t *testing.T, sha string) {
	t.Helper()
	latestSHAMu.Lock()
	old := latestHubSHAByBranch["v2"]
	latestHubSHAByBranch["v2"] = branchSHAInfo{SHA: sha}
	latestSHAMu.Unlock()
	t.Cleanup(func() {
		latestSHAMu.Lock()
		latestHubSHAByBranch["v2"] = old
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

// TestRolloutHubToSHAContainerName verifies `kubectl set image` targets the
// hub's ACTUAL container name ("hub"), not the deployment name ("hive-hub").
// Naming the wrong container makes set image a no-op/error, so a regression here
// silently breaks every hub self-upgrade.
func TestRolloutHubToSHAContainerName(t *testing.T) {
	// Fake kubectl that records its args, so we can assert the container= arg.
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args.txt")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + argsFile + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Stub the GHCR check so the roll proceeds to kubectl.
	oldImg := hubImageExists
	hubImageExists = func(string, *slog.Logger) bool { return true }
	t.Cleanup(func() { hubImageExists = oldImg })

	s := &HubServer{logger: slog.Default(), hubGitBranch: "v2",
		clusters: map[string]ClusterConfig{defaultClusterID: {ID: defaultClusterID, InCluster: true}}}
	if err := s.rolloutHubToSHA("abc1234"); err != nil {
		t.Fatalf("rolloutHubToSHA: %v", err)
	}

	got, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("kubectl not invoked: %v", err)
	}
	args := string(got)
	wantContainer := hubContainerName + "=ghcr.io/" + ghcrRepoHub + ":abc1234"
	if !strings.Contains(args, wantContainer) {
		t.Errorf("set image args = %q, want container arg %q", strings.TrimSpace(args), wantContainer)
	}
	if strings.Contains(args, hubDeploymentName+"=ghcr.io/") {
		t.Errorf("set image wrongly used the DEPLOYMENT name as the container: %q", strings.TrimSpace(args))
	}
}

// TestHubUpgradeStateUsesHubImageNotSpokeImage pins the split between the two
// GHCR repos. The hub and spoke images are separate builds that can succeed
// independently for the same commit; gating the hub's upgrade target on the
// SPOKE image meant a SHA whose spoke build failed was invisible to the hub,
// which then reported "current" against a stale target and never rolled.
func TestHubUpgradeStateUsesHubImageNotSpokeImage(t *testing.T) {
	setAutoUpgrade(t, false)

	// Spoke image stuck on an older SHA (its build failed for the newer one),
	// hub image already published for the newer SHA.
	setV2Latest(t, "aaaaaaa")
	setV2HubLatest(t, "bbbbbbb")

	// Hub is running the older SHA. It must see itself as behind the SHA whose
	// HUB image exists, not "current" against the stale spoke-gated value.
	s := &HubServer{logger: slog.Default(), hubGitBranch: "v2", hubGitHash: "aaaaaaa"}
	if got := s.hubUpgradeState(); got != "behind" {
		t.Errorf("hub behind on hub-image SHA -> %q, want behind", got)
	}

	// And once it is running that SHA, it is current — even though the spoke
	// cache still trails.
	s.hubGitHash = "bbbbbbb"
	if got := s.hubUpgradeState(); got != "current" {
		t.Errorf("hub at hub-image SHA -> %q, want current", got)
	}
}
