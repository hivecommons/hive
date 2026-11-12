package config

import (
	"os"
	"path/filepath"
	"testing"
)

// A per-agent overlay file replaces the agent's whole main-config entry at
// load, paused included, but a pause or resume only ever rewrote the main
// config. The overlay kept the paused bit from the agent's last config edit,
// and that stale bit won at the next restart.
//
// The case that lost a pause's provenance: the operator changed the model of
// a paused agent (the models PUT writes the overlay with paused: true), then
// resumed it (main config paused: false). After a hive restart the overlay
// paused the agent again, and hive-state.json, which only knew the resume,
// had no trigger, reason, actor or time to give it.

func overlayPauseSyncConfig(t *testing.T) (*Config, string, string) {
	t.Helper()
	dir := t.TempDir()
	hermeticPersistPaths(t, dir)
	agentsDir := filepath.Join(dir, "agent-configs")
	path := filepath.Join(dir, "hive.yaml")
	cfg := &Config{
		SourcePath: path,
		Project:    ProjectConfig{Org: "testorg", Repos: []string{"repo1"}},
		GitHub:     GitHubConfig{AppID: 1234567},
		AgentRuntimeConfig: AgentRuntimeConfig{
			Agents: map[string]AgentConfig{
				"worker": {Backend: "claude", Model: "claude-sonnet-5-5"},
				"other":  {Backend: "claude"},
			},
		},
		Data: DataConfig{AgentsDir: agentsDir},
	}
	return cfg, path, agentsDir
}

func overlayPaused(t *testing.T, agentsDir, name string) bool {
	t.Helper()
	overlays, err := LoadAgentOverrides(agentsDir)
	if err != nil {
		t.Fatal(err)
	}
	ov, ok := overlays[name]
	if !ok {
		t.Fatalf("no overlay for %s", name)
	}
	return ov.Paused
}

func TestSetAgentPausedAndSave_ResumeClearsOverlayPause(t *testing.T) {
	cfg, path, agentsDir := overlayPauseSyncConfig(t)

	// The agent is paused, then its model is edited: the overlay is written
	// with the pause in it.
	if _, err := cfg.SetAgentPausedAndSave("worker", true); err != nil {
		t.Fatal(err)
	}
	if err := SaveAgentFile(agentsDir, "worker", cfg.Agents["worker"]); err != nil {
		t.Fatal(err)
	}
	// The operator resumes it.
	if _, err := cfg.SetAgentPausedAndSave("worker", false); err != nil {
		t.Fatal(err)
	}
	if overlayPaused(t, agentsDir, "worker") {
		t.Error("overlay still says paused: true after the resume")
	}

	// "Restart": the resumed agent must come back running.
	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if reloaded.Agents["worker"].Paused {
		t.Fatal("a resumed agent came back paused after a restart (stale overlay paused bit)")
	}
	if reloaded.Agents["worker"].Model != "claude-sonnet-5-5" {
		t.Errorf("overlay model = %q, want the edited model kept", reloaded.Agents["worker"].Model)
	}
}

// The other direction: a pause of an agent whose overlay was written while it
// ran must survive a restart too.
func TestSetAgentPausedAndSave_PauseReachesOverlay(t *testing.T) {
	cfg, path, agentsDir := overlayPauseSyncConfig(t)
	if err := SaveAgentFile(agentsDir, "worker", cfg.Agents["worker"]); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.SetAgentPausedAndSave("worker", true); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reloaded.Agents["worker"].Paused {
		t.Fatal("a paused agent came back running after a restart (stale overlay paused bit)")
	}
}

// The tick's reconcile writes the live paused set; it must reach the overlay
// the same way, and an agent with no overlay file must not get one.
func TestReconcilePausedAndSave_SyncsOverlayPause(t *testing.T) {
	cfg, path, agentsDir := overlayPauseSyncConfig(t)
	if err := SaveAgentFile(agentsDir, "worker", cfg.Agents["worker"]); err != nil {
		t.Fatal(err)
	}
	if err := cfg.ReconcilePausedAndSave(map[string]bool{"worker": true, "other": true}); err != nil {
		t.Fatal(err)
	}
	if !overlayPaused(t, agentsDir, "worker") {
		t.Error("reconcile did not record the pause in the overlay")
	}
	if _, err := os.Stat(filepath.Join(agentsDir, "other.yaml")); !os.IsNotExist(err) {
		t.Errorf("reconcile created an overlay for an agent that had none (stat err = %v)", err)
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reloaded.Agents["worker"].Paused || !reloaded.Agents["other"].Paused {
		t.Fatalf("after restart: worker paused=%v other paused=%v, want both paused",
			reloaded.Agents["worker"].Paused, reloaded.Agents["other"].Paused)
	}
}
