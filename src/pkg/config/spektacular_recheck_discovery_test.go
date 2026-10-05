package config

import (
	"strings"
	"testing"
	"time"
)

func TestSpektacularRecheckDiscoveryDefaults(t *testing.T) {
	var cfg SpektacularConfig
	if got := cfg.DiscoveryTimeout(); got != 30*time.Second {
		t.Fatalf("DiscoveryTimeout = %s, want 30s", got)
	}
	if got := cfg.DiscoveryMaxTotalItems(); got != 100 {
		t.Fatalf("DiscoveryMaxTotalItems = %d, want 100", got)
	}
	if got := (SpektacularRecheckDiscoverySource{}).EffectiveMaxItems(); got != 20 {
		t.Fatalf("EffectiveMaxItems = %d, want 20", got)
	}
}

func TestValidateSpektacularRecheckDiscoveryRejectsBadKind(t *testing.T) {
	cfg := Config{
		Project: ProjectConfig{Org: "org"},
		GitHub:  GitHubConfig{Token: "x"},
		Agents:  map[string]AgentConfig{"scanner": {Backend: "claude"}},
	}
	cfg.Runs.Spektacular.Recheck.Discovery = SpektacularRecheckDiscoveryConfig{
		Enabled: true,
		Sources: []SpektacularRecheckDiscoverySource{{Kind: "search_engine", Name: "bad", URLOrRepo: "https://example.com/feed.xml"}},
	}
	err := cfg.validate()
	if err == nil || !strings.Contains(err.Error(), "invalid kind") {
		t.Fatalf("validate() = %v, want invalid kind", err)
	}
}

func TestValidateSpektacularRecheckDiscoveryRejectsDisallowedHost(t *testing.T) {
	cfg := Config{
		Project: ProjectConfig{Org: "org"},
		GitHub:  GitHubConfig{Token: "x"},
		Agents:  map[string]AgentConfig{"scanner": {Backend: "claude"}},
	}
	cfg.Variables.Security.HTTPAllowlist = []string{"allowed.example.com"}
	cfg.Runs.Spektacular.Recheck.Discovery = SpektacularRecheckDiscoveryConfig{
		Enabled: true,
		Sources: []SpektacularRecheckDiscoverySource{{Kind: "standards_feed", Name: "ietf", URLOrRepo: "https://blocked.example.com/feed.xml"}},
	}
	err := cfg.validate()
	if err == nil || !strings.Contains(err.Error(), "blocked.example.com") || !strings.Contains(err.Error(), "http_allowlist") {
		t.Fatalf("validate() = %v, want host allowlist error", err)
	}
}

func TestValidateSpektacularRecheckDiscoveryAcceptsAllowlistedSources(t *testing.T) {
	cfg := Config{
		Project: ProjectConfig{Org: "org"},
		GitHub:  GitHubConfig{Token: "x"},
		Agents:  map[string]AgentConfig{"scanner": {Backend: "claude"}},
	}
	cfg.Variables.Security.HTTPAllowlist = []string{"api.github.com", "standards.example.com"}
	cfg.Runs.Spektacular.Recheck.Discovery = SpektacularRecheckDiscoveryConfig{
		Enabled: true,
		Sources: []SpektacularRecheckDiscoverySource{
			{Kind: "upstream_release", Name: "up", URLOrRepo: "owner/repo"},
			{Kind: "repo_activity", Name: "activity", URLOrRepo: "owner/other"},
			{Kind: "standards_feed", Name: "feed", URLOrRepo: "https://standards.example.com/feed.xml"},
			{Kind: "landscape", Name: "landscape"},
		},
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate() = %v, want nil", err)
	}
}
