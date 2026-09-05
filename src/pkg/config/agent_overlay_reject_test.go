package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// overlayHiveYAML is a minimal valid config that points data.agents_dir at
// agentsDir, so LoadWithOverrides takes the per-agent overlay path that #6024
// crashed on. The base config declares BOTH agents the overlay tests touch, so
// a rejected overlay has a base entry to fall back to.
func overlayHiveYAML(agentsDir string) string {
	return `
project:
  org: my-org
  repos:
    - repo-a
github:
  token: ghp_tok
data:
  agents_dir: ` + agentsDir + `
agents:
  supervisor:
    backend: copilot
    enabled: true
  worker:
    backend: claude
    enabled: true
`
}

// writeOverlayConfig lays down a hive.yaml plus an agent-overlay directory and
// returns the config path.
func writeOverlayConfig(t *testing.T, overlays map[string]string) (configPath, agentsDir string) {
	t.Helper()
	dir := t.TempDir()
	agentsDir = filepath.Join(dir, "agent-configs")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatalf("mkdir agents dir: %v", err)
	}
	for name, body := range overlays {
		path := filepath.Join(agentsDir, name+".yaml")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("writing overlay %s: %v", path, err)
		}
	}
	configPath = filepath.Join(dir, "hive.yaml")
	if err := os.WriteFile(configPath, []byte(overlayHiveYAML(agentsDir)), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return configPath, agentsDir
}

// validatableConfig returns a Config that clears every non-agent gate in
// validate(), so the only thing a test's agents can trip is the agent loop.
func validatableConfig(agents map[string]AgentConfig) *Config {
	cfg := &Config{Agents: agents}
	cfg.Project.Org = "my-org"
	cfg.GitHub.Token = "ghp_tok"
	return cfg
}

// The #6024 regression: one contradictory per-agent overlay file used to fail
// the whole config load, so the process exited before the dashboard bound and
// the only documented fix (the dashboard API) was unreachable. The load must
// now succeed with that one overlay skipped.
func TestLoadWithOverrides_ContradictoryOverlayDoesNotFailLoad(t *testing.T) {
	configPath, _ := writeOverlayConfig(t, map[string]string{
		// The exact pairing from the incident: declared copilot, launches bob.
		"supervisor": "backend: copilot\nlaunch_cmd: bob --model auto\n",
	})

	cfg, err := LoadWithOverrides(configPath, "-")
	if err != nil {
		t.Fatalf("LoadWithOverrides() error = %v, want nil (a bad overlay must not brick the hive)", err)
	}
	// The other agents survive untouched.
	if _, ok := cfg.Agents["worker"]; !ok {
		t.Errorf("worker missing from Agents; a rejected overlay must not take other agents with it")
	}
	// The offending agent falls back to the BASE entry, not to nothing:
	// dropping it would silently change which agents run.
	sup, ok := cfg.Agents["supervisor"]
	if !ok {
		t.Fatalf("supervisor missing from Agents; want the base config entry to survive")
	}
	if sup.LaunchCmd != "" {
		t.Errorf("supervisor.LaunchCmd = %q, want %q (the bad overlay value must not be merged)", sup.LaunchCmd, "")
	}
	if sup.Backend != "copilot" {
		t.Errorf("supervisor.Backend = %q, want %q (from the base config)", sup.Backend, "copilot")
	}
	if sup.SourceFile() != "" {
		t.Errorf("supervisor.SourceFile() = %q, want %q (it came from the base config, not the overlay)", sup.SourceFile(), "")
	}
}

// A valid overlay must still apply normally - the reject gate is not allowed
// to become a blanket "ignore the overlay directory".
func TestLoadWithOverrides_ValidOverlayStillApplies(t *testing.T) {
	configPath, agentsDir := writeOverlayConfig(t, map[string]string{
		"worker": "backend: claude\nlaunch_cmd: claude --dangerously-skip-permissions\nmodel: claude-sonnet-4-6\n",
	})

	cfg, err := LoadWithOverrides(configPath, "-")
	if err != nil {
		t.Fatalf("LoadWithOverrides() error = %v", err)
	}
	worker, ok := cfg.Agents["worker"]
	if !ok {
		t.Fatalf("worker missing from Agents")
	}
	if worker.Model != "claude-sonnet-4-6" {
		t.Errorf("worker.Model = %q, want %q (the valid overlay must apply)", worker.Model, "claude-sonnet-4-6")
	}
	if !worker.Managed {
		t.Errorf("worker.Managed = false, want true for an overlay-loaded agent")
	}
	want := filepath.Join(agentsDir, "worker.yaml")
	if worker.SourceFile() != want {
		t.Errorf("worker.SourceFile() = %q, want %q", worker.SourceFile(), want)
	}
}

// One bad overlay must not take a good one down with it.
func TestLoadWithOverrides_BadOverlayDoesNotBlockGoodOverlay(t *testing.T) {
	configPath, _ := writeOverlayConfig(t, map[string]string{
		"supervisor": "backend: copilot\nlaunch_cmd: bob --model auto\n",
		"worker":     "backend: claude\nmodel: claude-opus-4-6\n",
	})

	cfg, err := LoadWithOverrides(configPath, "-")
	if err != nil {
		t.Fatalf("LoadWithOverrides() error = %v", err)
	}
	if got := cfg.Agents["worker"].Model; got != "claude-opus-4-6" {
		t.Errorf("worker.Model = %q, want %q (the good overlay must still apply)", got, "claude-opus-4-6")
	}
	if got := cfg.Agents["supervisor"].LaunchCmd; got != "" {
		t.Errorf("supervisor.LaunchCmd = %q, want %q (the bad overlay must be dropped)", got, "")
	}
}

// RejectInvalidAgentOverlays is the seam itself: bad entries out, good ones
// through, base config untouched.
func TestRejectInvalidAgentOverlays(t *testing.T) {
	cfg := &Config{Agents: map[string]AgentConfig{
		"supervisor": {Backend: "copilot"},
	}}
	overlays := map[string]AgentConfig{
		"supervisor": {Backend: "copilot", LaunchCmd: "bob --model auto", sourceFile: "/data/agent-configs/supervisor.yaml"},
		"worker":     {Backend: "claude", sourceFile: "/data/agent-configs/worker.yaml"},
		"badmode":    {Backend: "claude", CavemanMode: "nonsense", sourceFile: "/data/agent-configs/badmode.yaml"},
	}

	kept := cfg.RejectInvalidAgentOverlays(overlays)
	if _, ok := kept["supervisor"]; ok {
		t.Errorf("supervisor overlay kept, want rejected (backend/launch_cmd contradiction)")
	}
	if _, ok := kept["badmode"]; ok {
		t.Errorf("badmode overlay kept, want rejected (invalid caveman_mode)")
	}
	if _, ok := kept["worker"]; !ok {
		t.Errorf("worker overlay rejected, want kept (it is valid)")
	}
	// The gate must not mutate the caller's base config.
	if got := cfg.Agents["supervisor"].Backend; got != "copilot" {
		t.Errorf("base supervisor.Backend = %q, want %q (the gate must not mutate the base config)", got, "copilot")
	}
}

func TestRejectInvalidAgentOverlays_EmptyIsPassthrough(t *testing.T) {
	cfg := &Config{}
	if got := cfg.RejectInvalidAgentOverlays(nil); got != nil {
		t.Errorf("RejectInvalidAgentOverlays(nil) = %v, want nil", got)
	}
}

// LoadAgentOverrides must stamp the file each entry came from - the provenance
// the error message in #6024 was missing.
func TestLoadAgentOverrides_RecordsSourceFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "supervisor.yaml")
	if err := os.WriteFile(path, []byte("backend: copilot\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	agents, err := LoadAgentOverrides(dir)
	if err != nil {
		t.Fatalf("LoadAgentOverrides() error = %v", err)
	}
	sup, ok := agents["supervisor"]
	if !ok {
		t.Fatalf("supervisor missing")
	}
	if sup.SourceFile() != path {
		t.Errorf("SourceFile() = %q, want %q", sup.SourceFile(), path)
	}
}

// SaveAgentFile must not round-trip the provenance stamp back into the file:
// sourceFile describes where an entry was READ from, and persisting it would
// make it a config field.
func TestSaveAgentFile_DropsSourceFile(t *testing.T) {
	dir := t.TempDir()
	agent := AgentConfig{Backend: "claude", sourceFile: "/somewhere/else.yaml"}
	if err := SaveAgentFile(dir, "worker", agent); err != nil {
		t.Fatalf("SaveAgentFile() error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "worker.yaml"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(data), "else.yaml") {
		t.Errorf("saved overlay leaked the source path:\n%s", data)
	}
}

// The validation error must name the FILE, not just the agent. Naming only the
// agent is what made #6024 take hours: hive.yaml, the ConfigMap seed and the
// overlay directory all feed the same agent map.
func TestValidate_ErrorNamesOverlaySourceFile(t *testing.T) {
	cfg := validatableConfig(map[string]AgentConfig{
		"supervisor": {
			Backend:    "copilot",
			LaunchCmd:  "bob --model auto",
			sourceFile: "/data/agent-configs/supervisor.yaml",
		},
	})

	err := cfg.validate()
	if err == nil {
		t.Fatalf("validateAgents() = nil, want a contradiction error")
	}
	if !strings.Contains(err.Error(), "/data/agent-configs/supervisor.yaml") {
		t.Errorf("error does not name the overlay file: %v", err)
	}
	if !strings.Contains(err.Error(), "supervisor") {
		t.Errorf("error does not name the agent: %v", err)
	}
}

// An agent that came from the main config has no source file, and the message
// must stay exactly as it was rather than growing an empty parenthetical.
func TestValidate_ErrorOmitsSourceForBaseConfigAgent(t *testing.T) {
	cfg := validatableConfig(map[string]AgentConfig{
		"supervisor": {Backend: "copilot", LaunchCmd: "bob --model auto"},
	})

	err := cfg.validate()
	if err == nil {
		t.Fatalf("validateAgents() = nil, want a contradiction error")
	}
	if strings.Contains(err.Error(), "(from ") {
		t.Errorf("error names a source file for a base-config agent: %v", err)
	}
	if !strings.HasPrefix(err.Error(), "agent supervisor: ") {
		t.Errorf("error = %q, want it to start with %q", err.Error(), "agent supervisor: ")
	}
}

func TestAgentSourceLabel(t *testing.T) {
	tests := []struct {
		name       string
		agent      string
		sourceFile string
		want       string
	}{
		{"base config agent", "supervisor", "", "supervisor"},
		{"overlay agent", "supervisor", "/data/agent-configs/supervisor.yaml", "supervisor (from /data/agent-configs/supervisor.yaml)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := agentSourceLabel(tt.agent, tt.sourceFile); got != tt.want {
				t.Errorf("agentSourceLabel(%q, %q) = %q, want %q", tt.agent, tt.sourceFile, got, tt.want)
			}
		})
	}
}
