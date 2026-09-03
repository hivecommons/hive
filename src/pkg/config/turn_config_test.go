package config

import "testing"

func TestReentrantTurnEnabledDefaultOff(t *testing.T) {
	t.Setenv(ReentrantTurnEnvVar, "")
	t.Setenv(ReentrantTurnBackgroundFleetEnvVar, "")
	yes := true
	var nilCfg *Config
	if nilCfg.ReentrantTurnEnabled(AgentConfig{ReentrantTurn: &yes}) {
		t.Fatal("nil config must not enable re-entrant turns")
	}
	if (&Config{}).ReentrantTurnEnabled(AgentConfig{ReentrantTurn: &yes}) {
		t.Fatal("zero config must not enable re-entrant turns")
	}
}

func TestReentrantTurnEnabledRequiresGlobalAndAgentOptIn(t *testing.T) {
	t.Setenv(ReentrantTurnEnvVar, "")
	t.Setenv(ReentrantTurnBackgroundFleetEnvVar, "")
	yes := true
	no := false
	cfg := &Config{Turn: TurnConfig{Reentrant: ReentrantTurnConfig{Enabled: true}}}
	if !cfg.ReentrantTurnEnabled(AgentConfig{ReentrantTurn: &yes}) {
		t.Fatal("global gate plus per-agent opt-in should enable")
	}
	if cfg.ReentrantTurnEnabled(AgentConfig{ReentrantTurn: &no}) {
		t.Fatal("per-agent opt-out should win")
	}
	if cfg.ReentrantTurnEnabled(AgentConfig{}) {
		t.Fatal("global gate alone should not enroll an agent")
	}
}

func TestReentrantTurnBackgroundFleetGate(t *testing.T) {
	t.Setenv(ReentrantTurnEnvVar, "")
	t.Setenv(ReentrantTurnBackgroundFleetEnvVar, "")
	cfg := &Config{Turn: TurnConfig{Reentrant: ReentrantTurnConfig{
		Enabled:                true,
		BackgroundFleetEnabled: true,
	}}}
	if !cfg.ReentrantTurnEnabled(AgentConfig{}) {
		t.Fatal("background fleet gate should enroll unspecified agents")
	}
}

func TestReentrantTurnEnvOverrides(t *testing.T) {
	cfg := &Config{Turn: TurnConfig{Reentrant: ReentrantTurnConfig{
		Enabled:                false,
		BackgroundFleetEnabled: false,
	}}}
	t.Setenv(ReentrantTurnEnvVar, "true")
	t.Setenv(ReentrantTurnBackgroundFleetEnvVar, "true")
	if !cfg.ReentrantTurnEnabled(AgentConfig{}) {
		t.Fatal("env gates should enable background fleet")
	}
	t.Setenv(ReentrantTurnEnvVar, "false")
	if cfg.ReentrantTurnEnabled(AgentConfig{}) {
		t.Fatal("global env false should disable rollout")
	}
}
