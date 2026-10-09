package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestTrustedBotAuthorSetDefaultsToDependabot(t *testing.T) {
	var a AutoMergeConfig
	got := a.TrustedBotAuthorSet()
	if len(got) != 1 || !got["dependabot[bot]"] {
		t.Fatalf("unset TrustedBotAuthors should default to dependabot only, got %v", got)
	}
}

func TestTrustedBotAuthorSetExplicitEmptyDisables(t *testing.T) {
	var a AutoMergeConfig
	if err := yaml.Unmarshal([]byte("trusted_bot_authors: []\n"), &a); err != nil {
		t.Fatal(err)
	}
	if a.TrustedBotAuthors == nil {
		t.Fatal("explicit empty list must decode as non-nil so it is distinguishable from unset")
	}
	if got := a.TrustedBotAuthorSet(); len(got) != 0 {
		t.Fatalf("explicit empty list should disable the lane, got %v", got)
	}
}

func TestTrustedBotAuthorSetNormalizes(t *testing.T) {
	var a AutoMergeConfig
	if err := yaml.Unmarshal([]byte("trusted_bot_authors:\n  - ' Renovate[bot] '\n  - ''\n  - 'dependabot[bot]'\n"), &a); err != nil {
		t.Fatal(err)
	}
	got := a.TrustedBotAuthorSet()
	if len(got) != 2 || !got["renovate[bot]"] || !got["dependabot[bot]"] {
		t.Fatalf("expected lower-cased, trimmed, blank-dropped set, got %v", got)
	}
}

func TestTrustedAuthorAutoMergeConfigDefaultsAndValidation(t *testing.T) {
	var cfg TrustedAuthorAutoMergeConfig
	if cfg.Enabled {
		t.Fatal("trusted-author automerge must default off")
	}
	if got := cfg.EffectiveRequireRole(); got != RoleMerger {
		t.Fatalf("default require role = %q, want %q", got, RoleMerger)
	}
	if !cfg.EffectiveRequireGitHubPermission() {
		t.Fatal("default require_github_permission must be true")
	}
	labels := cfg.ExcludeLabelSet()
	if !labels[DefaultSentinelLabel] {
		t.Fatalf("default exclude labels = %v, missing sentinel label %q", labels, DefaultSentinelLabel)
	}
	for _, want := range DefaultTrustedAuthorExcludeLabels {
		if !labels[want] {
			t.Fatalf("default exclude labels = %v, missing %q", labels, want)
		}
	}
	if err := (TrustedAuthorAutoMergeConfig{Enabled: true, RequireRole: RoleOwner}).Validate(); err != nil {
		t.Fatalf("owner require_role should validate: %v", err)
	}
	if err := (TrustedAuthorAutoMergeConfig{Enabled: true, RequireRole: RoleReadWrite}).Validate(); err == nil {
		t.Fatal("read-write require_role should fail validation")
	}
	disabled := false
	cfg = TrustedAuthorAutoMergeConfig{RequireGitHubPermission: &disabled}
	if cfg.EffectiveRequireGitHubPermission() {
		t.Fatal("explicit require_github_permission: false should be honored")
	}
}
