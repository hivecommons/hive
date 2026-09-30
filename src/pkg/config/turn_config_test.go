package config

import (
	"testing"
	"time"
)

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

func TestPRFollowUpResumeEnabledDefaultOff(t *testing.T) {
	t.Setenv(PRFollowUpResumeEnvVar, "")
	var nilCfg *Config
	if nilCfg.PRFollowUpResumeEnabled() {
		t.Fatal("nil config must not enable PR follow-up resume")
	}
	if (&Config{}).PRFollowUpResumeEnabled() {
		t.Fatal("zero config must not enable PR follow-up resume")
	}
	cfg := &Config{Turn: TurnConfig{PRFollowUp: PRFollowUpConfig{Enabled: true}}}
	if !cfg.PRFollowUpResumeEnabled() {
		t.Fatal("turn.pr_follow_up.enabled should enable")
	}
	t.Setenv(PRFollowUpResumeEnvVar, "false")
	if cfg.PRFollowUpResumeEnabled() {
		t.Fatal("env rollback must override config")
	}
	t.Setenv(PRFollowUpResumeEnvVar, "true")
	if !(&Config{}).PRFollowUpResumeEnabled() {
		t.Fatal("env must be able to enable for one process")
	}
}

func TestPRFollowUpMaxAge(t *testing.T) {
	t.Setenv(PRFollowUpMaxAgeEnvVar, "")
	var nilCfg *Config
	if got := nilCfg.PRFollowUpMaxAge(); got != DefaultPRFollowUpMaxAge {
		t.Fatalf("nil config max age = %v, want default", got)
	}
	for _, raw := range []string{"", "garbage", "-1h", "0s"} {
		cfg := &Config{Turn: TurnConfig{PRFollowUp: PRFollowUpConfig{MaxAge: raw}}}
		if got := cfg.PRFollowUpMaxAge(); got != DefaultPRFollowUpMaxAge {
			t.Fatalf("max_age %q = %v, want default %v", raw, got, DefaultPRFollowUpMaxAge)
		}
	}
	cfg := &Config{Turn: TurnConfig{PRFollowUp: PRFollowUpConfig{MaxAge: "6h"}}}
	if got := cfg.PRFollowUpMaxAge(); got != 6*time.Hour {
		t.Fatalf("configured max age = %v, want 6h", got)
	}
	t.Setenv(PRFollowUpMaxAgeEnvVar, "90m")
	if got := cfg.PRFollowUpMaxAge(); got != 90*time.Minute {
		t.Fatalf("env max age = %v, want 90m", got)
	}
}

func TestPRFollowUpRetention(t *testing.T) {
	t.Setenv(PRFollowUpRetentionEnvVar, "")
	var nilCfg *Config
	if got := nilCfg.PRFollowUpRetention(); got != DefaultPRFollowUpRetention {
		t.Fatalf("nil config retention = %v, want default %v", got, DefaultPRFollowUpRetention)
	}
	for _, raw := range []string{"", "soon", "-1h", "0s"} {
		cfg := &Config{Turn: TurnConfig{PRFollowUp: PRFollowUpConfig{Retention: raw}}}
		if got := cfg.PRFollowUpRetention(); got != DefaultPRFollowUpRetention {
			t.Fatalf("retention %q = %v, want default %v", raw, got, DefaultPRFollowUpRetention)
		}
	}
	cfg := &Config{Turn: TurnConfig{PRFollowUp: PRFollowUpConfig{Retention: "72h"}}}
	if got := cfg.PRFollowUpRetention(); got != 72*time.Hour {
		t.Fatalf("configured retention = %v, want 72h", got)
	}
	t.Setenv(PRFollowUpRetentionEnvVar, "48h")
	if got := cfg.PRFollowUpRetention(); got != 48*time.Hour {
		t.Fatalf("env retention = %v, want the env override 48h", got)
	}
}

func TestPRFollowUpResumeIDMaxAge(t *testing.T) {
	t.Setenv(PRFollowUpResumeIDMaxAgeEnvVar, "")
	var nilCfg *Config
	if got := nilCfg.PRFollowUpResumeIDMaxAge(); got != DefaultPRFollowUpResumeIDMaxAge {
		t.Fatalf("nil config resume id max age = %v, want default %v", got, DefaultPRFollowUpResumeIDMaxAge)
	}
	for _, raw := range []string{"", "whenever", "-1h", "0s"} {
		cfg := &Config{Turn: TurnConfig{PRFollowUp: PRFollowUpConfig{ResumeIDMaxAge: raw}}}
		if got := cfg.PRFollowUpResumeIDMaxAge(); got != DefaultPRFollowUpResumeIDMaxAge {
			t.Fatalf("resume_id_max_age %q = %v, want default %v", raw, got, DefaultPRFollowUpResumeIDMaxAge)
		}
	}
	cfg := &Config{Turn: TurnConfig{PRFollowUp: PRFollowUpConfig{ResumeIDMaxAge: "12h"}}}
	if got := cfg.PRFollowUpResumeIDMaxAge(); got != 12*time.Hour {
		t.Fatalf("configured resume id max age = %v, want 12h", got)
	}
	t.Setenv(PRFollowUpResumeIDMaxAgeEnvVar, "30m")
	if got := cfg.PRFollowUpResumeIDMaxAge(); got != 30*time.Minute {
		t.Fatalf("env resume id max age = %v, want the env override 30m", got)
	}
}
