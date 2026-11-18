package config

import (
	"strings"
	"testing"
	"time"
)

func evBool(b bool) *bool { return &b }

func TestResolveReviewPreflightDefaults(t *testing.T) {
	t.Setenv(ReviewPreflightOfflineEnv, "")
	r := ResolveReviewPreflight(ReviewPreflightConfig{}, ReviewPreflightConfig{})
	if !r.RuleDocs || r.Dependencies || !r.Vulnerabilities || !r.ReleaseAge || r.Offline {
		t.Fatalf("unexpected default switches: %+v", r)
	}
	if r.OSVURL != DefaultReviewPreflightOSVURL || r.MinReleaseAgeDays != 0 ||
		r.RuleDocBudgetBytes != DefaultReviewPreflightRuleDocBudgetBytes || r.CacheTTL != DefaultReviewPreflightCacheTTL ||
		r.RequestTimeout != DefaultReviewPreflightRequestTimeout || r.MaxDependencies != DefaultReviewPreflightMaxDependencies ||
		r.SeverityFloor != "low" || r.BlockSeverity != "high" {
		t.Fatalf("unexpected defaults: %+v", r)
	}
	if len(r.RuleDocOverride) != 0 {
		t.Fatalf("no explicit list by default: discovery follows the repo's own references, got %v", r.RuleDocOverride)
	}
}

func TestResolveReviewPreflightReleaseAgePolicyIsDeclaredNotDefaulted(t *testing.T) {
	if r := ResolveReviewPreflight(ReviewPreflightConfig{}, ReviewPreflightConfig{}); r.MinReleaseAgeDays != 0 {
		t.Fatalf("no release-age policy may be implied by default, got %d", r.MinReleaseAgeDays)
	}
	if r := ResolveReviewPreflight(ReviewPreflightConfig{}, ReviewPreflightConfig{MinReleaseAgeDays: 7}); r.MinReleaseAgeDays != 7 {
		t.Fatalf("a repo-declared policy must be honored, got %d", r.MinReleaseAgeDays)
	}
	if r := ResolveReviewPreflight(ReviewPreflightConfig{MinReleaseAgeDays: 3}, ReviewPreflightConfig{}); r.MinReleaseAgeDays != 3 {
		t.Fatalf("a hive-declared policy must be honored, got %d", r.MinReleaseAgeDays)
	}
	only := ResolveReviewPreflight(ReviewPreflightConfig{RuleDocPaths: []string{" docs/rules/** ", ""}}, ReviewPreflightConfig{})
	if strings.Join(only.RuleDocOverride, ",") != "docs/rules/**" {
		t.Fatalf("hive-wide list must apply when the repo sets none: %v", only.RuleDocOverride)
	}
}

func TestResolveReviewPreflightRepoOverridesHive(t *testing.T) {
	hive := ReviewPreflightConfig{
		Dependencies:      evBool(true),
		OSVURL:            "https://osv.mirror.example/",
		MinReleaseAgeDays: 30,
		CacheTTL:          "1h",
		RuleDocPaths:      []string{"docs/rules/**"},
		RuleDocExclude:    []string{"CODEOWNERS"},
		RegistryURLs:      map[string]string{"npm": "https://npm.mirror.example/", "go": "https://goproxy.example"},
		Rules:             []ReviewPreflightRule{{ID: "r1", Text: "hive rule", Severity: "high"}, {ID: "r2", Text: "other"}},
		Ignore:            []ReviewPreflightIgnore{{ID: "GHSA-1", Reason: "hive-wide"}},
	}
	repo := ReviewPreflightConfig{
		Dependencies:      evBool(false),
		MinReleaseAgeDays: -1,
		CacheTTL:          "30m",
		RuleDocPaths:      []string{"handbook/**"},
		RuleDocExclude:    []string{"STYLE*.md"},
		RegistryURLs:      map[string]string{"npm": "https://npm.repo.example"},
		Rules:             []ReviewPreflightRule{{ID: "r1", Text: "repo rule", Severity: "low"}, {ID: "r3", Text: "new"}},
		Ignore:            []ReviewPreflightIgnore{{ID: "ghsa-1", Reason: "repo reason"}, {ID: "GHSA-2", Package: "x", Reason: "r"}},
	}
	r := ResolveReviewPreflight(hive, repo)
	if r.Dependencies {
		t.Error("repo must override hive-wide dependencies=true")
	}
	if r.OSVURL != "https://osv.mirror.example" {
		t.Errorf("repo without osv_url must inherit hive's, got %q", r.OSVURL)
	}
	if r.MinReleaseAgeDays != 0 {
		t.Errorf("a negative repo min age clears the hive-wide policy, got %d", r.MinReleaseAgeDays)
	}
	if r.CacheTTL != 30*time.Minute {
		t.Errorf("cache ttl = %v", r.CacheTTL)
	}
	if got := strings.Join(r.RuleDocExclude, ","); got != "STYLE*.md" {
		t.Errorf("repo exclude must replace hive's, got %q", got)
	}
	if patterns := strings.Join(r.RuleDocOverride, ","); patterns != "handbook/**" {
		t.Errorf("a repo's rule_doc_paths must replace the hive-wide list: %s", patterns)
	}
	if r.RegistryURLs["npm"] != "https://npm.repo.example" || r.RegistryURLs["go"] != "https://goproxy.example" {
		t.Errorf("registry urls must merge per key: %v", r.RegistryURLs)
	}
	if len(r.Rules) != 3 || r.Rules[0].Text != "repo rule" || r.Rules[0].Severity != "low" || r.Rules[1].Severity != "medium" {
		t.Errorf("rules must merge by id with the repo winning and default severity medium: %+v", r.Rules)
	}
	if len(r.Ignore) != 2 || r.Ignore[0].Reason != "repo reason" {
		t.Errorf("ignores must merge by id and package, repo winning: %+v", r.Ignore)
	}
}

func TestResolveReviewPreflightOfflineEnv(t *testing.T) {
	t.Setenv(ReviewPreflightOfflineEnv, "true")
	if !ResolveReviewPreflight(ReviewPreflightConfig{Offline: evBool(false)}, ReviewPreflightConfig{}).Offline {
		t.Fatal("the env switch must win over the file")
	}
}

func TestResolveReviewPreflightBadValuesFallBack(t *testing.T) {
	r := ResolveReviewPreflight(ReviewPreflightConfig{CacheTTL: "soon", SeverityFloor: "nuclear", RuleDocBudgetBytes: 5}, ReviewPreflightConfig{})
	if r.CacheTTL != DefaultReviewPreflightCacheTTL || r.SeverityFloor != "low" || r.RuleDocBudgetBytes != DefaultReviewPreflightRuleDocBudgetBytes {
		t.Fatalf("bad values must fall back to defaults: %+v", r)
	}
}

func TestReviewPreflightForUsesRepoSettings(t *testing.T) {
	t.Setenv(ReviewPreflightOfflineEnv, "")
	cfg := &Config{
		Review: ReviewConfig{Preflight: ReviewPreflightConfig{Dependencies: evBool(true)}},
		Repos:  map[string]RepoSettings{"widgets": {ReviewPreflight: ReviewPreflightConfig{Dependencies: evBool(false)}}},
	}
	if cfg.ReviewPreflightFor("widgets").Dependencies {
		t.Error("widgets overrides dependencies off")
	}
	if !cfg.ReviewPreflightFor("gadgets").Dependencies {
		t.Error("a repo with no entry inherits the hive-wide value")
	}
	if (*Config)(nil).ReviewPreflightFor("x").Dependencies {
		t.Error("nil config means defaults")
	}
}

func TestReviewPreflightValidate(t *testing.T) {
	for name, bad := range map[string]ReviewPreflightConfig{
		"cache_ttl":         {CacheTTL: "forever"},
		"request_timeout":   {RequestTimeout: "-1s"},
		"severity_floor":    {SeverityFloor: "urgent"},
		"block_severity":    {BlockSeverity: "urgent"},
		"rule_doc_budget":   {RuleDocBudgetBytes: 10},
		"max_dependencies":  {MaxDependencies: -1},
		"rule needs text":   {Rules: []ReviewPreflightRule{{ID: "x"}}},
		"rule long text":    {Rules: []ReviewPreflightRule{{ID: "x", Text: strings.Repeat("a", MaxReviewPreflightRuleTextLen+1)}}},
		"rule severity":     {Rules: []ReviewPreflightRule{{ID: "x", Text: "t", Severity: "meh"}}},
		"ignore no reason":  {Ignore: []ReviewPreflightIgnore{{ID: "GHSA-1"}}},
		"ignore bad expiry": {Ignore: []ReviewPreflightIgnore{{ID: "GHSA-1", Reason: "r", Expires: "next year"}}},
	} {
		if err := bad.Validate("review.preflight"); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
	ok := ReviewPreflightConfig{CacheTTL: "2h", SeverityFloor: "medium", RuleDocBudgetBytes: 5000,
		Rules:  []ReviewPreflightRule{{ID: "x", Text: "t", Severity: "high"}},
		Ignore: []ReviewPreflightIgnore{{ID: "GHSA-1", Reason: "r", Expires: "2027-01-31"}}}
	if err := ok.Validate("review.preflight"); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	cfg := &Config{Repos: map[string]RepoSettings{"w": {ReviewPreflight: ReviewPreflightConfig{CacheTTL: "bad"}}}}
	if err := cfg.ValidateReviewPreflight(); err == nil || !strings.Contains(err.Error(), "repos.w.review_preflight") {
		t.Fatalf("a repo override must be validated and named: %v", err)
	}
}
