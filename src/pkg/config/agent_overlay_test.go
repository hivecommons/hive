package config

import (
	"os"
	"path/filepath"
	"testing"
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
		Agents: map[string]AgentConfig{
			"scanner": {Backend: "claude", Enabled: true},
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
		Agents: map[string]AgentConfig{
			"scanner": {Backend: "claude", Model: "opus", Enabled: true},
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
		Agents: map[string]AgentConfig{
			"my-agent": {Backend: "copilot", Enabled: true},
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
