package config

import "testing"

func TestSessionsEnabledEnvOverridesConfig(t *testing.T) {
	c := &Config{}
	t.Setenv(EnvSessionsEnabled, "")
	if c.SessionsEnabled() {
		t.Error("sessions enabled by default")
	}
	c.Dashboard.Sessions.Enabled = true
	if !c.SessionsEnabled() {
		t.Error("config flag ignored")
	}
	t.Setenv(EnvSessionsEnabled, "false")
	if c.SessionsEnabled() {
		t.Error("env false did not override config true")
	}
	c.Dashboard.Sessions.Enabled = false
	t.Setenv(EnvSessionsEnabled, "1")
	if !c.SessionsEnabled() {
		t.Error("env true did not override config false")
	}
	t.Setenv(EnvSessionsEnabled, "garbage")
	if c.SessionsEnabled() {
		t.Error("unrecognised env value must fall back to config")
	}
	var nilCfg *Config
	t.Setenv(EnvSessionsEnabled, "")
	if nilCfg.SessionsEnabled() {
		t.Error("nil config enabled sessions")
	}
}

func TestSessionsLimitsPrecedence(t *testing.T) {
	for _, e := range []string{EnvSessionsMax, EnvSessionsMaxPerUser, EnvSessionsIdleTTLHours} {
		t.Setenv(e, "")
	}
	c := &Config{}
	if c.SessionsMax() != DefaultSessionsMax || c.SessionsMaxPerUser() != DefaultSessionsMaxPerUser || c.SessionsIdleTTLHours() != DefaultSessionsIdleTTLHours {
		t.Errorf("defaults = %d/%d/%d", c.SessionsMax(), c.SessionsMaxPerUser(), c.SessionsIdleTTLHours())
	}
	c.Dashboard.Sessions = DashboardSessionsConfig{MaxSessions: 5, MaxPerUser: 2, IdleTTLHours: 6}
	if c.SessionsMax() != 5 || c.SessionsMaxPerUser() != 2 || c.SessionsIdleTTLHours() != 6 {
		t.Errorf("config values ignored: %d/%d/%d", c.SessionsMax(), c.SessionsMaxPerUser(), c.SessionsIdleTTLHours())
	}
	t.Setenv(EnvSessionsMax, "9")
	t.Setenv(EnvSessionsMaxPerUser, "-3")
	if c.SessionsMax() != 9 {
		t.Errorf("env did not win: %d", c.SessionsMax())
	}
	if c.SessionsMaxPerUser() != 2 {
		t.Errorf("invalid env must fall through to config: %d", c.SessionsMaxPerUser())
	}
}

func TestDashboardSessionsValidateRejectsNegative(t *testing.T) {
	if err := (DashboardSessionsConfig{MaxSessions: -1}).Validate(); err == nil {
		t.Error("negative max_sessions accepted")
	}
	if err := (DashboardSessionsConfig{IdleTTLHours: -1}).Validate(); err == nil {
		t.Error("negative idle_ttl_hours accepted")
	}
	if err := (DashboardSessionsConfig{Enabled: true}).Validate(); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}
}
