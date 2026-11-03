package config

import (
	"strings"
	"testing"
)

// The zero value must keep every existing hive on GitHub: an unset key and
// an explicit "github" resolve identically, and only "work_source" moves.
func TestACMMIssueTrackerDefault(t *testing.T) {
	if got := (ACMMConfig{}).EffectiveIssueTracker(); got != ACMMIssueTrackerGitHub {
		t.Fatalf("unset issue_tracker = %q, want %q", got, ACMMIssueTrackerGitHub)
	}
	if got := (ACMMConfig{IssueTracker: "github"}).EffectiveIssueTracker(); got != ACMMIssueTrackerGitHub {
		t.Fatalf("github = %q", got)
	}
	if got := (ACMMConfig{IssueTracker: " work_source "}).EffectiveIssueTracker(); got != ACMMIssueTrackerWorkSource {
		t.Fatalf("work_source = %q", got)
	}
}

func TestResolveACMMIssueTracker(t *testing.T) {
	cfg := ACMMConfig{IssueTracker: ACMMIssueTrackerWorkSource}
	if got, err := cfg.ResolveACMMIssueTracker(""); err != nil || got != ACMMIssueTrackerWorkSource {
		t.Fatalf("empty override: %q, %v", got, err)
	}
	if got, err := cfg.ResolveACMMIssueTracker("github"); err != nil || got != ACMMIssueTrackerGitHub {
		t.Fatalf("github override should win over config: %q, %v", got, err)
	}
	if got, err := (ACMMConfig{}).ResolveACMMIssueTracker("work_source"); err != nil || got != ACMMIssueTrackerWorkSource {
		t.Fatalf("work_source override should win over default: %q, %v", got, err)
	}
	if _, err := cfg.ResolveACMMIssueTracker("linear"); err == nil || !strings.Contains(err.Error(), "linear") {
		t.Fatalf("unknown override must error naming the value, got %v", err)
	}
}

func TestValidate_RejectsBadACMMIssueTracker(t *testing.T) {
	c := &Config{
		Project: ProjectConfig{Org: "my-org"},
		GitHub:  GitHubConfig{Token: "t"},
		AgentRuntimeConfig: AgentRuntimeConfig{
			Agents: map[string]AgentConfig{
				"scanner": {Backend: "claude"},
			},
		},
	}
	c.Governor.ACMM.IssueTracker = "jira"
	err := c.validate()
	if err == nil || !strings.Contains(err.Error(), "acmm.issue_tracker") || !strings.Contains(err.Error(), "jira") {
		t.Fatalf("validate() = %v, want acmm.issue_tracker error naming jira", err)
	}
	c.Governor.ACMM.IssueTracker = "work_source"
	if err := c.validate(); err != nil {
		t.Fatalf("work_source should validate: %v", err)
	}
}
