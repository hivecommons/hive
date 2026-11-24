package config

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// hivecommons/hive#19574: sandbox.setup parse, merge and bounds.

func TestSandboxSetupOffByDefault(t *testing.T) {
	var agent *AgentConfig
	got := agent.SandboxSetup(AgentSandboxConfig{})
	if got.Enabled() {
		t.Fatalf("setup must be off unless configured, got %+v", got)
	}
	if got.TimeoutS != DefaultSandboxSetupTimeoutS || got.TotalTimeoutS != DefaultSandboxSetupTotalTimeoutS {
		t.Fatalf("defaults = %+v", got)
	}
}

func TestSandboxSetupParsesFromYAML(t *testing.T) {
	src := `
setup:
  commands:
    - ./scripts/install.sh
  health_commands:
    - python -c 'import torch'
  timeout_s: 600
  total_timeout_s: 1200
`
	var sb AgentSandboxOverride
	if err := yaml.Unmarshal([]byte(src), &sb); err != nil {
		t.Fatal(err)
	}
	want := &SandboxSetupConfig{Commands: []string{"./scripts/install.sh"}, HealthCommands: []string{"python -c 'import torch'"}, TimeoutS: 600, TotalTimeoutS: 1200}
	if !reflect.DeepEqual(sb.Setup, want) {
		t.Fatalf("parsed = %+v, want %+v", sb.Setup, want)
	}
	var g AgentSandboxConfig
	if err := yaml.Unmarshal([]byte(src), &g); err != nil || !reflect.DeepEqual(g.Setup, want) {
		t.Fatalf("hive-wide parse = %+v, %v", g.Setup, err)
	}
}

func TestSandboxSetupMergePrecedence(t *testing.T) {
	global := AgentSandboxConfig{Setup: &SandboxSetupConfig{Commands: []string{"global-install"}, HealthCommands: []string{"global-health"}, TimeoutS: 30, TotalTimeoutS: 90}}

	var none *AgentConfig
	if got := none.SandboxSetup(global); !reflect.DeepEqual(got.Commands, []string{"global-install"}) || !reflect.DeepEqual(got.HealthCommands, []string{"global-health"}) || got.TimeoutS != 30 || got.TotalTimeoutS != 90 {
		t.Errorf("global not inherited: %+v", got)
	}
	inherit := &AgentConfig{Sandbox: &AgentSandboxOverride{}}
	if got := inherit.SandboxSetup(global); !reflect.DeepEqual(got.Commands, []string{"global-install"}) {
		t.Errorf("an agent without a setup block (null) must inherit: %+v", got)
	}
	own := &AgentConfig{Sandbox: &AgentSandboxOverride{Setup: &SandboxSetupConfig{Commands: []string{"own-install"}, TimeoutS: 5}}}
	got := own.SandboxSetup(global)
	if !reflect.DeepEqual(got.Commands, []string{"own-install"}) || len(got.HealthCommands) != 0 || got.TimeoutS != 5 {
		t.Errorf("the agent's block must win as a whole: %+v", got)
	}
	if got.TotalTimeoutS != 90 {
		t.Errorf("a timeout left at zero must fall back to the global block: %+v", got)
	}
	off := &AgentConfig{Sandbox: &AgentSandboxOverride{Setup: &SandboxSetupConfig{}}}
	if got := off.SandboxSetup(global); got.Enabled() {
		t.Errorf("an empty per-agent block must turn setup off: %+v", got)
	}
	// The result is a copy: changing it must not change the config.
	got.Commands[0] = "mutated"
	if own.Sandbox.Setup.Commands[0] != "own-install" {
		t.Error("SandboxSetup returned the config's own slice")
	}
}

func TestValidateSandboxSetupBounds(t *testing.T) {
	if err := ValidateSandboxSetup(nil); err != nil {
		t.Fatalf("nil block: %v", err)
	}
	ok := &SandboxSetupConfig{Commands: []string{"./install.sh"}, HealthCommands: []string{"true"}, TimeoutS: 1, TotalTimeoutS: 2}
	if err := ValidateSandboxSetup(ok); err != nil {
		t.Fatalf("valid block refused: %v", err)
	}
	cases := map[string]struct {
		in   SandboxSetupConfig
		want string
	}{
		"empty command":    {SandboxSetupConfig{Commands: []string{" "}}, "setup.commands[0] must not be empty"},
		"empty health":     {SandboxSetupConfig{HealthCommands: []string{"true", ""}}, "setup.health_commands[1] must not be empty"},
		"long command":     {SandboxSetupConfig{Commands: []string{strings.Repeat("x", MaxSandboxSetupCommandBytes+1)}}, "longer than"},
		"nul":              {SandboxSetupConfig{HealthCommands: []string{"a\x00b"}}, "NUL"},
		"too many":         {SandboxSetupConfig{Commands: make([]string, MaxSandboxSetupCommands+1)}, "at most"},
		"too many health":  {SandboxSetupConfig{HealthCommands: make([]string, MaxSandboxSetupCommands+1)}, "setup.health_commands takes at most"},
		"negative timeout": {SandboxSetupConfig{TimeoutS: -1}, "setup.timeout_s"},
		"negative total":   {SandboxSetupConfig{TotalTimeoutS: -1}, "setup.total_timeout_s"},
	}
	for name, c := range cases {
		in := c.in
		if err := ValidateSandboxSetup(&in); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
}
