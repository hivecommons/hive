package config

import (
	"strings"
	"testing"
	"time"
)

func TestDependencyBotRebaseAfter(t *testing.T) {
	var nilCfg *Config
	if got := nilCfg.DependencyBotRebaseAfter(); got != 4*time.Hour {
		t.Errorf("nil config: got %v, want 4h", got)
	}
	if got := (&Config{}).DependencyBotRebaseAfter(); got != 4*time.Hour {
		t.Errorf("unset: got %v, want 4h", got)
	}
	c := &Config{Governor: GovernorConfig{DependencyBotRebaseAfterS: 2 * 60 * 60}}
	if got := c.DependencyBotRebaseAfter(); got != 2*time.Hour {
		t.Errorf("set to 7200s: got %v, want 2h", got)
	}
}

func TestValidate_DependencyBotRebaseAfterS(t *testing.T) {
	build := func(s int) *Config {
		return &Config{
			Project:  ProjectConfig{Org: "my-org"},
			GitHub:   GitHubConfig{Token: "t"},
			Governor: GovernorConfig{DependencyBotRebaseAfterS: s},
			AgentRuntimeConfig: AgentRuntimeConfig{
				Agents: map[string]AgentConfig{"scanner": {Backend: "claude"}},
			},
		}
	}
	for _, s := range []int{0, 3600} {
		if err := build(s).validate(); err != nil {
			t.Errorf("validate() rejected dependency_bot_rebase_after_s=%d: %v", s, err)
		}
	}
	err := build(-1).validate()
	if err == nil || !strings.Contains(err.Error(), "dependency_bot_rebase_after_s") {
		t.Fatalf("validate() with dependency_bot_rebase_after_s=-1: got %v, want an error naming the key", err)
	}
}
