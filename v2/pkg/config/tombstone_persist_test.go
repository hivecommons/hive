package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadWithDashboardOverlay_AdoptsTombstoneAtBoot is the boot half of the
// #2439 regression: on restart the entrypoint loads the seed and the startup
// ApplyPack reconciles the ACMM roster. If the tombstone written by the delete
// handler (which only lands in the dashboard overlay, the sole agent source the
// dashboard can write) is not adopted at load time, ApplyPack cannot see it and
// re-adds the deleted pack agents (brainstorm/guide) every restart.
//
// Before the fix the boot path used plain config.Load, which never reads the
// overlay, so cfg.RemovedAgents was empty and the agents came back. This test
// asserts LoadWithDashboardOverlay surfaces the overlay's removed_agents AND
// evicts a tombstoned agent that lingers in the seed.
func TestLoadWithDashboardOverlay_AdoptsTombstoneAtBoot(t *testing.T) {
	dir := t.TempDir()
	seedPath := filepath.Join(dir, "hive.yaml")

	// Seed = the stale ConfigMap the entrypoint restores over the runtime file.
	// It still lists guide, because the dashboard cannot rewrite the ConfigMap.
	seed := `
project:
  org: testorg
  repos: [repo1]
github:
  token: ghp_test123456789
agents:
  scanner:
    backend: copilot
    kick_template: scanner-advisory.md
  guide:
    backend: copilot
    kick_template: guide.md
  brainstorm:
    backend: copilot
    kick_template: brainstorm.md
`
	if err := os.WriteFile(seedPath, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	// Overlay = what the delete handler persisted: brainstorm+guide tombstoned.
	overlayPath := filepath.Join(dir, "hive.yaml.dashboard")
	overlay := `
project:
  org: testorg
  repos: [repo1]
github:
  token: ghp_test123456789
removed_agents: [brainstorm, guide]
agents:
  scanner:
    backend: copilot
    kick_template: scanner-advisory.md
`
	if err := os.WriteFile(overlayPath, []byte(overlay), 0o644); err != nil {
		t.Fatal(err)
	}

	origOverlay := DashboardOverlayFile
	DashboardOverlayFile = overlayPath
	t.Cleanup(func() { DashboardOverlayFile = origOverlay })
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")

	// Precondition and the crux of the boot bug: plain Load (what main.go used at
	// boot) never reads the overlay, so the tombstone is invisible and the seed's
	// guide/brainstorm survive. This is exactly why ApplyPack re-added them.
	raw, err := Load(seedPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if raw.IsAgentRemoved("guide") {
		t.Fatal("precondition: plain Load must NOT see the overlay tombstone (that is the boot bug)")
	}

	cfg, err := LoadWithDashboardOverlay(seedPath)
	if err != nil {
		t.Fatalf("LoadWithDashboardOverlay: %v", err)
	}

	for _, name := range []string{"brainstorm", "guide"} {
		if !cfg.IsAgentRemoved(name) {
			t.Errorf("tombstone for %q not adopted from overlay at boot", name)
		}
		if _, ok := cfg.Agents[name]; ok {
			t.Errorf("tombstoned agent %q survived in roster from the seed", name)
		}
	}
	if _, ok := cfg.Agents["scanner"]; !ok {
		t.Error("non-deleted agent scanner should still be present")
	}
}

// TestLoadWithDashboardOverlay_TombstoneSurvivesShortOverlay is the reload half
// of #2439. The ~2-min persistState saver reloads via LoadWithDashboardOverlay.
// The fullness guard (overlay.Project.Org == "" || len(overlay.Agents) == 0)
// early-returns for a short or agent-less overlay. Before the fix that early
// return skipped the RemovedAgents assignment, so the reload dropped the
// tombstone to empty and the next save rewrote every layer tombstone-free —
// the deleted agents then reappeared on an interval. The tombstone must be
// adopted even when the overlay is otherwise too short to trust its agents.
func TestLoadWithDashboardOverlay_TombstoneSurvivesShortOverlay(t *testing.T) {
	dir := t.TempDir()
	seedPath := filepath.Join(dir, "hive.yaml")
	seed := `
project:
  org: testorg
  repos: [repo1]
github:
  token: ghp_test123456789
agents:
  scanner:
    backend: copilot
    kick_template: scanner-advisory.md
  guide:
    backend: copilot
    kick_template: guide.md
`
	if err := os.WriteFile(seedPath, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	// Short overlay: carries the tombstone but NO agents (and empty org). This
	// trips the fullness guard, which used to skip the RemovedAgents adoption.
	overlayPath := filepath.Join(dir, "hive.yaml.dashboard")
	overlay := `
removed_agents: [guide]
`
	if err := os.WriteFile(overlayPath, []byte(overlay), 0o644); err != nil {
		t.Fatal(err)
	}

	origOverlay := DashboardOverlayFile
	DashboardOverlayFile = overlayPath
	t.Cleanup(func() { DashboardOverlayFile = origOverlay })
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")

	cfg, err := LoadWithDashboardOverlay(seedPath)
	if err != nil {
		t.Fatalf("LoadWithDashboardOverlay: %v", err)
	}
	if !cfg.IsAgentRemoved("guide") {
		t.Fatal("tombstone dropped by the short-overlay early bail (#2439 reload wipe)")
	}
	if _, ok := cfg.Agents["guide"]; ok {
		t.Error("tombstoned guide should be pruned from the roster even on a short overlay")
	}
}

// TestMergeAgentOverridesTombstoneWinsOverStaleFile pins the defense-in-depth
// contract from #2439 FIX 3: even if a stale /data/agent-configs/<name>.yaml
// survives the delete (RemoveAgentFile failed and was logged non-fatally), the
// tombstone must win — MergeAgentOverrides skips the file's agent. This makes
// the tombstone authoritative independent of the per-agent file's deletion.
func TestMergeAgentOverridesTombstoneWinsOverStaleFile(t *testing.T) {
	cfg := &Config{
		Agents:        map[string]AgentConfig{},
		RemovedAgents: []string{"brainstorm"},
	}
	// The stale per-agent overlay file, still on disk, offered up by a reload.
	cfg.MergeAgentOverrides(map[string]AgentConfig{
		"brainstorm": {KickTemplate: "brainstorm.md"},
		"scanner":    {KickTemplate: "scanner-advisory.md"},
	})
	if _, ok := cfg.Agents["brainstorm"]; ok {
		t.Error("tombstone must win over a stale per-agent overlay file")
	}
	if _, ok := cfg.Agents["scanner"]; !ok {
		t.Error("a non-tombstoned agent must still merge from its file")
	}
}
