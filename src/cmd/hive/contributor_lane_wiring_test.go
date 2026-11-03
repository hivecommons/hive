package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
	if !policy.Enabled || policy.HiveID != "hive-prod" || policy.Soak != time.Duration(config.DefaultContributorLaneSoakMinutes)*time.Minute || !policy.RequireReviewerVerdict || policy.AutoPromoteAfterMergedPR != config.DefaultContributorTierAutoPromoteAfterMerged {
		t.Fatalf("default policy = %+v", policy)
	}
	for _, label := range config.DefaultContributorLaneExcludeLabels {
		if !policy.ExcludeLabels[label] {
			t.Fatalf("exclude labels %v missing %q", policy.ExcludeLabels, label)
		}
	}
	if !policy.RequireTiers["trusted"] || !policy.RequireTiers["merger"] || len(policy.RequireTiers) != 2 {
		t.Fatalf("default require tiers = %v, want trusted+merger", policy.RequireTiers)
	}
	off := false
	cfg.AutoMerge.ContributorLane = config.ContributorLaneAutoMergeConfig{Enabled: &off, SoakMinutes: 15, RequireReviewerVerdict: &off, Repos: []string{"Widget"}, RequireTiers: []string{"Advisor"}}
	cfg.AutoMerge.ContributorTier = config.ContributorTierAutoMergeConfig{AutoPromoteAfterMerged: 9}
	policy = contributorLanePolicyFunc(cfg)()
	if policy.Enabled || policy.Soak != 15*time.Minute || policy.RequireReviewerVerdict || !policy.Repos["widget"] || policy.AutoPromoteAfterMergedPR != 9 {
		t.Fatalf("explicit policy = %+v", policy)
	}
	if !policy.RequireTiers["advisor"] || len(policy.RequireTiers) != 1 {
		t.Fatalf("explicit require tiers = %v, want advisor", policy.RequireTiers)
	}
}

func TestContributorLaneReviewerConfigured(t *testing.T) {
	if contributorLaneReviewerConfigured(nil) {
		t.Fatal("nil config has no reviewer")
	}
	cfg := &config.Config{AgentRuntimeConfig: config.AgentRuntimeConfig{
		Agents: map[string]config.AgentConfig{
			"coder":    {Enabled: true, Role: "implementer"},
			"disabled": {Enabled: false, Role: "reviewer"},
			"paused":   {Enabled: true, Paused: true, Role: "reviewer"},
		},
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
	cfg := &config.Config{AgentRuntimeConfig: config.AgentRuntimeConfig{Agents: map[string]config.AgentConfig{"critic": {Enabled: true, Role: "reviewer"}}}}
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

func TestContributorLaneVerdictFromArtifactNormalizesLegacyRequiresHuman(t *testing.T) {
	artifact := review.Artifact{Items: []review.Aggregate{{
		Repo:           "acme/widget",
		Number:         7,
		HeadSHA:        "sha7",
		Verdict:        review.VerdictRequiresHuman,
		RequiresHuman:  true,
		HumanSummaries: []string{"correctness requires human: field is plumbed but never populated; add the producer."},
		Perspectives:   map[review.Perspective]review.Verdict{review.PerspectiveCorrectness: review.VerdictRequiresHuman},
	}}}

	got := contributorLaneVerdictFromArtifact(artifact, "acme/widget", 7, "sha7", false)

	if got.Verdict != string(review.VerdictChangesRequested) || got.Approved || got.DecisionRequested {
		t.Fatalf("verdict = %+v, want normalized changes_requested", got)
	}
}

func TestContributorLaneVerdictFromArtifactPrefersNewestHeadAggregate(t *testing.T) {
	old := time.Date(2026, 10, 17, 1, 0, 0, 0, time.UTC)
	newer := old.Add(time.Hour)
	artifact := review.Artifact{Items: []review.Aggregate{
		{
			Repo:           "acme/widget",
			Number:         7,
			HeadSHA:        "sha7",
			BodyDigest:     "old-body",
			Verdict:        review.VerdictRequiresHuman,
			HumanSummaries: []string{"security requires human: old body"},
			RecordedAt:     old,
		},
		{
			Repo:           "acme/widget",
			Number:         7,
			HeadSHA:        "sha7",
			BodyDigest:     "new-body",
			Verdict:        review.VerdictRequiresHuman,
			HumanSummaries: []string{"security requires human: current body"},
			RecordedAt:     newer,
		},
	}}

	got := contributorLaneVerdictFromArtifact(artifact, "acme/widget", 7, "sha7", false)

	if got.Verdict != string(review.VerdictRequiresHuman) || len(got.HumanSummaries) != 1 || got.HumanSummaries[0] != "security requires human: current body" {
		t.Fatalf("verdict = %+v, want newest body aggregate", got)
	}
}

// hivecommons/hive#13205: the reviewer wrote head_sha "98489e7" and the
// aggregate was keyed by it, so the lane's full-head lookup found nothing
// and logged contributor-lane:no-verdict every cycle although the swarm had
// approved the current head.
func TestContributorLaneVerdictFromArtifactMatchesAbbreviatedHead(t *testing.T) {
	const full = "98489e7ba141fc60c30c9aaf47774dc42f9b1c76"
	artifact := review.Artifact{Items: []review.Aggregate{{
		Repo:          "hivecommons/hive",
		Number:        13205,
		HeadSHA:       "98489e7",
		Verdict:       review.VerdictApprove,
		MergeEligible: true,
	}}}

	got := contributorLaneVerdictFromArtifact(artifact, "hivecommons/hive", 13205, full, artifact.HasAggregateApproval("hivecommons/hive", 13205, full))

	if !got.Configured || !got.Approved || got.Verdict != string(review.VerdictApprove) {
		t.Fatalf("verdict = %+v, want approved verdict found via the abbreviated head", got)
	}
}

// A missing artifact is normal before the first review, but malformed data
// remains actionable. Both cases must keep the contributor gate closed.
func TestContributorLaneVerdictArtifactLogging(t *testing.T) {
	dir := t.TempDir()
	old, legacy := review.ReviewVerdictsPath, review.LegacyReviewVerdictsPath
	review.ReviewVerdictsPath = filepath.Join(dir, "verdicts.json")
	review.LegacyReviewVerdictsPath = filepath.Join(dir, "legacy.json")
	t.Cleanup(func() { review.ReviewVerdictsPath, review.LegacyReviewVerdictsPath = old, legacy })
	cfg := &config.Config{AgentRuntimeConfig: config.AgentRuntimeConfig{Agents: map[string]config.AgentConfig{"critic": {Enabled: true, Role: "reviewer"}}}}
	var logs bytes.Buffer
	gate := contributorLaneVerdictFunc(cfg, slog.New(slog.NewTextHandler(&logs, nil)))
	for i := 0; i < 3; i++ {
		if got := gate("acme/widget", 7, "sha7"); !got.Configured || got.Approved {
			t.Fatalf("missing artifact: %+v", got)
		}
	}
	if logs.Len() != 0 {
		t.Fatalf("missing artifact logged: %s", logs.String())
	}
	if err := os.WriteFile(review.ReviewVerdictsPath, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := gate("acme/widget", 7, "sha7"); !got.Configured || got.Approved {
		t.Fatalf("malformed artifact: %+v", got)
	}
	if !strings.Contains(logs.String(), "review-verdicts.json unavailable") {
		t.Fatalf("malformed artifact not logged: %s", logs.String())
	}
}
