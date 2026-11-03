package config

import (
	"strings"
	"testing"
	"time"
)

func contributorFirstConfig(agents map[string]AgentConfig, delegatable ...string) *Config {
	c := &Config{
		Project: ProjectConfig{Org: "my-org"},
		GitHub:  GitHubConfig{Token: "t"},
		AgentRuntimeConfig: AgentRuntimeConfig{
			Agents: agents,
		},
	}
	c.Hub.ContributeDelegatableRoles = delegatable
	return c
}

func TestValidateContributorFirst(t *testing.T) {
	cases := []struct {
		name        string
		agents      map[string]AgentConfig
		delegatable []string
		wantErr     string
	}{
		{
			name:   "default-delegatable role is accepted",
			agents: map[string]AgentConfig{"quality": {Backend: "claude", ContributorFirst: true}},
		},
		{
			name:   "flag off needs no delegation",
			agents: map[string]AgentConfig{"architect": {Backend: "claude"}},
		},
		{
			name:    "privileged role not in the delegatable list fails the load",
			agents:  map[string]AgentConfig{"architect": {Backend: "claude", ContributorFirst: true}},
			wantErr: "contribute_delegatable_roles",
		},
		{
			name:        "privileged role listed as delegatable is accepted",
			agents:      map[string]AgentConfig{"architect": {Backend: "claude", ContributorFirst: true}},
			delegatable: []string{"architect"},
		},
		{
			name:    "supervisor is never delegatable",
			agents:  map[string]AgentConfig{"supervisor": {Backend: "claude", ContributorFirst: true}},
			wantErr: "supervisor",
		},
		{
			name:    "negative grace fails even with the flag off",
			agents:  map[string]AgentConfig{"quality": {Backend: "claude", ContributorFirstGrace: -time.Minute}},
			wantErr: "contributor_first_grace",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := contributorFirstConfig(tc.agents, tc.delegatable...)
			err := c.validateContributorFirst()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateContributorFirst() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validateContributorFirst() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

// The validator is wired into the full validation pass: a bad flag fails
// Validate, not just the helper.
func TestValidate_RejectsUndelegatableContributorFirst(t *testing.T) {
	c := contributorFirstConfig(map[string]AgentConfig{
		"scanner":   {Backend: "claude"},
		"architect": {Backend: "claude", ContributorFirst: true},
	})
	err := c.validate()
	if err == nil || !strings.Contains(err.Error(), "contributor_first") || !strings.Contains(err.Error(), "architect") {
		t.Fatalf("validate() = %v, want contributor_first error naming architect", err)
	}
	c.Hub.ContributeDelegatableRoles = []string{"architect"}
	if err := c.validate(); err != nil {
		t.Fatalf("a delegatable architect should validate: %v", err)
	}
}

func TestContributorFirstGraceOrDefault(t *testing.T) {
	if got := (AgentConfig{}).ContributorFirstGraceOrDefault(); got != DefaultContributorFirstGrace {
		t.Errorf("unset grace = %v, want default %v", got, DefaultContributorFirstGrace)
	}
	if got := (AgentConfig{ContributorFirstGrace: 3 * time.Minute}).ContributorFirstGraceOrDefault(); got != 3*time.Minute {
		t.Errorf("set grace = %v, want 3m", got)
	}
	if got := (AgentConfig{ContributorFirstGrace: -time.Minute}).ContributorFirstGraceOrDefault(); got != DefaultContributorFirstGrace {
		t.Errorf("negative grace (never loaded, hand-built) = %v, want default %v", got, DefaultContributorFirstGrace)
	}
}
