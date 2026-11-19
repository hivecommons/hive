package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestSandboxPreflightOffByDefault(t *testing.T) {
	var agent *AgentConfig
	got := agent.SandboxPreflight(AgentSandboxConfig{})
	if len(got.Commands) != 0 {
		t.Fatalf("preflight must be off unless configured, got %v", got.Commands)
	}
	if got.TimeoutS != DefaultSandboxPreflightTimeoutS || got.TotalTimeoutS != DefaultSandboxPreflightTotalTimeoutS || got.RetryDelayS != 0 {
		t.Fatalf("defaults = %+v", got)
	}
	if DefaultSandboxScheduleTimeoutS != 600 {
		t.Fatalf("default schedule timeout = %d, want 600", DefaultSandboxScheduleTimeoutS)
	}
}

func TestSandboxPreflightMergePrecedence(t *testing.T) {
	global := AgentSandboxConfig{Preflight: &SandboxPreflightConfig{Commands: []string{"global-check"}, TimeoutS: 30, TotalTimeoutS: 90, RetryDelayS: 4}}

	var none *AgentConfig
	if got := none.SandboxPreflight(global); !reflect.DeepEqual(got.Commands, []string{"global-check"}) || got.TimeoutS != 30 || got.TotalTimeoutS != 90 || got.RetryDelayS != 4 {
		t.Errorf("global not inherited: %+v", got)
	}
	inherit := &AgentConfig{Sandbox: &AgentSandboxOverride{}}
	if got := inherit.SandboxPreflight(global); !reflect.DeepEqual(got.Commands, []string{"global-check"}) {
		t.Errorf("an agent without a preflight block must inherit: %+v", got)
	}
	own := &AgentConfig{Sandbox: &AgentSandboxOverride{Preflight: &SandboxPreflightConfig{Commands: []string{"own-check"}, TimeoutS: 5}}}
	got := own.SandboxPreflight(global)
	if !reflect.DeepEqual(got.Commands, []string{"own-check"}) || got.TimeoutS != 5 {
		t.Errorf("the agent's block must win: %+v", got)
	}
	if got.TotalTimeoutS != 90 || got.RetryDelayS != 4 {
		t.Errorf("timeouts left at zero must fall back to the global block: %+v", got)
	}
	off := &AgentConfig{Sandbox: &AgentSandboxOverride{Preflight: &SandboxPreflightConfig{}}}
	if got := off.SandboxPreflight(global); len(got.Commands) != 0 {
		t.Errorf("an empty commands list must turn the preflight off for the agent: %+v", got)
	}
	got.Commands[0] = "mutated"
	if global.Preflight.Commands[0] != "global-check" || own.Sandbox.Preflight.Commands[0] != "own-check" {
		t.Error("the returned commands alias the config")
	}
}

func TestSandboxJobScheduleTimeoutFallsBackToGlobal(t *testing.T) {
	global := AgentSandboxConfig{Job: &SandboxJobConfig{WorkspaceClaim: "c", ScheduleTimeoutS: 900}}
	agent := &AgentConfig{Sandbox: &AgentSandboxOverride{Job: &SandboxJobConfig{}}}
	if got := agent.SandboxJob(global).ScheduleTimeoutS; got != 900 {
		t.Errorf("schedule timeout = %d, want the global 900", got)
	}
	agent.Sandbox.Job.ScheduleTimeoutS = 60
	if got := agent.SandboxJob(global).ScheduleTimeoutS; got != 60 {
		t.Errorf("schedule timeout = %d, want the agent's 60", got)
	}
}

func TestValidateSandboxPreflight(t *testing.T) {
	if err := ValidateSandboxPreflight(nil); err != nil {
		t.Fatalf("nil must be valid (off): %v", err)
	}
	if err := ValidateSandboxPreflight(&SandboxPreflightConfig{Commands: []string{`python -c 'import torch'`}, TimeoutS: 1, TotalTimeoutS: 2, RetryDelayS: 3}); err != nil {
		t.Fatalf("valid preflight refused: %v", err)
	}
	for _, tc := range []struct {
		name string
		p    SandboxPreflightConfig
		want string
	}{
		{"empty command", SandboxPreflightConfig{Commands: []string{"true", "   "}}, "commands[1] must not be empty"},
		{"nul byte", SandboxPreflightConfig{Commands: []string{"a\x00b"}}, "NUL"},
		{"too long", SandboxPreflightConfig{Commands: []string{strings.Repeat("x", MaxSandboxPreflightCommandBytes+1)}}, "longer than"},
		{"too many", SandboxPreflightConfig{Commands: make([]string, MaxSandboxPreflightCommands+1)}, "at most"},
		{"negative timeout", SandboxPreflightConfig{TimeoutS: -1}, "timeout_s must not be negative"},
		{"negative total", SandboxPreflightConfig{TotalTimeoutS: -1}, "total_timeout_s must not be negative"},
		{"negative delay", SandboxPreflightConfig{RetryDelayS: -1}, "retry_delay_s must not be negative"},
	} {
		if err := ValidateSandboxPreflight(&tc.p); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v, want it to contain %q", tc.name, err, tc.want)
		}
	}
	if err := ValidateSandboxJobScheduleTimeout(&SandboxJobConfig{ScheduleTimeoutS: -5}); err == nil {
		t.Error("a negative schedule_timeout_s must be refused")
	}
	if err := ValidateSandboxJobScheduleTimeout(nil); err != nil {
		t.Errorf("nil job: %v", err)
	}
}
