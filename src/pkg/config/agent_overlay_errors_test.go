package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests cover the error paths of the agent-overlay lane that the happy-path
// suite (agent_overlay_test.go) and the #6024 reject-gate suite
// (agent_overlay_reject_test.go) leave untouched: LoadAgentOverrides' three
// failure returns, the four validateAgentOverlay gates the reject tests don't
// trip, and the filesystem failure returns in SaveAgentFile / RemoveAgentFile.

// LoadAgentOverrides distinguishes "the overlay dir does not exist" (a normal,
// nil-error state) from every other ReadDir failure, which must surface: a
// swallowed ENOTDIR would silently boot the hive without its overlays.
func TestLoadAgentOverrides_DirIsAFile(t *testing.T) {
	dir := t.TempDir()
	notADir := filepath.Join(dir, "agents")
	if err := os.WriteFile(notADir, []byte("plain file"), 0o644); err != nil {
		t.Fatal(err)
	}

	agents, err := LoadAgentOverrides(notADir)
	if err == nil {
		t.Fatalf("LoadAgentOverrides(%q) = nil error, want a reading-dir error", notADir)
	}
	if !strings.Contains(err.Error(), "reading agent overlay dir") {
		t.Errorf("error = %v, want it to name the overlay dir read", err)
	}
	if agents != nil {
		t.Errorf("agents = %v, want nil on error", agents)
	}
}

// An entry that matches *.yaml but cannot be read (here: a dangling symlink)
// must fail the load with an error naming the file, not vanish silently.
func TestLoadAgentOverrides_UnreadableFile(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "broken.yaml")
	if err := os.Symlink(filepath.Join(dir, "no-such-target"), link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	_, err := LoadAgentOverrides(dir)
	if err == nil {
		t.Fatal("LoadAgentOverrides() = nil error, want a read error for the dangling symlink")
	}
	if !strings.Contains(err.Error(), "broken.yaml") {
		t.Errorf("error = %v, want it to name broken.yaml", err)
	}
}

func TestLoadAgentOverrides_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mangled.yaml")
	if err := os.WriteFile(path, []byte("backend: [unclosed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadAgentOverrides(dir)
	if err == nil {
		t.Fatal("LoadAgentOverrides() = nil error, want a parse error")
	}
	if !strings.Contains(err.Error(), "parsing agent file") || !strings.Contains(err.Error(), "mangled.yaml") {
		t.Errorf("error = %v, want a parsing error naming mangled.yaml", err)
	}
}

// The reject gate must trip on every validateAgentOverlay branch, not just the
// backend/launch_cmd contradiction and caveman_mode the #6024 tests exercise.
// Each of these overlays would have failed the whole config load pre-#6024, so
// each must be rejected — and a rejection that stopped checking after the first
// two gates would let a dormant-agent config (bad channel type, #5591) boot.
func TestRejectInvalidAgentOverlays_AllValidationGates(t *testing.T) {
	tests := []struct {
		name    string
		overlay AgentConfig
	}{
		{"unsupported backend", AgentConfig{Backend: "nonsense-backend"}},
		{"invalid explain_mode", AgentConfig{Backend: "claude", ExplainMode: "verbose"}},
		{"channel type without runtime", AgentConfig{Backend: "claude", Channels: []ChannelConfig{{Type: "webhook"}}}},
		{"invalid tools preset", AgentConfig{Backend: "claude", Tools: &ToolsConfig{Preset: "bogus"}}},
		{"connection missing name", AgentConfig{Backend: "claude", Connections: []ConnectionConfig{{Type: "mcp", URI: "http://localhost:1"}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{}
			kept := cfg.RejectInvalidAgentOverlays(map[string]AgentConfig{"bad": tt.overlay})
			if _, ok := kept["bad"]; ok {
				t.Errorf("overlay with %s was kept, want rejected", tt.name)
			}
		})
	}
}

// SaveAgentFile's atomic write ends in a rename; a rename that fails (here:
// the destination is a directory) must surface, or the caller believes a
// config was persisted that was not.
func TestSaveAgentFile_RenameFails(t *testing.T) {
	dir := t.TempDir()
	// Occupy the destination path with a directory so os.Rename cannot
	// replace it with the temp file.
	if err := os.Mkdir(filepath.Join(dir, "worker.yaml"), 0o755); err != nil {
		t.Fatal(err)
	}

	err := SaveAgentFile(dir, "worker", AgentConfig{Backend: "claude"})
	if err == nil {
		t.Fatal("SaveAgentFile() = nil error, want a rename error")
	}
	if !strings.Contains(err.Error(), "renaming agent file") {
		t.Errorf("error = %v, want a renaming error", err)
	}
}

// RemoveAgentFile treats "already gone" as success, but any OTHER removal
// failure (here: the path is a non-empty directory) must be reported — the
// caller is about to tell the operator the agent's overlay is deleted.
func TestRemoveAgentFile_RemoveFails(t *testing.T) {
	dir := t.TempDir()
	occupied := filepath.Join(dir, "worker.yaml")
	if err := os.MkdirAll(filepath.Join(occupied, "child"), 0o755); err != nil {
		t.Fatal(err)
	}

	err := RemoveAgentFile(dir, "worker")
	if err == nil {
		t.Fatal("RemoveAgentFile() = nil error, want a removal error for a non-empty directory")
	}
	if !strings.Contains(err.Error(), "removing agent file") {
		t.Errorf("error = %v, want a removing error", err)
	}
}
