package config

import (
	"os"
	"path/filepath"
	"testing"
)

// #13221: ApplyPack persists reconciled agents to the overlay layers with
// backend_owner: pack. A later hand edit of `backend:` in hive.yaml must not
// be shadowed by that stale pack value on the next load.
func TestMergeAgentOverrides_HandSetBackendVsPackOverlay(t *testing.T) {
	tests := []struct {
		name        string
		base        AgentConfig
		overlay     AgentConfig
		wantBackend string
		wantOwner   string
	}{
		{
			name:        "hand-set base backend beats pack-owned overlay",
			base:        AgentConfig{Backend: "claude", Enabled: true},
			overlay:     AgentConfig{Backend: "copilot", BackendOwner: FieldOwnerPack, Enabled: true},
			wantBackend: "claude",
			wantOwner:   "",
		},
		{
			name:        "empty base backend keeps pack overlay value",
			base:        AgentConfig{Enabled: true},
			overlay:     AgentConfig{Backend: "copilot", BackendOwner: FieldOwnerPack, Enabled: true},
			wantBackend: "copilot",
			wantOwner:   FieldOwnerPack,
		},
		{
			name:        "operator-owned overlay backend is kept",
			base:        AgentConfig{Backend: "claude", Enabled: true},
			overlay:     AgentConfig{Backend: "copilot", BackendOwner: FieldOwnerOperator, Enabled: true},
			wantBackend: "copilot",
			wantOwner:   FieldOwnerOperator,
		},
		{
			name:        "unowned overlay backend still overrides base",
			base:        AgentConfig{Backend: "claude", Enabled: true},
			overlay:     AgentConfig{Backend: "copilot", Enabled: true},
			wantBackend: "copilot",
			wantOwner:   "",
		},
		{
			name:        "pack-stamped base backend loses to pack overlay",
			base:        AgentConfig{Backend: "claude", BackendOwner: FieldOwnerPack, Enabled: true},
			overlay:     AgentConfig{Backend: "copilot", BackendOwner: FieldOwnerPack, Enabled: true},
			wantBackend: "copilot",
			wantOwner:   FieldOwnerPack,
		},
		{
			name:        "same backend keeps pack ownership",
			base:        AgentConfig{Backend: "copilot", Enabled: true},
			overlay:     AgentConfig{Backend: "copilot", BackendOwner: FieldOwnerPack, Enabled: true},
			wantBackend: "copilot",
			wantOwner:   FieldOwnerPack,
		},
		{
			name:        "invalid hand-set backend does not displace overlay",
			base:        AgentConfig{Backend: "no-such-backend", Enabled: true},
			overlay:     AgentConfig{Backend: "copilot", BackendOwner: FieldOwnerPack, Enabled: true},
			wantBackend: "copilot",
			wantOwner:   FieldOwnerPack,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{AgentRuntimeConfig: AgentRuntimeConfig{Agents: map[string]AgentConfig{"brainstorm": tt.base}}}
			cfg.MergeAgentOverrides(map[string]AgentConfig{"brainstorm": tt.overlay})
			got := cfg.Agents["brainstorm"]
			if got.Backend != tt.wantBackend {
				t.Errorf("backend = %q, want %q", got.Backend, tt.wantBackend)
			}
			if got.BackendOwner != tt.wantOwner {
				t.Errorf("backend_owner = %q, want %q", got.BackendOwner, tt.wantOwner)
			}
			if !got.Managed {
				t.Error("merged agent should be Managed")
			}
		})
	}
}

// End to end through the load path the issue hit: hive.yaml hand-sets the
// backend, and a previous boot's pack apply left a pack-owned overlay file.
func TestLoadWithOverrides_HandSetBackendBeatsPackOverlayFile(t *testing.T) {
	configPath, _ := writeOverlayConfig(t, map[string]string{
		"worker": "backend: copilot\nbackend_owner: pack\nmodel: claude-sonnet-4-6\nmodel_owner: pack\nenabled: true\n",
	})

	cfg, err := LoadWithOverrides(configPath, "-")
	if err != nil {
		t.Fatalf("LoadWithOverrides() error = %v", err)
	}
	worker := cfg.Agents["worker"]
	if worker.Backend != "claude" {
		t.Errorf("worker backend = %q, want hand-set claude", worker.Backend)
	}
	if worker.BackendOwner != "" {
		t.Errorf("worker backend_owner = %q, want empty so ApplyPack stamps it operator", worker.BackendOwner)
	}
	if worker.ModelOwner != FieldOwnerPack {
		t.Errorf("worker model_owner = %q, want pack (only backend is restored)", worker.ModelOwner)
	}
}

// The dashboard overlay layer (Kubernetes mode) re-merges agents through the
// same function, so a hand-set backend must survive it too.
func TestLoadWithDashboardOverlay_HandSetBackendBeatsPackOverlay(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	configPath, _ := writeOverlayConfig(t, nil)
	overlayPath := filepath.Join(filepath.Dir(configPath), "hive.yaml.dashboard")
	overlay := `
project:
  org: my-org
agents:
  worker:
    backend: copilot
    backend_owner: pack
    enabled: true
`
	if err := os.WriteFile(overlayPath, []byte(overlay), 0o600); err != nil {
		t.Fatalf("writing dashboard overlay: %v", err)
	}
	origOverlay := DashboardOverlayFile
	DashboardOverlayFile = overlayPath
	t.Cleanup(func() { DashboardOverlayFile = origOverlay })

	cfg, err := LoadWithDashboardOverlay(configPath)
	if err != nil {
		t.Fatalf("LoadWithDashboardOverlay() error = %v", err)
	}
	if got := cfg.Agents["worker"].Backend; got != "claude" {
		t.Errorf("worker backend = %q, want hand-set claude", got)
	}
}
