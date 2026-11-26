package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadAgentOverrides_Empty(t *testing.T) {
	dir := t.TempDir()
	agents, err := LoadAgentOverrides(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(agents) != 0 {
		t.Errorf("expected 0 agents, got %d", len(agents))
	}
}

func TestLoadAgentOverrides_NonExistent(t *testing.T) {
	agents, err := LoadAgentOverrides("/nonexistent/path")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if agents != nil {
		t.Errorf("expected nil, got %v", agents)
	}
}

func TestLoadAgentOverrides_EmptyPath(t *testing.T) {
	agents, err := LoadAgentOverrides("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if agents != nil {
		t.Errorf("expected nil, got %v", agents)
	}
}

func TestSaveAndLoadAgentFile(t *testing.T) {
	dir := t.TempDir()

	includeRepos := true
	agent := AgentConfig{
		Backend:      "copilot",
		Model:        "claude-sonnet-4-6",
		Enabled:      true,
		DisplayName:  "Docs Writer",
		Description:  "Writes docs",
		SortOrder:    45,
		Emoji:        "📝",
		Color:        "#e91e63",
		BeadRole:     "worker",
		LaneKeywords: []string{"docs", "readme"},
		IncludeRepos: &includeRepos,
	}

	if err := SaveAgentFile(dir, "docs-writer", agent); err != nil {
		t.Fatalf("save failed: %v", err)
	}

	path := filepath.Join(dir, "docs-writer.yaml")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Fatalf("file not created at %s", path)
	}

	agents, err := LoadAgentOverrides(dir)
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}

	loaded, ok := agents["docs-writer"]
	if !ok {
		t.Fatal("docs-writer not found in loaded agents")
	}
	if loaded.DisplayName != "Docs Writer" {
		t.Errorf("display_name = %q, want %q", loaded.DisplayName, "Docs Writer")
	}
	if loaded.SortOrder != 45 {
		t.Errorf("sort_order = %d, want 45", loaded.SortOrder)
	}
	if loaded.Emoji != "📝" {
		t.Errorf("emoji = %q, want 📝", loaded.Emoji)
	}
	if loaded.Color != "#e91e63" {
		t.Errorf("color = %q, want #e91e63", loaded.Color)
	}
	if !loaded.Managed {
		t.Error("loaded agent should have Managed=true")
	}
}

func TestLoadRecoversStagedAgentImport(t *testing.T) {
	const name = "imported-worker"
	for _, layer := range []string{"primary", "dashboard overlay"} {
		t.Run(layer, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("KUBERNETES_SERVICE_HOST", "")
			t.Cleanup(SetSATokenFileForTest(filepath.Join(dir, "no-token")))
			if layer == "dashboard overlay" {
				t.Setenv("KUBERNETES_SERVICE_HOST", "127.0.0.1")
				oldOverlay := DashboardOverlayFile
				DashboardOverlayFile = filepath.Join(dir, "dashboard.yaml")
				t.Cleanup(func() { DashboardOverlayFile = oldOverlay })
			}
			base := minimalValidYAML("testorg", "ghp_test")
			dataDir := fmt.Sprintf("\ndata:\n  agents_dir: %q\n", dir)
			source := writeTempConfig(t, base+dataDir+"removed_agents: [imported-worker]\n")
			if err := os.Chtimes(source, time.Unix(1_700_000_000, 0), time.Unix(1_700_000_000, 0)); err != nil {
				t.Fatal(err)
			}
			// A failed earlier removal can leave an overlay on disk.
			if err := SaveAgentFile(dir, name, AgentConfig{Backend: "claude", DisplayName: "Old worker", Paused: true}); err != nil {
				t.Fatal(err)
			}
			staged := filepath.Join(dir, name+".yaml"+agentImportPendingSuffix)
			if err := os.WriteFile(staged, []byte("backend: copilot\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadWithDashboardOverlay(source)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := cfg.Agents[name]; ok || !cfg.IsAgentRemoved(name) {
				t.Fatal("loading a staged import alone changed the roster")
			}
			committed := base + "  imported-worker:\n    backend: copilot\n    display_name: New worker\n    pause_owner: operator\n" + dataDir +
				"removed_agents: []\ngovernor:\n  modes:\n    idle:\n      imported-worker: 4h\n  cadence_owners:\n    idle:\n      imported-worker: operator\n"
			committedPath := source
			if layer == "dashboard overlay" {
				committedPath = DashboardOverlayFile
			}
			if err := os.WriteFile(committedPath, []byte(committed), 0o600); err != nil {
				t.Fatal(err)
			}
			if layer == "dashboard overlay" {
				// A remount can write a fresh copy of the old seed after the save.
				remountTime := time.Now().Add(time.Hour)
				if err := os.Chtimes(source, remountTime, remountTime); err != nil {
					t.Fatal(err)
				}
			}
			cfg, err = LoadWithDashboardOverlay(source)
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.Agents[name]; !got.Managed || got.DisplayName != "New worker" || got.Paused || cfg.IsAgentRemoved(name) {
				t.Fatalf("agent after recovery = %+v, removed = %t", got, cfg.IsAgentRemoved(name))
			}
			if got := cfg.Governor.Modes["idle"].Cadences[name]; got != "4h" {
				t.Fatalf("cadence after recovery = %q, want 4h", got)
			}
		})
	}
}

func TestSaveImportedAgentRejectsProtectedDashboardOverlay(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "127.0.0.1")
	for _, overlay := range []string{"governor: [", "governor: incompatible\n"} {
		t.Run(overlay, func(t *testing.T) {
			dir := t.TempDir()
			oldOverlay, oldRuntime := DashboardOverlayFile, RuntimeConfigFile
			DashboardOverlayFile = filepath.Join(dir, "dashboard.yaml")
			RuntimeConfigFile = filepath.Join(dir, "runtime.yaml")
			t.Cleanup(func() { DashboardOverlayFile, RuntimeConfigFile = oldOverlay, oldRuntime })
			if err := os.WriteFile(DashboardOverlayFile, []byte(overlay), 0o600); err != nil {
				t.Fatal(err)
			}
			agentsDir := filepath.Join(dir, "agents")
			source := writeTempConfig(t, minimalValidYAML("testorg", "ghp_test")+fmt.Sprintf("\ndata:\n  agents_dir: %q\n", agentsDir))
			cfg, err := LoadWithDashboardOverlay(source)
			if err != nil {
				t.Fatal(err)
			}
			err = cfg.SaveImportedAgent("writer", AgentConfig{Backend: "copilot"}, map[string]Cadence{"idle": "4h"}, "", "")
			if !errors.Is(err, ErrImportAgentOverlay) {
				t.Fatalf("import error = %v, want protected overlay error", err)
			}
			if got, err := os.ReadFile(DashboardOverlayFile); err != nil || string(got) != overlay {
				t.Fatalf("dashboard overlay = %q, %v", got, err)
			}
			for _, path := range []string{RuntimeConfigFile, filepath.Join(agentsDir, "writer.yaml"), filepath.Join(agentsDir, "writer.yaml"+agentImportPendingSuffix)} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Errorf("file %s after refused import: %v, want no file", path, err)
				}
			}
		})
	}
}

func TestReloadReadBeforeImportCannotReplaceIt(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Cleanup(SetSATokenFileForTest(filepath.Join(dir, "no-token")))
	oldRuntime := RuntimeConfigFile
	RuntimeConfigFile = filepath.Join(dir, "runtime.yaml")
	t.Cleanup(func() { RuntimeConfigFile = oldRuntime })
	source := writeTempConfig(t, minimalValidYAML("testorg", "ghp_test")+fmt.Sprintf("\ndata:\n  agents_dir: %q\n", dir))
	cfg, err := LoadWithDashboardOverlay(source)
	if err != nil {
		t.Fatal(err)
	}
	stale, err := LoadWithDashboardOverlay(source)
	if err != nil {
		t.Fatal(err)
	}
	agent := AgentConfig{Backend: "copilot", PauseOwner: FieldOwnerOperator}
	if err := cfg.SaveImportedAgent("writer", agent, map[string]Cadence{"idle": "4h"}, "", ""); err != nil {
		t.Fatal(err)
	}
	if cfg.ReplaceFromReload(stale) {
		t.Fatal("reload from before the import replaced it")
	}
	fresh, err := LoadWithDashboardOverlay(source)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.ReplaceFromReload(fresh) {
		t.Fatal("reload from after the import was rejected")
	}
	if got := cfg.Governor.Modes["idle"].Cadences["writer"]; got != "4h" {
		t.Fatalf("reloaded writer cadence = %q, want 4h", got)
	}
}

func TestRemoveAgentFile(t *testing.T) {
	dir := t.TempDir()

	agent := AgentConfig{Backend: "copilot", Enabled: true}
	if err := SaveAgentFile(dir, "test-agent", agent); err != nil {
		t.Fatalf("save failed: %v", err)
	}

	if err := RemoveAgentFile(dir, "test-agent"); err != nil {
		t.Fatalf("remove failed: %v", err)
	}

	path := filepath.Join(dir, "test-agent.yaml")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("file should have been deleted at %s", path)
	}
}

func TestRemoveAgentFile_NonExistent(t *testing.T) {
	dir := t.TempDir()
	if err := RemoveAgentFile(dir, "no-such-agent"); err != nil {
		t.Errorf("removing non-existent file should not error, got: %v", err)
	}
}

func TestMergeAgentOverrides(t *testing.T) {
	cfg := &Config{
		AgentRuntimeConfig: AgentRuntimeConfig{
			Agents: map[string]AgentConfig{
				"scanner": {Backend: "claude", Enabled: true},
			},
		},
	}

	overlays := map[string]AgentConfig{
		"docs-writer": {Backend: "copilot", Enabled: true, Managed: true},
	}
	cfg.MergeAgentOverrides(overlays)

	if len(cfg.Agents) != 2 {
		t.Fatalf("expected 2 agents, got %d", len(cfg.Agents))
	}
	if !cfg.Agents["docs-writer"].Managed {
		t.Error("overlay agent should have Managed=true")
	}
	if cfg.Agents["scanner"].Managed {
		t.Error("base agent should have Managed=false")
	}
}

func TestMergeAgentOverrides_OverrideExisting(t *testing.T) {
	cfg := &Config{
		AgentRuntimeConfig: AgentRuntimeConfig{
			Agents: map[string]AgentConfig{
				"scanner": {Backend: "claude", Model: "opus", Enabled: true},
			},
		},
	}

	overlays := map[string]AgentConfig{
		"scanner": {Backend: "copilot", Model: "sonnet", Enabled: true, Managed: true},
	}
	cfg.MergeAgentOverrides(overlays)

	if cfg.Agents["scanner"].Backend != "copilot" {
		t.Errorf("overlay should override base: backend = %q, want copilot", cfg.Agents["scanner"].Backend)
	}
}

func TestApplyAgentDefaults(t *testing.T) {
	cfg := &Config{
		AgentRuntimeConfig: AgentRuntimeConfig{
			Agents: map[string]AgentConfig{
				"my-agent": {Backend: "copilot", Enabled: true},
			},
		},
	}
	cfg.ApplyAgentDefaults("my-agent")

	agent := cfg.Agents["my-agent"]
	if agent.ID != "my-agent" {
		t.Errorf("ID = %q, want my-agent", agent.ID)
	}
	if agent.Name() != "my-agent" {
		t.Errorf("Name() = %q, want my-agent", agent.Name())
	}
	if agent.Role != "my-agent" {
		t.Errorf("Role = %q, want my-agent", agent.Role)
	}
	if agent.BeadsDir != "/data/beads/my-agent" {
		t.Errorf("BeadsDir = %q, want /data/beads/my-agent", agent.BeadsDir)
	}
}

func TestLoadSkipsNonYAML(t *testing.T) {
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "subdir.yaml"), 0o755); err != nil {
		t.Fatal(err)
	}

	agents, err := LoadAgentOverrides(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(agents) != 0 {
		t.Errorf("expected 0 agents, got %d", len(agents))
	}
}
