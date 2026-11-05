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
			name:    "mixed-case agent name could never be claimed",
			agents:  map[string]AgentConfig{"Quality": {Backend: "claude", ContributorFirst: true}},
			wantErr: "lower-case",
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

// The dashboard's General-tab save calls ValidateContributorFirstAgent; a
// refusal there must read exactly like the load error, so the operator sees
// one message whichever way the key was set.
func TestValidateContributorFirstAgent_MatchesLoadError(t *testing.T) {
	cases := map[string]AgentConfig{
		"architect":  {Backend: "claude", ContributorFirst: true},
		"supervisor": {Backend: "claude", ContributorFirst: true},
		"Scanner":    {Backend: "claude", ContributorFirst: true},
		"quality":    {Backend: "claude", ContributorFirstGrace: -time.Minute},
	}
	for name, agent := range cases {
		t.Run(name, func(t *testing.T) {
			c := contributorFirstConfig(map[string]AgentConfig{name: agent})
			loadErr := c.validateContributorFirst()
			apiErr := ValidateContributorFirstAgent(name, agent, c.Hub)
			if loadErr == nil || apiErr == nil {
				t.Fatalf("want both to fail, got load=%v api=%v", loadErr, apiErr)
			}
			if loadErr.Error() != apiErr.Error() {
				t.Fatalf("messages differ:\nload: %s\napi:  %s", loadErr, apiErr)
			}
		})
	}
	if err := ValidateContributorFirstAgent("scanner", AgentConfig{ContributorFirst: true, ContributorFirstGrace: 0}, HubConfig{}); err != nil {
		t.Fatalf("default-delegatable scanner with zero grace: %v", err)
	}
}

func TestContributorFirstDelegatable(t *testing.T) {
	hub := HubConfig{ContributeDelegatableRoles: []string{"ci-maintainer", "supervisor"}}
	for name, want := range map[string]bool{
		"scanner":       true, // the defaults always apply; the list adds to them
		"ci-maintainer": true,
		"architect":     false,
		"supervisor":    false, // never delegatable, even when listed
		"Scanner":       false,
	} {
		if got := ContributorFirstDelegatable(name, hub); got != want {
			t.Errorf("ContributorFirstDelegatable(%q) = %v, want %v", name, got, want)
		}
	}
	if !ContributorFirstDelegatable("outreach", HubConfig{}) {
		t.Error("outreach is in the default delegatable list")
	}
}
