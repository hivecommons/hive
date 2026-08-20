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
