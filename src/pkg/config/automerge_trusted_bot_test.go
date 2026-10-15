package config

import (
	"strings"
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
	if labels[DefaultSentinelLabel] {
		t.Fatalf("default exclude labels = %v, sentinel label %q should not block trusted authors", labels, DefaultSentinelLabel)
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

func TestContributorLaneAutoMergeConfigDefaultsAndValidation(t *testing.T) {
	var cfg ContributorLaneAutoMergeConfig
	if !cfg.IsEnabled() {
		t.Fatal("contributor-lane automerge must default on")
	}
	if got := cfg.EffectiveSoakMinutes(); got != DefaultContributorLaneSoakMinutes {
		t.Fatalf("default soak = %d, want %d", got, DefaultContributorLaneSoakMinutes)
	}
	if !cfg.EffectiveRequireReviewerVerdict() {
		t.Fatal("default require_reviewer_verdict must be true")
	}
	labels := cfg.ExcludeLabelSet()
	for _, want := range DefaultContributorLaneExcludeLabels {
		if !labels[want] {
			t.Fatalf("default exclude labels = %v, missing %q", labels, want)
		}
	}
	if len(cfg.RepoSet()) != 0 {
		t.Fatalf("empty repos should yield empty set, got %v", cfg.RepoSet())
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("zero config should validate: %v", err)
	}
	if err := (ContributorLaneAutoMergeConfig{SoakMinutes: -1}).Validate(); err == nil {
		t.Fatal("negative soak_minutes should fail validation")
	}
	off := false
	cfg = ContributorLaneAutoMergeConfig{Enabled: &off, RequireReviewerVerdict: &off, SoakMinutes: 30, Repos: []string{" Widget ", ""}, ExcludeLabels: []string{"Hold", " "}}
	if cfg.IsEnabled() || cfg.EffectiveRequireReviewerVerdict() || cfg.EffectiveSoakMinutes() != 30 {
		t.Fatalf("explicit values not honored: %+v", cfg)
	}
	if repos := cfg.RepoSet(); !repos["widget"] || len(repos) != 1 {
		t.Fatalf("RepoSet = %v, want lower-cased trimmed widget", repos)
	}
	if ex := cfg.ExcludeLabelSet(); !ex["hold"] || len(ex) != 1 {
		t.Fatalf("ExcludeLabelSet = %v, want hold only", ex)
	}
}

// require_tiers defaults to the operator-granted trusted/merger tiers and
// rejects tiers a contributor reaches without an operator (#12041).
func TestContributorLaneAutoMergeConfigRequireTiers(t *testing.T) {
	var cfg ContributorLaneAutoMergeConfig
	if got := strings.Join(cfg.EffectiveRequireTiers(), ","); got != "merger,trusted" {
		t.Fatalf("default require_tiers = %q, want merger,trusted", got)
	}
	if set := cfg.RequireTierSet(); set["newcomer"] || set["contributor"] {
		t.Fatalf("default require_tiers must not include self-reached tiers: %v", set)
	}
	cfg.RequireTiers = []string{" Advisor ", "trusted"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("grantable tiers should validate: %v", err)
	}
	if set := cfg.RequireTierSet(); !set["advisor"] || !set["trusted"] || set["merger"] || len(set) != 2 {
		t.Fatalf("RequireTierSet = %v, want advisor+trusted", set)
	}
	for _, tier := range []string{"newcomer", "contributor", "revoked", "", "anyone"} {
		if err := (ContributorLaneAutoMergeConfig{RequireTiers: []string{"trusted", tier}}).Validate(); err == nil {
			t.Fatalf("require_tiers %q must fail validation", tier)
		}
	}
}

func TestBotTakeoverAutoMergeConfigDefaultsOnAndHonorsExplicitOff(t *testing.T) {
	var a AutoMergeConfig
	if !a.BotTakeover.IsEnabled() {
		t.Fatal("auto_merge.bot_takeover.enabled must default on")
	}
	if err := yaml.Unmarshal([]byte("bot_takeover:\n  enabled: false\n"), &a); err != nil {
		t.Fatal(err)
	}
	if a.BotTakeover.IsEnabled() {
		t.Fatal("explicit auto_merge.bot_takeover.enabled: false must disable the takeover")
	}
}
