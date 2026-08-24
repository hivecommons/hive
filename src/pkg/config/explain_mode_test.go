package config

import (
	"strings"
	"testing"
)

func TestValidateExplainMode(t *testing.T) {
	valid := []string{"", ExplainModeOff, ExplainModeBrief, ExplainModeFull}
	for _, v := range valid {
		if !ValidateExplainMode(v) {
			t.Errorf("ValidateExplainMode(%q) = false, want true", v)
		}
	}

	// Rejected on purpose: near-misses an operator is likely to type. Silently
	// accepting one would leave explanation off with no error to explain why.
	invalid := []string{"verbose", "on", "true", "yes", "Full", "BRIEF", "debug", " full"}
	for _, v := range invalid {
		if ValidateExplainMode(v) {
			t.Errorf("ValidateExplainMode(%q) = true, want false", v)
		}
	}
}

func TestValidate_RejectsBadExplainMode(t *testing.T) {
	c := &Config{
		Project: ProjectConfig{Org: "my-org"},
		GitHub:  GitHubConfig{Token: "t"},
		Agents: map[string]AgentConfig{
			"scanner": {Backend: "claude", ExplainMode: "verbose"},
		},
	}
	err := c.validate()
	if err == nil {
		t.Fatal("validate() accepted an invalid explain_mode")
	}
	// The message must name the agent and the offending value, or an operator
	// with a 30-agent config cannot find the typo.
	for _, want := range []string{"scanner", "explain_mode", "verbose"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestValidate_AcceptsEveryExplainMode(t *testing.T) {
	for _, mode := range []string{"", ExplainModeOff, ExplainModeBrief, ExplainModeFull} {
		c := &Config{
			Project: ProjectConfig{Org: "my-org"},
			GitHub:  GitHubConfig{Token: "t"},
			Agents: map[string]AgentConfig{
				"scanner": {Backend: "claude", ExplainMode: mode},
			},
		}
		if err := c.validate(); err != nil {
			t.Errorf("validate() rejected explain_mode %q: %v", mode, err)
		}
	}
}

// The marker is a contract between the prompt (pkg/agent) and the log filter
// (pkg/dashboard). Changing it breaks explanation written by agents already
// running, so it is pinned literally rather than by reference.
func TestExplainLinePrefixIsStable(t *testing.T) {
	if ExplainLinePrefix != "EXPLAIN:" {
		t.Errorf("ExplainLinePrefix = %q; changing it orphans explanation already in agent logs", ExplainLinePrefix)
	}
}

// #4712: the hive-wide default is settable in governor config, not only through
// the deployment environment. These pin the precedence (config over env), the
// source label the dashboard shows so an operator can tell WHICH one is in
// force, and the normalization of a value nobody recognizes.
func TestResolveExplainModeDefault(t *testing.T) {
	tests := []struct {
		name       string
		configured string
		env        string
		wantMode   string
		wantSource string
	}{
		{
			name:       "unset everywhere is off with no source",
			wantMode:   ExplainModeOff,
			wantSource: "",
		},
		{
			name:       "governor config sets the default",
			configured: ExplainModeBrief,
			wantMode:   ExplainModeBrief,
			wantSource: ExplainModeSourceConfig,
		},
		{
			// The env var stays supported for hives that already set it.
			name:       "env var is the fallback",
			env:        ExplainModeFull,
			wantMode:   ExplainModeFull,
			wantSource: ExplainModeSourceEnv,
		},
		{
			// A hosted spoke owner can override what the deployment set —
			// the whole reason the setting moved into config.
			name:       "governor config wins over the env var",
			configured: ExplainModeFull,
			env:        ExplainModeBrief,
			wantMode:   ExplainModeFull,
			wantSource: ExplainModeSourceConfig,
		},
		{
			// Explicit "off" in config is a real choice, not "unset": it must
			// override an env var that turned explanation on fleet-wide.
			name:       "explicit off in config overrides the env var",
			configured: ExplainModeOff,
			env:        ExplainModeFull,
			wantMode:   ExplainModeOff,
			wantSource: ExplainModeSourceConfig,
		},
		{
			name:       "whitespace around a configured value is tolerated",
			configured: "  full  ",
			wantMode:   ExplainModeFull,
			wantSource: ExplainModeSourceConfig,
		},
		{
			name:       "unrecognized configured value degrades to off",
			configured: "verbose",
			wantMode:   ExplainModeOff,
			wantSource: ExplainModeSourceConfig,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(ExplainModeEnvVar, tt.env)
			g := GovernorConfig{ExplainMode: tt.configured}
			if got := g.ResolveExplainModeDefault(); got != tt.wantMode {
				t.Errorf("ResolveExplainModeDefault() = %q, want %q", got, tt.wantMode)
			}
			if got := g.ExplainModeDefaultSource(); got != tt.wantSource {
				t.Errorf("ExplainModeDefaultSource() = %q, want %q", got, tt.wantSource)
			}
		})
	}
}

func TestValidate_RejectsBadGovernorExplainMode(t *testing.T) {
	c := &Config{
		Project:  ProjectConfig{Org: "my-org"},
		GitHub:   GitHubConfig{Token: "t"},
		Governor: GovernorConfig{ExplainMode: "verbose"},
		Agents:   map[string]AgentConfig{"scanner": {Backend: "claude"}},
	}
	err := c.validate()
	if err == nil {
		t.Fatal("validate() accepted an invalid governor explain_mode")
	}
	// The message has to name the field and the bad value, or an operator
	// cannot find what to fix in the dashboard.
	msg := err.Error()
	for _, want := range []string{"governor", "explain_mode", "verbose"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}
}

func TestValidate_AcceptsEveryGovernorExplainMode(t *testing.T) {
	for _, mode := range []string{"", ExplainModeOff, ExplainModeBrief, ExplainModeFull} {
		c := &Config{
			Project:  ProjectConfig{Org: "my-org"},
			GitHub:   GitHubConfig{Token: "t"},
			Governor: GovernorConfig{ExplainMode: mode},
			Agents:   map[string]AgentConfig{"scanner": {Backend: "claude"}},
		}
		if err := c.validate(); err != nil {
			t.Errorf("validate() rejected governor explain_mode %q: %v", mode, err)
		}
	}
}
