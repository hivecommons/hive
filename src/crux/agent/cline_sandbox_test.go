package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/sandbox"
)

func TestClineSandboxJobCommand(t *testing.T) {
	spec := SandboxKickSpec{Runtime: config.SandboxRuntimeJob, Timeout: time.Minute,
		AgentConfig: configSnapshot{Backend: "cline", Model: "openrouter/anthropic/test", ReasoningEffort: "high"}}
	cmd, err := sandboxKickCommand(spec, ".hive/kick-prompt.txt")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(cmd, " ")
	for _, want := range []string{"crux cline-turn --provider-secret", "--model openrouter/anthropic/test",
		"--thinking high", "--timeout 1m0s", "--prompt-file .hive/kick-prompt.txt"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %s: %s", want, joined)
		}
	}
	spec.AgentConfig.ReasoningEffort = ""
	spec.Timeout = 0
	cmd, err = sandboxKickCommand(spec, "prompt")
	if err != nil || strings.Contains(strings.Join(cmd, " "), "--thinking") || strings.Contains(strings.Join(cmd, " "), "--timeout") {
		t.Fatalf("default controls: %v, %v", cmd, err)
	}
	for _, cfg := range []configSnapshot{
		{Backend: "cline", Model: "auto"},
		{Backend: "cline", Model: "openrouter/test", ReasoningEffort: "bogus"},
		{Backend: "cline", Model: "openrouter/test", LaunchCmd: "custom"},
	} {
		spec.AgentConfig = cfg
		if _, err := sandboxKickCommand(spec, "prompt"); err == nil {
			t.Errorf("invalid Cline command accepted: %+v", cfg)
		}
	}
	spec.AgentConfig = configSnapshot{Backend: "cline", Model: "openrouter/test"}
	for _, runtime := range []string{"", config.SandboxRuntimePodman} {
		spec.Runtime = runtime
		cmd, err := sandboxKickCommand(spec, ".hive/kick-prompt.txt")
		if err != nil || !strings.Contains(strings.Join(cmd, " "), "--provider-file /run/hive-cline-provider/providers.json") {
			t.Fatalf("Podman did not select scoped file mode: %v", err)
		}
	}
	spec.AgentConfig = configSnapshot{Backend: "copilot"}
	if _, err := sandboxKickCommand(spec, "prompt"); err != nil {
		t.Fatal("existing sandbox backend changed", err)
	}
}

func TestClineSandboxJobPolicyAndCredentialPreflight(t *testing.T) {
	t.Setenv("HIVE_CLINE_CONFIG_DIR", "")
	agent := &AgentProcess{Config: config.AgentConfig{Model: "openrouter/test"}}
	job := config.SandboxJobConfig{EnvFromSecrets: []string{"dedicated-cline-provider"}}
	if err := validateClineSandbox(agent, config.SandboxRuntimeJob, job, ""); err != nil {
		t.Fatal(err)
	}
	if err := validateClineSandbox(agent, config.SandboxRuntimePodman, job, ""); err == nil || !strings.Contains(err.Error(), "Podman") {
		t.Fatal("Podman accepted provider credentials", err)
	}
	if err := validateClineSandbox(agent, config.SandboxRuntimeJob, config.SandboxJobConfig{EnvFromSecrets: []string{" "}}, ""); err == nil ||
		!strings.Contains(err.Error(), "env_from_secrets") {
		t.Fatal("missing explicit Secret reference accepted", err)
	}
	agent.Config.Tools = &config.ToolsConfig{Preset: "read-only"}
	if err := validateClineSandbox(agent, config.SandboxRuntimeJob, job, ""); err == nil {
		t.Fatal("Job dropped tool restrictions")
	}
	agent.Config.Tools = nil
	agent.Config.Connections = []config.ConnectionConfig{{Type: "mcp", URI: "https://must-not-use.invalid"}}
	if err := validateClineSandbox(agent, config.SandboxRuntimeJob, job, ""); err == nil {
		t.Fatal("Job dropped connections policy")
	}
}

func TestClineManagerSandboxJobRouting(t *testing.T) {
	yes := true
	cfg := config.AgentConfig{Backend: "cline", Model: "openrouter/test", ReasoningEffort: "medium",
		Sandbox: &config.AgentSandboxOverride{Enabled: &yes, Runtime: config.SandboxRuntimeJob}}
	m := NewManager(map[string]config.AgentConfig{"cline-worker": cfg}, quietTestLogger(),
		ProjectContext{Org: "example", Repos: []string{"repo"}})
	m.SetSandboxConfig(config.AgentSandboxConfig{Enabled: true, Image: "cline-image", WorkspaceDir: t.TempDir(),
		Job: &config.SandboxJobConfig{WorkspaceClaim: "hive-data", EnvFromSecrets: []string{"dedicated-cline-provider"}}})
	m.setSandboxRunnerForTest(&sandboxFakeRunner{})
	captured := make(chan sandbox.LaunchSpec, 1)
	m.setSandboxJobLauncherFactoryForTest(func(job config.SandboxJobConfig) sandbox.Launcher {
		if len(job.EnvFromSecrets) != 1 || job.EnvFromSecrets[0] != "dedicated-cline-provider" {
			t.Errorf("Secret reference not carried: %+v", job.EnvFromSecrets)
		}
		return clineCapturingLauncher{captured: captured}
	})
	if err := m.Start(context.Background(), "cline-worker"); err != nil {
		t.Fatal(err)
	}
	if err := m.SendKick("cline-worker", "bounded task"); err != nil {
		t.Fatal(err)
	}
	select {
	case spec := <-captured:
		command := strings.Join(spec.Command, " ")
		if !strings.Contains(command, "cline-turn --provider-secret") || !strings.Contains(command, "--thinking medium") {
			t.Fatal("Cline did not use the bounded Job runner", command)
		}
		if len(spec.EnvAllowlist) != 0 || spec.Env["HIVE_CLINE_PROVIDERS_B64"] != "" {
			t.Fatal("controller injected credentials instead of using Job Secret references")
		}
	case <-time.After(sandboxStateTransitionTimeout):
		t.Fatal("Job launcher did not receive the Cline kick")
	}
	waitIdle(t, m, "cline-worker")
}

func TestClineSandboxRefusalPreservesIdleState(t *testing.T) {
	t.Setenv("HIVE_CLINE_CONFIG_DIR", "")
	tests := []struct {
		name    string
		runtime string
		secrets []string
		mutate  func(*config.AgentConfig)
		want    string
	}{
		{name: "Podman", runtime: config.SandboxRuntimePodman, want: "Podman"},
		{name: "missing Secret", runtime: config.SandboxRuntimeJob, want: "env_from_secrets"},
		{name: "tools", runtime: config.SandboxRuntimeJob, secrets: []string{"provider"},
			mutate: func(c *config.AgentConfig) { c.Tools = &config.ToolsConfig{Preset: "read-only"} }, want: "policy"},
		{name: "launch override", runtime: config.SandboxRuntimeJob, secrets: []string{"provider"},
			mutate: func(c *config.AgentConfig) { c.LaunchCmd = "custom" }, want: "launch_cmd"},
		{name: "implicit model", runtime: config.SandboxRuntimeJob, secrets: []string{"provider"},
			mutate: func(c *config.AgentConfig) { c.Model = "auto" }, want: "provider/model"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			yes := true
			cfg := config.AgentConfig{Backend: "cline", Model: "openrouter/test",
				Sandbox: &config.AgentSandboxOverride{Enabled: &yes, Runtime: tt.runtime}}
			if tt.mutate != nil {
				tt.mutate(&cfg)
			}
			m := NewManager(map[string]config.AgentConfig{"cline-worker": cfg}, quietTestLogger(),
				ProjectContext{Org: "example", Repos: []string{"repo"}})
			m.SetSandboxConfig(config.AgentSandboxConfig{Enabled: true, Image: "cline-image", WorkspaceDir: t.TempDir(),
				Job: &config.SandboxJobConfig{WorkspaceClaim: "hive-data", EnvFromSecrets: tt.secrets}})
			if err := m.Start(context.Background(), "cline-worker"); err != nil {
				t.Fatal(err)
			}
			if err := m.SendKick("cline-worker", "bounded task"); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("want refusal %q, got %v", tt.want, err)
			}
			if got := m.AllStatuses()["cline-worker"].State; got != StateIdle {
				t.Fatalf("refused kick changed state to %s", got)
			}
		})
	}
}

type clineCapturingLauncher struct{ captured chan<- sandbox.LaunchSpec }

func (l clineCapturingLauncher) Run(_ context.Context, spec sandbox.LaunchSpec) (sandbox.Result, error) {
	l.captured <- spec
	return sandbox.Result{}, nil
}
