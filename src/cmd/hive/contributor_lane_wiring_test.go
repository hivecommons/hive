package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/review"
)

func TestContributorLanePolicyFuncReadsConfig(t *testing.T) {
	if contributorLanePolicyFunc(nil)().Enabled {
		t.Fatal("nil config must disable the tier")
	}
	cfg := &config.Config{HiveID: "hive-prod"}
	policy := contributorLanePolicyFunc(cfg)()
	if !policy.Enabled || policy.HiveID != "hive-prod" || policy.Soak != time.Duration(config.DefaultContributorLaneSoakMinutes)*time.Minute || !policy.RequireReviewerVerdict {
		t.Fatalf("default policy = %+v", policy)
	}
	for _, label := range config.DefaultContributorLaneExcludeLabels {
		if !policy.ExcludeLabels[label] {
			t.Fatalf("exclude labels %v missing %q", policy.ExcludeLabels, label)
		}
	}
	off := false
	cfg.AutoMerge.ContributorLane = config.ContributorLaneAutoMergeConfig{Enabled: &off, SoakMinutes: 15, RequireReviewerVerdict: &off, Repos: []string{"Widget"}}
	policy = contributorLanePolicyFunc(cfg)()
	if policy.Enabled || policy.Soak != 15*time.Minute || policy.RequireReviewerVerdict || !policy.Repos["widget"] {
		t.Fatalf("explicit policy = %+v", policy)
	}
}

func TestContributorLaneReviewerConfigured(t *testing.T) {
	if contributorLaneReviewerConfigured(nil) {
		t.Fatal("nil config has no reviewer")
	}
	cfg := &config.Config{Agents: map[string]config.AgentConfig{
		"coder":    {Enabled: true, Role: "implementer"},
		"disabled": {Enabled: false, Role: "reviewer"},
		"paused":   {Enabled: true, Paused: true, Role: "reviewer"},
	}}
	if contributorLaneReviewerConfigured(cfg) {
		t.Fatal("disabled/paused reviewers must not count")
	}
	cfg.Agents["critic"] = config.AgentConfig{Enabled: true, Role: "reviewer"}
	if !contributorLaneReviewerConfigured(cfg) {
		t.Fatal("enabled reviewer role should count")
	}
	cfg.Review.ReviewerAgents = []string{"coder"}
	if !contributorLaneReviewerConfigured(cfg) {
		t.Fatal("explicit reviewer_agents membership should count")
	}
	cfg.Review.ReviewerAgents = []string{"nobody"}
	if contributorLaneReviewerConfigured(cfg) {
		t.Fatal("explicit reviewer_agents list excludes every agent")
	}
}

func TestContributorLaneVerdictFunc(t *testing.T) {
	orig := review.ReviewVerdictsPath
	review.ReviewVerdictsPath = filepath.Join(t.TempDir(), "verdicts", review.ReviewVerdictsFile)
	defer func() { review.ReviewVerdictsPath = orig }()

	noReviewer := &config.Config{}
	if v := contributorLaneVerdictFunc(noReviewer, nil)("acme/widget", 7, "sha7"); v.Configured {
		t.Fatalf("no reviewer configured should be unconfigured: %+v", v)
	}
	cfg := &config.Config{Agents: map[string]config.AgentConfig{"critic": {Enabled: true, Role: "reviewer"}}}
	if v := contributorLaneVerdictFunc(cfg, nil)("acme/widget", 7, "sha7"); !v.Configured || v.Approved {
		t.Fatalf("missing artifact must fail closed: %+v", v)
	}
	artifact := review.Artifact{Items: []review.Aggregate{
		{Repo: "acme/widget", Number: 7, HeadSHA: "sha7", Verdict: review.VerdictApprove, MergeEligible: true},
		{Repo: "acme/widget", Number: 8, HeadSHA: "sha8", Verdict: review.VerdictChangesRequested},
	}}
	if err := review.WriteArtifact(review.ReviewVerdictsPath, artifact); err != nil {
		t.Fatal(err)
	}
	if v := contributorLaneVerdictFunc(cfg, nil)("acme/widget", 7, "sha7"); !v.Configured || !v.Approved || v.Verdict != string(review.VerdictApprove) {
		t.Fatalf("approved head = %+v", v)
	}
	if v := contributorLaneVerdictFunc(cfg, nil)("acme/widget", 7, "sha9"); v.Approved || v.Verdict != "" {
		t.Fatalf("stale head must not be approved: %+v", v)
	}
	if v := contributorLaneVerdictFunc(cfg, nil)("acme/widget", 8, "sha8"); v.Approved || v.Verdict != string(review.VerdictChangesRequested) {
		t.Fatalf("request-changes = %+v", v)
	}
}
