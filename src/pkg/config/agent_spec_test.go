package config

import (
	"strings"
	"testing"
)

func TestValidateAgentSpecRef(t *testing.T) {
	base := func(agentSpec string) *Config {
		return &Config{
			Project: ProjectConfig{Org: "my-org"},
			GitHub:  GitHubConfig{Token: "t"},
			Agents: map[string]AgentConfig{
				"supervisor": {Backend: "copilot", AgentSpec: agentSpec},
			},
		}
	}
	if err := base("agents/reviewer/spec.yaml").validate(); err != nil {
		t.Fatalf("validate() with agent_spec = %v, want nil", err)
	}
	err := base("bad\x00spec").validate()
	if err == nil {
		t.Fatal("validate() with NUL agent_spec = nil, want error")
	}
	if !strings.Contains(err.Error(), "supervisor") || !strings.Contains(err.Error(), "agent_spec") {
		t.Errorf("error = %q, want agent name and field", err.Error())
	}
}
