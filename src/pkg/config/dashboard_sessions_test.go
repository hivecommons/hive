package config

import "testing"

func TestSessionsEnabledEnvOverridesConfig(t *testing.T) {
	c := &Config{}
	t.Setenv(EnvSessionsEnabled, "")
	if !c.SessionsEnabled() {
		t.Error("sessions must be on by default when dashboard.sessions.enabled is unset")
	}
	c.Dashboard.Sessions.Enabled = boolPtrSessions(false)
	if c.SessionsEnabled() {
		t.Error("explicit enabled: false ignored")
	}
	c.Dashboard.Sessions.Enabled = boolPtrSessions(true)
	if !c.SessionsEnabled() {
		t.Error("config flag ignored")
	}
	t.Setenv(EnvSessionsEnabled, "false")
	if c.SessionsEnabled() {
		t.Error("env false did not override config true")
	}
	c.Dashboard.Sessions.Enabled = boolPtrSessions(false)
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
	if err := (DashboardSessionsConfig{Enabled: boolPtrSessions(true)}).Validate(); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}
}

func TestSessionsWorkspaceRepoPrecedence(t *testing.T) {
	t.Setenv(EnvSessionsWorkspaceRepo, "")
	var nilCfg *Config
	if got := nilCfg.SessionsWorkspaceRepo(); got != "" {
		t.Errorf("nil config workspace repo = %q", got)
	}
	c := &Config{}
	if got := c.SessionsWorkspaceRepo(); got != "" {
		t.Errorf("empty config workspace repo = %q, want none", got)
	}
	c.Project.Org, c.Project.PrimaryRepo = "acme", "widgets"
	if got := c.SessionsWorkspaceRepo(); got != "acme/widgets" {
		t.Errorf("project repo not used: %q", got)
	}
	c.Dashboard.Sessions.WorkspaceRepo = "acme/other"
	if got := c.SessionsWorkspaceRepo(); got != "acme/other" {
		t.Errorf("config override ignored: %q", got)
	}
	t.Setenv(EnvSessionsWorkspaceRepo, "someone/else")
	if got := c.SessionsWorkspaceRepo(); got != "someone/else" {
		t.Errorf("env override ignored: %q", got)
	}
	// A malformed value never falls through to a different repository.
	t.Setenv(EnvSessionsWorkspaceRepo, "https://github.com/acme/widgets")
	if got := c.SessionsWorkspaceRepo(); got != "" {
		t.Errorf("malformed env fell through to %q", got)
	}
	t.Setenv(EnvSessionsWorkspaceRepo, "")
	c.Dashboard.Sessions.WorkspaceRepo = "acme"
	if got := c.SessionsWorkspaceRepo(); got != "" {
		t.Errorf("malformed config fell through to %q", got)
	}
	if err := c.Dashboard.Sessions.Validate(); err == nil {
		t.Error("workspace_repo without a slash validated")
	}
	c.Dashboard.Sessions.WorkspaceRepo = "acme/widgets"
	if err := c.Dashboard.Sessions.Validate(); err != nil {
		t.Errorf("valid workspace_repo rejected: %v", err)
	}
	c.Project.PrimaryRepo = ""
	c.Dashboard.Sessions.WorkspaceRepo = ""
	if got := c.SessionsWorkspaceRepo(); got != "" {
		t.Errorf("org without primary repo produced %q", got)
	}
}

func boolPtrSessions(v bool) *bool { return &v }

// The startup auto-answer is on unless hive.yaml or HIVE_SESSIONS_AUTOSTART
// turns it off; the env wins when it holds a recognised boolean.
func TestSessionsAutostartDefaultOnAndEnvOverrides(t *testing.T) {
	t.Setenv(EnvSessionsAutostart, "")
	c := &Config{}
	if !c.SessionsAutostart() {
		t.Error("autostart must be on when dashboard.sessions.autostart is unset")
	}
	var nilCfg *Config
	if !nilCfg.SessionsAutostart() {
		t.Error("nil config must keep the default (on)")
	}
	c.Dashboard.Sessions.Autostart = boolPtrSessions(false)
	if c.SessionsAutostart() || c.Dashboard.Sessions.AutostartOrDefault() {
		t.Error("explicit autostart: false ignored")
	}
	t.Setenv(EnvSessionsAutostart, "on")
	if !c.SessionsAutostart() {
		t.Error("env on did not override config false")
	}
	if c.Dashboard.Sessions.AutostartOrDefault() {
		t.Error("AutostartOrDefault must report the saved value, not the env")
	}
	c.Dashboard.Sessions.Autostart = boolPtrSessions(true)
	t.Setenv(EnvSessionsAutostart, "0")
	if c.SessionsAutostart() {
		t.Error("env off did not override config true")
	}
	t.Setenv(EnvSessionsAutostart, "garbage")
	if !c.SessionsAutostart() {
		t.Error("unrecognised env value must fall back to config")
	}
}
