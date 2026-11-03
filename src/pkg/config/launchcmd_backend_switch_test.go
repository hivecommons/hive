package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The incident pairing: the dashboard moved the agent to codex but left the
// bob launch_cmd behind.
const staleBobLaunchCmd = "/usr/bin/bob --allow-all --model claude-sonnet-4-6"

func TestReconcileLaunchCmdForBackend(t *testing.T) {
	g := GovernorConfig{}
	tests := []struct {
		name        string
		backend     string
		launchCmd   string
		want        string
		wantCleared bool
	}{
		{"contradiction is cleared", "codex", staleBobLaunchCmd, "", true},
		{"matching launch_cmd is kept", "bob", staleBobLaunchCmd, staleBobLaunchCmd, false},
		{"empty launch_cmd is kept", "codex", "", "", false},
		{"whitespace launch_cmd is kept", "codex", "  ", "  ", false},
		{"wrapper with no opinion is kept", "codex", "/opt/run-agent.sh --fast", "/opt/run-agent.sh --fast", false},
		{"agent-launch.sh declaring the new backend is kept", "codex", "agent-launch.sh --backend codex", "agent-launch.sh --backend codex", false},
		{"agent-launch.sh declaring the old backend is cleared", "codex", "agent-launch.sh --backend bob", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, cleared := g.ReconcileLaunchCmdForBackend(tt.backend, tt.launchCmd)
			if got != tt.want || cleared != tt.wantCleared {
				t.Errorf("ReconcileLaunchCmdForBackend(%q, %q) = (%q, %v), want (%q, %v)",
					tt.backend, tt.launchCmd, got, cleared, tt.want, tt.wantCleared)
			}
		})
	}
}

// Gap 3 from the outage: the base config (not an overlay) carried the
// contradiction, so the #6024 "fell back to base config" path had nothing
// good to fall back to and the load was fatal. The load must now succeed
// with launch_cmd dropped, backend kept, and the drop recorded for the
// dashboard banner.
func TestLoadWithOverrides_BaseConfigContradictionDegradesInsteadOfFailing(t *testing.T) {
	dir := t.TempDir()
	agentsDir := filepath.Join(dir, "agent-configs")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "hive.yaml")
	base := `
project:
  org: my-org
  repos:
    - repo-a
github:
  token: ghp_tok
data:
  agents_dir: ` + agentsDir + `
agents:
  brainstorm:
    backend: codex
    launch_cmd: ` + staleBobLaunchCmd + `
    enabled: true
  worker:
    backend: bob
    launch_cmd: ` + staleBobLaunchCmd + `
    enabled: true
`
	if err := os.WriteFile(configPath, []byte(base), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadWithOverrides(configPath, "-")
	if err != nil {
		t.Fatalf("LoadWithOverrides() error = %v, want nil (a stale launch_cmd must never take the hive down)", err)
	}
	bs := cfg.Agents["brainstorm"]
	if bs.Backend != "codex" {
		t.Errorf("brainstorm backend = %q, want codex (operator-owned field must win)", bs.Backend)
	}
	if bs.LaunchCmd != "" {
		t.Errorf("brainstorm launch_cmd = %q, want cleared", bs.LaunchCmd)
	}
	// The agent whose launch_cmd agrees with its backend is untouched.
	if w := cfg.Agents["worker"]; w.LaunchCmd != staleBobLaunchCmd || w.Backend != "bob" {
		t.Errorf("worker = %+v, want untouched", w)
	}
	if len(cfg.LaunchCmdDegradations) != 1 {
		t.Fatalf("LaunchCmdDegradations = %+v, want exactly one entry", cfg.LaunchCmdDegradations)
	}
	d := cfg.LaunchCmdDegradations[0]
	if d.Agent != "brainstorm" || d.Backend != "codex" || d.LaunchCmd != staleBobLaunchCmd {
		t.Errorf("degradation = %+v, want brainstorm/codex/%q", d, staleBobLaunchCmd)
	}
	if !strings.Contains(d.Error, "contradicts launch_cmd") {
		t.Errorf("degradation error = %q, want the validator's contradiction wording", d.Error)
	}
}

func TestDegradeContradictingLaunchCmds_SortedAndIdempotent(t *testing.T) {
	cfg := validatableConfig(map[string]AgentConfig{
		"zeta":  {Backend: "codex", LaunchCmd: staleBobLaunchCmd},
		"alpha": {Backend: "claude", LaunchCmd: staleBobLaunchCmd},
		"ok":    {Backend: "bob", LaunchCmd: staleBobLaunchCmd},
		"empty": {Backend: "codex"},
	})
	got := cfg.DegradeContradictingLaunchCmds()
	if len(got) != 2 || got[0].Agent != "alpha" || got[1].Agent != "zeta" {
		t.Fatalf("degraded = %+v, want [alpha zeta]", got)
	}
	if cfg.Agents["zeta"].LaunchCmd != "" || cfg.Agents["alpha"].LaunchCmd != "" {
		t.Errorf("contradicting launch_cmds not cleared: %+v", cfg.Agents)
	}
	if cfg.Agents["ok"].LaunchCmd != staleBobLaunchCmd {
		t.Errorf("matching launch_cmd was cleared")
	}
	// The config is now valid, so a second pass is a no-op.
	if again := cfg.DegradeContradictingLaunchCmds(); len(again) != 0 {
		t.Errorf("second pass degraded %+v, want nothing", again)
	}
	if len(cfg.LaunchCmdDegradations) != 2 {
		t.Errorf("LaunchCmdDegradations = %+v, want the two first-pass entries only", cfg.LaunchCmdDegradations)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() after degrade = %v, want nil", err)
	}
}

// Gap 2: Save must refuse the contradiction the loader would reject, rather
// than write a crash-loop to disk.
func TestSave_RefusesBackendLaunchCmdContradiction(t *testing.T) {
	cfg := newScopeTestConfig(t, map[string]AgentConfig{
		"brainstorm": {Backend: "codex", LaunchCmd: staleBobLaunchCmd},
	})
	err := cfg.Save()
	if err == nil {
		t.Fatal("Save() = nil, want error for backend/launch_cmd contradiction")
	}
	if !strings.Contains(err.Error(), "brainstorm") || !strings.Contains(err.Error(), "contradicts launch_cmd") {
		t.Errorf("Save() error = %v, want agent name and contradiction wording", err)
	}
	if _, statErr := os.Stat(cfg.SourcePath); !os.IsNotExist(statErr) {
		t.Errorf("hive.yaml was written despite the refusal (stat err = %v)", statErr)
	}

	// Clearing launch_cmd (what the dashboard now does) makes the save go through.
	a := cfg.Agents["brainstorm"]
	a.LaunchCmd = ""
	cfg.Agents["brainstorm"] = a
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save() after clearing launch_cmd = %v, want nil", err)
	}
}

func TestValidateAgentLaunchCmds(t *testing.T) {
	cfg := validatableConfig(map[string]AgentConfig{
		"b": {Backend: "codex", LaunchCmd: staleBobLaunchCmd},
		"a": {Backend: "bob", LaunchCmd: staleBobLaunchCmd},
	})
	err := cfg.ValidateAgentLaunchCmds()
	if err == nil || !strings.HasPrefix(err.Error(), "agent b") {
		t.Errorf("ValidateAgentLaunchCmds() = %v, want error naming agent b", err)
	}
	if err := validatableConfig(map[string]AgentConfig{"a": {Backend: "codex"}}).ValidateAgentLaunchCmds(); err != nil {
		t.Errorf("ValidateAgentLaunchCmds() on valid config = %v, want nil", err)
	}
}

func TestSaveAgentFile_RefusesBackendLaunchCmdContradiction(t *testing.T) {
	dir := t.TempDir()
	err := SaveAgentFile(dir, "brainstorm", AgentConfig{Backend: "codex", LaunchCmd: staleBobLaunchCmd})
	if err == nil {
		t.Fatal("SaveAgentFile() = nil, want error for backend/launch_cmd contradiction")
	}
	if !strings.Contains(err.Error(), "refusing to save agent overlay brainstorm") {
		t.Errorf("SaveAgentFile() error = %v, want refusal wording", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "brainstorm.yaml")); !os.IsNotExist(statErr) {
		t.Errorf("overlay was written despite the refusal (stat err = %v)", statErr)
	}
	if err := SaveAgentFile(dir, "brainstorm", AgentConfig{Backend: "bob", LaunchCmd: staleBobLaunchCmd}); err != nil {
		t.Errorf("SaveAgentFile() with matching launch_cmd = %v, want nil", err)
	}
}
