package config

import (
	"strings"
	"testing"
)

func TestGitHubActionsOIDCValidateRequiresAudienceWhenEnabled(t *testing.T) {
	if err := (GitHubActionsConfig{OIDC: GitHubActionsOIDCConfig{Enabled: true}}).Validate(); err == nil || !strings.Contains(err.Error(), "audience") {
		t.Fatalf("enabled OIDC without audience err=%v, want audience error", err)
	}
	if err := (GitHubActionsConfig{OIDC: GitHubActionsOIDCConfig{Enabled: true, Audience: "hive"}}).Validate(); err != nil {
		t.Fatalf("enabled OIDC with audience: %v", err)
	}
}

func TestGitHubActionsOIDCDefaults(t *testing.T) {
	var cfg GitHubActionsOIDCConfig
	if cfg.JWKSURLEffective() != DefaultGitHubActionsOIDCJWKSURL {
		t.Fatalf("jwks default = %q", cfg.JWKSURLEffective())
	}
	if cfg.MaxSkewEffective() != DefaultGitHubActionsOIDCMaxSkew {
		t.Fatalf("max skew default = %s", cfg.MaxSkewEffective())
	}
}
