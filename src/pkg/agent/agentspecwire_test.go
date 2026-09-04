package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestApplyAgentSpecConfigChangesLaunchCommand(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.yaml")
	spec := `name: custom-reviewer
backend: claude
model: claude-opus-4-6
mode: autonomous
launch_cmd: /opt/agents/reviewer --stdio --model claude-opus-4-6
prompt: "Use the external review checklist."
tools:
  preset: full
skills:
  - review-checklist
`
	if err := os.WriteFile(path, []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, prompt, applied, err := applyAgentSpecConfig(config.AgentConfig{
		Backend:   "copilot",
		Model:     "auto",
		Mode:      "ADVISORY",
		LaunchCmd: "copilot --model auto",
		AgentSpec: path,
	})
	if err != nil {
		t.Fatalf("applyAgentSpecConfig: %v", err)
	}
	if !applied {
		t.Fatal("agent spec was not applied")
	}
	if cfg.Backend != "claude" {
		t.Errorf("backend = %q, want claude", cfg.Backend)
	}
	if cfg.Model != "claude-opus-4-6" {
		t.Errorf("model = %q", cfg.Model)
	}
	if cfg.Mode != "ISSUES_PRS_MERGE" {
		t.Errorf("mode = %q, want ISSUES_PRS_MERGE", cfg.Mode)
	}
	if cfg.LaunchCmd != "/opt/agents/reviewer --stdio --model claude-opus-4-6" {
		t.Errorf("launch_cmd = %q", cfg.LaunchCmd)
	}
	if prompt != "Use the external review checklist." {
		t.Errorf("prompt = %q", prompt)
	}
	if cfg.Tools == nil || cfg.Tools.Preset != "full" {
		t.Fatalf("tools = %+v, want full preset", cfg.Tools)
	}
	if len(cfg.Skills) != 1 || cfg.Skills[0] != "review-checklist" {
		t.Errorf("skills = %v", cfg.Skills)
	}
}

func TestApplyAgentSpecConfigDirectoryRef(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "spec.yaml"), []byte("name: a\nbackend: copilot\nmodel: auto\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _, applied, err := applyAgentSpecConfig(config.AgentConfig{AgentSpec: dir, LaunchCmd: "stale custom launcher"})
	if err != nil {
		t.Fatalf("applyAgentSpecConfig(dir): %v", err)
	}
	if !applied || cfg.Backend != "copilot" || cfg.Model != "auto" {
		t.Fatalf("cfg = %+v applied=%v", cfg, applied)
	}
	if cfg.LaunchCmd != "" {
		t.Fatalf("launch_cmd = %q, want cleared when spec omits launch_cmd", cfg.LaunchCmd)
	}
}

func TestApplyAgentSpecNoRefAndErrors(t *testing.T) {
	cfg, prompt, applied, err := applyAgentSpecConfig(config.AgentConfig{Backend: "copilot"})
	if err != nil {
		t.Fatalf("no-ref error = %v", err)
	}
	if applied || prompt != "" || cfg.Backend != "copilot" {
		t.Fatalf("cfg=%+v prompt=%q applied=%v", cfg, prompt, applied)
	}
	if _, _, _, err := applyAgentSpecConfig(config.AgentConfig{AgentSpec: filepath.Join(t.TempDir(), "missing.yaml")}); err == nil {
		t.Fatal("missing agent_spec should error")
	}
}

func TestApplyAgentSpecSetsBootstrapPromptUnlessOverridden(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.yaml")
	if err := os.WriteFile(path, []byte("name: a\nbackend: copilot\nmodel: auto\nmode: observe\nprompt: spec prompt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := &Manager{}
	agent := &AgentProcess{Name: "a", Config: config.AgentConfig{AgentSpec: path}}
	if err := m.applyAgentSpec(agent); err != nil {
		t.Fatalf("applyAgentSpec: %v", err)
	}
	if agent.BootstrapOverride != "spec prompt" {
		t.Fatalf("BootstrapOverride = %q", agent.BootstrapOverride)
	}
	if agent.Config.Mode != "ADVISORY" {
		t.Fatalf("mode = %q", agent.Config.Mode)
	}

	agent = &AgentProcess{Name: "a", BootstrapOverride: "operator prompt", Config: config.AgentConfig{AgentSpec: path}}
	if err := m.applyAgentSpec(agent); err != nil {
		t.Fatalf("applyAgentSpec with override: %v", err)
	}
	if agent.BootstrapOverride != "operator prompt" {
		t.Fatalf("BootstrapOverride = %q, want operator prompt", agent.BootstrapOverride)
	}
}

func TestApplyAgentSpecUpdatesAndClearsSpecOwnedPrompt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.yaml")
	if err := os.WriteFile(path, []byte("name: a\nbackend: copilot\nmodel: auto\nmode: observe\nprompt: first prompt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	agent := &AgentProcess{Name: "a", Config: config.AgentConfig{AgentSpec: path}}
	m := &Manager{}
	if err := m.applyAgentSpec(agent); err != nil {
		t.Fatalf("applyAgentSpec first: %v", err)
	}
	if agent.BootstrapOverride != "first prompt" {
		t.Fatalf("BootstrapOverride = %q", agent.BootstrapOverride)
	}

	if err := os.WriteFile(path, []byte("name: a\nbackend: copilot\nmodel: auto\nmode: observe\nprompt: updated prompt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.applyAgentSpec(agent); err != nil {
		t.Fatalf("applyAgentSpec update: %v", err)
	}
	if agent.BootstrapOverride != "updated prompt" {
		t.Fatalf("BootstrapOverride after update = %q", agent.BootstrapOverride)
	}

	if err := os.WriteFile(path, []byte("name: a\nbackend: copilot\nmodel: auto\nmode: observe\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.applyAgentSpec(agent); err != nil {
		t.Fatalf("applyAgentSpec removal: %v", err)
	}
	if agent.BootstrapOverride != "" {
		t.Fatalf("BootstrapOverride after prompt removal = %q", agent.BootstrapOverride)
	}
}

func TestApplyAgentSpecDoesNotReplaceOperatorPromptOnSpecUpdate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.yaml")
	if err := os.WriteFile(path, []byte("name: a\nbackend: copilot\nmodel: auto\nmode: observe\nprompt: spec prompt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	agent := &AgentProcess{Name: "a", Config: config.AgentConfig{AgentSpec: path}}
	m := &Manager{}
	if err := m.applyAgentSpec(agent); err != nil {
		t.Fatalf("applyAgentSpec first: %v", err)
	}
	agent.BootstrapOverride = "operator prompt"
	if err := os.WriteFile(path, []byte("name: a\nbackend: copilot\nmodel: auto\nmode: observe\nprompt: changed spec prompt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.applyAgentSpec(agent); err != nil {
		t.Fatalf("applyAgentSpec update: %v", err)
	}
	if agent.BootstrapOverride != "operator prompt" {
		t.Fatalf("BootstrapOverride = %q, want operator prompt", agent.BootstrapOverride)
	}
}

func TestUpdateConfigRemovingAgentSpecClearsSpecOwnedPrompt(t *testing.T) {
	agent := &AgentProcess{
		Name:              "a",
		Config:            config.AgentConfig{AgentSpec: "agent.yaml"},
		baseConfig:        config.AgentConfig{AgentSpec: "agent.yaml"},
		BootstrapOverride: "spec prompt",
		agentSpecPrompt:   "spec prompt",
	}
	m := &Manager{agents: map[string]*AgentProcess{"a": agent}}

	if err := m.UpdateConfig("a", config.AgentConfig{Backend: "copilot"}); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	if agent.BootstrapOverride != "" || agent.agentSpecPrompt != "" {
		t.Fatalf("spec prompt survived removal: bootstrap=%q tracked=%q", agent.BootstrapOverride, agent.agentSpecPrompt)
	}
}

func TestUpdateConfigRemovingAgentSpecPreservesOperatorPrompt(t *testing.T) {
	agent := &AgentProcess{
		Name:              "a",
		Config:            config.AgentConfig{AgentSpec: "agent.yaml"},
		baseConfig:        config.AgentConfig{AgentSpec: "agent.yaml"},
		BootstrapOverride: "operator prompt",
		agentSpecPrompt:   "spec prompt",
	}
	m := &Manager{agents: map[string]*AgentProcess{"a": agent}}

	if err := m.UpdateConfig("a", config.AgentConfig{Backend: "copilot"}); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	if agent.BootstrapOverride != "operator prompt" || agent.agentSpecPrompt != "" {
		t.Fatalf("operator prompt not preserved: bootstrap=%q tracked=%q", agent.BootstrapOverride, agent.agentSpecPrompt)
	}
}

func TestApplyAgentSpecUsesBaseConfigAndRevokesRemovedSpecFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.yaml")
	if err := os.WriteFile(path, []byte("name: a\nbackend: claude\nmodel: sonnet\nmode: suggest\nlaunch_cmd: first\ntools:\n  preset: full\nskills:\n  - broad\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	base := config.AgentConfig{
		AgentSpec: path,
		Backend:   "copilot",
		Model:     "auto",
		Tools:     &config.ToolsConfig{Preset: "advisory"},
		Skills:    []string{"base-skill"},
	}
	agent := &AgentProcess{Name: "a", Config: base, baseConfig: base}
	m := &Manager{}
	if err := m.applyAgentSpec(agent); err != nil {
		t.Fatalf("applyAgentSpec first: %v", err)
	}
	if agent.Config.LaunchCmd != "first" || agent.Config.Tools == nil || agent.Config.Tools.Preset != "full" || len(agent.Config.Skills) != 1 {
		t.Fatalf("first apply cfg = %+v", agent.Config)
	}

	if err := os.WriteFile(path, []byte("name: a\nbackend: claude\nmodel: sonnet\nmode: suggest\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.applyAgentSpec(agent); err != nil {
		t.Fatalf("applyAgentSpec second: %v", err)
	}
	if agent.Config.LaunchCmd != "" || agent.Config.Tools != nil || agent.Config.Skills != nil {
		t.Fatalf("removed spec fields persisted: launch=%q tools=%+v skills=%v", agent.Config.LaunchCmd, agent.Config.Tools, agent.Config.Skills)
	}
}

func TestRestartValidatesAgentSpecBeforeTeardown(t *testing.T) {
	cancelled := false
	agent := &AgentProcess{
		Name:         "a",
		State:        StateRunning,
		Paused:       true,
		RestartCount: 7,
		Config: config.AgentConfig{
			AgentSpec: filepath.Join(t.TempDir(), "missing.yaml"),
		},
		cancel: func() { cancelled = true },
	}
	m := &Manager{agents: map[string]*AgentProcess{"a": agent}}

	if err := m.Restart(context.Background(), "a"); err == nil {
		t.Fatal("Restart with missing agent_spec succeeded")
	}
	if cancelled {
		t.Fatal("Restart cancelled running agent before validating agent_spec")
	}
	if agent.RestartCount != 7 || agent.forceRelaunch {
		t.Fatalf("agent teardown fields changed: restart_count=%d force=%v", agent.RestartCount, agent.forceRelaunch)
	}
}

func TestRestartWithBootstrapValidatesAgentSpecBeforeMutating(t *testing.T) {
	agent := &AgentProcess{
		Name:              "a",
		State:             StateRunning,
		Paused:            true,
		BootstrapOverride: "existing",
		Config: config.AgentConfig{
			AgentSpec: filepath.Join(t.TempDir(), "missing.yaml"),
		},
		cancel: func() { t.Fatal("cancel called before agent_spec validation") },
	}
	m := &Manager{agents: map[string]*AgentProcess{"a": agent}}

	if err := m.RestartWithBootstrap(context.Background(), "a", "replacement"); err == nil {
		t.Fatal("RestartWithBootstrap with missing agent_spec succeeded")
	}
	if agent.BootstrapOverride != "existing" || !agent.Paused {
		t.Fatalf("agent mutated before validation: bootstrap=%q paused=%v", agent.BootstrapOverride, agent.Paused)
	}
}

func TestAgentSpecModeMappings(t *testing.T) {
	if got := agentSpecMode("unexpected"); got != "unexpected" {
		t.Fatalf("unexpected mode = %q", got)
	}
	if got := agentSpecMode("suggest"); got != "ISSUES_AND_PRS" {
		t.Fatalf("suggest = %q", got)
	}
}
