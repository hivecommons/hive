package config

import (
	"strings"
	"testing"
)

func TestDashboardThemeValidate(t *testing.T) {
	cfg := &Config{
		Project: ProjectConfig{Org: "myorg"},
		GitHub:  GitHubConfig{Token: "tok"},
		Agents:  map[string]AgentConfig{"scanner": {Backend: "claude", Model: "sonnet"}},
	}
	cfg.Dashboard.Theme = "honeycomb"
	cfg.Dashboard.ThemeOverrides.Tokens = map[string]string{"--accent": "#e0a33a"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid dashboard theme rejected: %v", err)
	}
	cfg.Dashboard.ThemeOverrides.Tokens = map[string]string{"--accnet": "#e0a33a"}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "unsupported token") {
		t.Fatalf("invalid token error = %v, want unsupported token", err)
	}
}
